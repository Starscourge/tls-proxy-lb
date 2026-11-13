package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// certDir writes certs/a.test.{crt,key} and certs/default.{crt,key} (for
// *.test) into a new directory and returns it.
func certDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	certs := filepath.Join(dir, "certs")
	if err := os.Mkdir(certs, 0o700); err != nil {
		t.Fatal(err)
	}
	ca := tlstest.NewCA(t)
	ca.WriteFiles(t, certs, "a.test", tlstest.LeafOptions{}, "a.test")
	ca.WriteFiles(t, certs, "default", tlstest.LeafOptions{}, "*.test")
	return dir
}

// mustSnapshot parses src as a file in a directory holding the certificates
// fullConfig names, and builds its snapshot.
func mustSnapshot(t *testing.T, src string) *Snapshot {
	t.Helper()
	src = strings.ReplaceAll(src, "/etc/proxy/default.key", "certs/default.key")
	c, err := Parse(filepath.Join(certDir(t), "test.toml"), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSnapshot(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNormaliseHost(t *testing.T) {
	for in, want := range map[string]string{
		"a.test":         "a.test",
		"A.Test":         "a.test",
		"a.test:8080":    "a.test",
		"A.TEST.:443":    "a.test",
		"a.test.":        "a.test",
		"10.0.0.1:80":    "10.0.0.1",
		"[::1]:8080":     "::1",
		"[::1]":          "::1",
		"::1":            "::1",
		"[2001:DB8::1]":  "2001:db8::1",
		"":               "",
		"localhost:8080": "localhost",
	} {
		if got := NormaliseHost(in); got != want {
			t.Errorf("NormaliseHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnapshotRoute(t *testing.T) {
	s := mustSnapshot(t, fullConfig)
	a, b := s.Pool("pool_a"), s.Pool("pool_b")
	if a == nil || b == nil || s.Pool("nope") != nil {
		t.Fatal("pools not built")
	}
	for host, want := range map[string]*lb.Pool{
		"a.test":      a,
		"A.TEST:8080": a,
		"a.test.":     a,
		"[::1]:8080":  b,
		"b.test":      b, // default pool
		"":            b,
	} {
		if got := s.Route(host); got != want {
			t.Errorf("Route(%q) = %v, want %v", host, poolName(got), poolName(want))
		}
	}
	if len(a.Backends) != 2 || a.Backends[1].Addr != "10.0.0.12:8000" {
		t.Fatalf("pool_a backends %+v", a.Backends)
	}
	if _, ok := a.Balancer.(*lb.LeastConnBalancer); !ok {
		t.Fatalf("pool_a balancer %T", a.Balancer)
	}
	if _, ok := b.Balancer.(*lb.RingBalancer); !ok {
		t.Fatalf("pool_b balancer %T", b.Balancer)
	}
	if s.Timeouts.HeaderRead.Milliseconds() != 2500 || s.Limits.MaxConns != 500 || len(s.Listeners) != 3 {
		t.Fatalf("settings not copied: %+v %+v %v", s.Timeouts, s.Limits, s.Listeners)
	}
	if s.Certs.Len() != 1 || !s.Certs.Has("a.test") || !s.Certs.HasDefault() {
		t.Fatalf("certificates not loaded: %d host certs, default %v", s.Certs.Len(), s.Certs.HasDefault())
	}
	if got := s.Certs.Lookup("A.TEST").Leaf.DNSNames; !reflect.DeepEqual(got, []string{"a.test"}) {
		t.Fatalf("a.test served %v", got)
	}
	if got := s.Certs.Lookup("other.test").Leaf.DNSNames; !reflect.DeepEqual(got, []string{"*.test"}) {
		t.Fatalf("other.test served %v", got)
	}
}

// TestSnapshotCertErrors: every certificate that fails to load is reported
// with its field, and a certificate for the wrong host is one of them.
func TestSnapshotCertErrors(t *testing.T) {
	dir := certDir(t)
	src := base + pool + `
[[route]]
host = "b.test"
pool = "p"
cert = "certs/a.test.crt"
key  = "certs/a.test.key"

[[route]]
host = "a.test"
pool = "p"
cert = "certs/missing.crt"
key  = "certs/a.test.key"

[tls]
default_cert = "certs/default.crt"
default_key  = "certs/a.test.key"
`
	c, err := Parse(filepath.Join(dir, "bad.toml"), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSnapshot(c, nil)
	if err == nil {
		t.Fatal("bad certificates accepted")
	}
	var fields []string
	for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
		var fe *FieldError
		if !errors.As(e, &fe) {
			t.Fatalf("%v is not a *FieldError", e)
		}
		fields = append(fields, fe.Field)
	}
	if want := []string{"route[0].cert", "route[1].cert", "tls.default_cert"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields %q, want %q\n%v", fields, want, err)
	}
	for _, want := range []string{
		`bad.toml: route[0].cert: certificate is not valid for "b.test" (names [a.test])`,
		"route[1].cert: open " + filepath.Join(dir, "certs/missing.crt"),
		"tls.default_cert: tls: private key does not match public key",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error\n%v\nwant it to contain %q", err, want)
		}
	}
}

func TestSnapshotNoDefaultIsNil(t *testing.T) {
	s := mustSnapshot(t, minimal)
	if p := s.Route("other.test"); p != nil {
		t.Fatalf("unknown host routed to %s", p.Name)
	}
	if p := s.Route("a.test"); p == nil || p.Name != "p" {
		t.Fatalf("a.test routed to %v", poolName(p))
	}
}

func TestRouteZeroAllocs(t *testing.T) {
	s := mustSnapshot(t, fullConfig)
	if n := testing.AllocsPerRun(1000, func() { s.Route("a.test:8080") }); n != 0 {
		t.Fatalf("Route allocates %v times per call", n)
	}
}

func TestNewSnapshotRejectsDanglingPool(t *testing.T) {
	c, err := Parse("test.toml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	c.Routes[0].Pool = "gone"
	if _, err := NewSnapshot(c, nil); err == nil {
		t.Fatal("route to an unknown pool accepted")
	}
	c.Routes[0].Pool = "p"
	c.DefaultPool = "gone"
	if _, err := NewSnapshot(c, nil); err == nil {
		t.Fatal("unknown default pool accepted")
	}
	c.DefaultPool = ""
	c.Timeouts.Idle = 0
	if _, err := NewSnapshot(c, nil); err == nil {
		t.Fatal("zero timeout accepted")
	}
}

// TestStoreSwap: readers racing a writer always see a whole snapshot,
// either the old one or the new one (run with -race).
func TestStoreSwap(t *testing.T) {
	s1 := mustSnapshot(t, minimal)
	s2 := mustSnapshot(t, fullConfig)
	st := NewStore(s1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				s := st.Load()
				if s != s1 && s != s2 {
					t.Error("Load returned an unknown snapshot")
					return
				}
				_ = s.Route("a.test")
			}
		})
	}
	wg.Go(func() {
		for i := range 1000 {
			if i%2 == 0 {
				st.Store(s2)
			} else {
				st.Store(s1)
			}
		}
	})
	wg.Wait()
}

func poolName(p *lb.Pool) string {
	if p == nil {
		return "<nil>"
	}
	return p.Name
}

// Backends with the same address share one connection pool from the
// registry, whichever lb pool they are in. Without a registry nothing is
// reused.
func TestNewSnapshotConnectionPools(t *testing.T) {
	src := base + route + `
[pool.p]
backends = [{ address = "127.0.0.1:8000" }, { address = "127.0.0.1:8001" }]
[pool.q]
backends = [{ address = "127.0.0.1:8000" }]
`
	c, err := Parse("pools.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	reg := upstream.NewRegistry(c.Upstream.Options())
	defer reg.Close()
	s, err := NewSnapshot(c, reg)
	if err != nil {
		t.Fatal(err)
	}
	p, q := s.Pool("p").Backends, s.Pool("q").Backends
	if p[0] == q[0] {
		t.Fatal("backends shared across pools; only connections should be")
	}
	if p[0].Conns != q[0].Conns || p[0].Conns != reg.Pool("127.0.0.1:8000") || p[1].Conns == p[0].Conns {
		t.Fatal("connection pools not shared by address")
	}
	if !p[0].Conns.Reuse() {
		t.Fatal("registry pool does not reuse with the default settings")
	}
	s, err = NewSnapshot(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := s.Pool("p").Backends[0]; b.Conns == nil || b.Conns.Reuse() {
		t.Fatal("without a registry a backend must dial per request")
	}
}

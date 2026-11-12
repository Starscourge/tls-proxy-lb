package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestLoadPair(t *testing.T) {
	ca := tlstest.NewCA(t)
	dir := t.TempDir()
	certA, keyA := ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	_, keyB := ca.WriteFiles(t, dir, "b", tlstest.LeafOptions{}, "b.test")
	wild, wildKey := ca.WriteFiles(t, dir, "wild", tlstest.LeafOptions{}, "*.test")

	if c, err := LoadPair(certA, keyA, "a.test"); err != nil || c.Leaf == nil {
		t.Fatalf("LoadPair(a) = %v, %v", c, err)
	}
	if _, err := LoadPair(certA, keyA, ""); err != nil {
		t.Fatalf("LoadPair without host: %v", err)
	}
	if _, err := LoadPair(wild, wildKey, "c.test"); err != nil {
		t.Fatalf("wildcard for c.test: %v", err)
	}
	for name, tc := range map[string]struct {
		cert, key, host, want string
	}{
		"wrong host":    {certA, keyA, "b.test", `not valid for "b.test" (names [a.test])`},
		"wrong key":     {certA, keyB, "a.test", "private key does not match"},
		"missing cert":  {filepath.Join(dir, "nope.crt"), keyA, "a.test", "no such file"},
		"key for cert":  {certA, certA, "a.test", "certificate rather than a key"},
		"wildcard deep": {wild, wildKey, "x.y.test", "not valid"},
	} {
		if _, err := LoadPair(tc.cert, tc.key, tc.host); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestLookup(t *testing.T) {
	ca := tlstest.NewCA(t)
	a, def := ca.Pair(t, "a.test"), ca.Pair(t, "default.test")
	s := NewCertSet(map[string]*tls.Certificate{"a.test": &a}, &def)
	for name, want := range map[string]*tls.Certificate{
		"a.test": &a, "A.Test": &a, "a.test.": &a, "b.test": &def, "": &def,
	} {
		if got := s.Lookup(name); got != want {
			t.Errorf("Lookup(%q) = %p, want %p", name, got, want)
		}
	}
	if !s.Has("A.TEST") || s.Has("b.test") || !s.HasDefault() || s.Len() != 1 {
		t.Error("Has, HasDefault or Len wrong")
	}
	noDef := NewCertSet(map[string]*tls.Certificate{"a.test": &a}, nil)
	if noDef.Lookup("b.test") != nil || noDef.Lookup("") != nil {
		t.Error("Lookup without a default returned a certificate")
	}
	if n := testing.AllocsPerRun(100, func() { s.Lookup("a.test") }); n != 0 {
		t.Errorf("Lookup allocates %v times", n)
	}
}

func TestWarnings(t *testing.T) {
	ca := tlstest.NewCA(t)
	now := time.Now()
	issue := func(nb, na time.Time) *tls.Certificate {
		c, k := ca.Issue(t, tlstest.LeafOptions{NotBefore: nb, NotAfter: na}, "x.test")
		p, err := tls.X509KeyPair(c, k)
		if err != nil {
			t.Fatal(err)
		}
		return &p
	}
	s := NewCertSet(map[string]*tls.Certificate{
		"expired.test": issue(now.Add(-48*time.Hour), now.Add(-time.Hour)),
		"future.test":  issue(now.Add(time.Hour), now.Add(48*time.Hour)),
		"soon.test":    issue(now.Add(-time.Hour), now.Add(10*24*time.Hour)),
		"fine.test":    issue(now.Add(-time.Hour), now.Add(90*24*time.Hour)),
	}, issue(now.Add(-48*time.Hour), now.Add(-time.Hour)))
	got := s.Warnings(now)
	want := []string{"default: certificate expired", "expired.test: certificate expired", "future.test: certificate not valid until", "soon.test: certificate expires"}
	if len(got) != len(want) {
		t.Fatalf("warnings %q, want %d", got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("warning %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

// tlsServer runs one TLS listener with cfg. Each connection is handshaken,
// sent "ok" and closed. Handshake errors are reported on errs.
type tlsServer struct {
	addr string
	errs chan error
}

func startTLS(t *testing.T, cfg *tls.Config) *tlsServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &tlsServer{addr: ln.Addr().String(), errs: make(chan error, 100)}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				tc := tls.Server(c, cfg)
				if err := tc.Handshake(); err != nil {
					s.errs <- err
					return
				}
				io.WriteString(tc, "ok")
				tc.Close()
			})
		}
	})
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return s
}

// dialTLS handshakes with cfg, reads the server's "ok" (which also takes
// in any TLS 1.3 session ticket) and returns the connection state.
func (s *tlsServer) dial(cfg *tls.Config) (tls.ConnectionState, error) {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", s.addr, cfg)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "ok" {
		return tls.ConnectionState{}, errors.Join(errors.New("reading ok: "+string(b)), err)
	}
	return c.ConnectionState(), nil
}

func fixture(t *testing.T, withDefault bool) (*tlstest.CA, *tlsServer) {
	t.Helper()
	ca := tlstest.NewCA(t)
	a, b, def := ca.Pair(t, "a.test"), ca.Pair(t, "b.test"), ca.Pair(t, "*.test")
	var d *tls.Certificate
	if withDefault {
		d = &def
	}
	set := NewCertSet(map[string]*tls.Certificate{"a.test": &a, "b.test": &b}, d)
	return ca, startTLS(t, ServerConfig(func() *CertSet { return set }))
}

func TestServerConfigSNI(t *testing.T) {
	ca, s := fixture(t, false)
	for _, host := range []string{"a.test", "b.test"} {
		st, err := s.dial(ca.ClientConfig(host))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if leaf := st.PeerCertificates[0]; leaf.DNSNames[0] != host {
			t.Errorf("SNI %s got certificate for %v", host, leaf.DNSNames)
		}
		if st.NegotiatedProtocol != ALPN && st.NegotiatedProtocol != "" {
			t.Errorf("ALPN %q", st.NegotiatedProtocol)
		}
	}

	// No default: an unknown name and a missing name both fail with
	// unrecognized_name.
	for _, sni := range []string{"c.test", ""} {
		cfg := ca.ClientConfig(sni)
		cfg.InsecureSkipVerify = true // no SNI means nothing to verify against
		// The client sees the alert as "remote error: tls: <alert>".
		_, err := s.dial(cfg)
		if err == nil || !strings.Contains(err.Error(), "remote error: tls: unrecognized name") {
			t.Errorf("SNI %q: err %v, want alert unrecognized_name (112)", sni, err)
		}
		if serr := <-s.errs; serr == nil {
			t.Errorf("SNI %q: server handshake succeeded", sni)
		}
	}
}

func TestServerConfigDefaultCert(t *testing.T) {
	ca, s := fixture(t, true)
	st, err := s.dial(ca.ClientConfig("c.test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.PeerCertificates[0].DNSNames[0]; got != "*.test" {
		t.Errorf("unknown SNI got %q, want the default *.test", got)
	}
	cfg := ca.ClientConfig("")
	cfg.InsecureSkipVerify = true
	if st, err = s.dial(cfg); err != nil || st.PeerCertificates[0].DNSNames[0] != "*.test" {
		t.Errorf("no SNI: %v", err)
	}
}

func TestServerConfigVersionsAndSuites(t *testing.T) {
	ca, s := fixture(t, false)
	for _, tc := range []struct {
		name    string
		min     uint16
		max     uint16
		suites  []uint16
		wantErr bool
	}{
		{"TLS 1.0", tls.VersionTLS10, tls.VersionTLS10, nil, true},
		{"TLS 1.1", tls.VersionTLS11, tls.VersionTLS11, nil, true},
		{"TLS 1.2", tls.VersionTLS12, tls.VersionTLS12, nil, false},
		{"TLS 1.3", tls.VersionTLS13, tls.VersionTLS13, nil, false},
		{"TLS 1.2 CBC only", tls.VersionTLS12, tls.VersionTLS12, []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA}, true},
		{"TLS 1.2 RSA key exchange only", tls.VersionTLS12, tls.VersionTLS12, []uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256}, true},
	} {
		cfg := ca.ClientConfig("a.test")
		cfg.MinVersion, cfg.MaxVersion, cfg.CipherSuites = tc.min, tc.max, tc.suites
		st, err := s.dial(cfg)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: handshake succeeded with %s", tc.name, tls.CipherSuiteName(st.CipherSuite))
			} else {
				<-s.errs
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if st.Version != tc.max {
			t.Errorf("%s: negotiated %s", tc.name, tls.VersionName(st.Version))
		}
		if tc.max == tls.VersionTLS12 && st.CipherSuite != tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 &&
			st.CipherSuite != tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384 &&
			st.CipherSuite != tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256 {
			t.Errorf("%s: suite %s", tc.name, tls.CipherSuiteName(st.CipherSuite))
		}
	}
}

func TestServerConfigALPN(t *testing.T) {
	ca, s := fixture(t, false)
	cfg := ca.ClientConfig("a.test")
	cfg.NextProtos = []string{"h2", "http/1.1"}
	if st, err := s.dial(cfg); err != nil || st.NegotiatedProtocol != "http/1.1" {
		t.Errorf("h2,http/1.1: %v, negotiated %q", err, st.NegotiatedProtocol)
	}
	cfg.NextProtos = []string{"h2"}
	if _, err := s.dial(cfg); err == nil {
		t.Error("h2 only: handshake succeeded")
	} else {
		<-s.errs
	}
}

// TestSessionResumption: a second connection with the same client session
// cache resumes, for TLS 1.2 and TLS 1.3.
func TestSessionResumption(t *testing.T) {
	ca, s := fixture(t, false)
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		cfg := ca.ClientConfig("a.test")
		cfg.MinVersion, cfg.MaxVersion = v, v
		cfg.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		var resumed []bool
		for range 3 {
			st, err := s.dial(cfg)
			if err != nil {
				t.Fatalf("%s: %v", tls.VersionName(v), err)
			}
			resumed = append(resumed, st.DidResume)
		}
		if resumed[0] || !resumed[1] || !resumed[2] {
			t.Errorf("%s: DidResume %v, want [false true true]", tls.VersionName(v), resumed)
		}
	}
}

// TestCertsFromCallback: each handshake asks the callback, so swapping the
// set it returns changes the certificate on the next handshake.
func TestCertsFromCallback(t *testing.T) {
	ca := tlstest.NewCA(t)
	old, renewed := ca.Pair(t, "a.test"), ca.Pair(t, "a.test")
	var mu sync.Mutex
	cur := NewCertSet(map[string]*tls.Certificate{"a.test": &old}, nil)
	s := startTLS(t, ServerConfig(func() *CertSet { mu.Lock(); defer mu.Unlock(); return cur }))
	serial := func() string {
		st, err := s.dial(ca.ClientConfig("a.test"))
		if err != nil {
			t.Fatal(err)
		}
		return st.PeerCertificates[0].SerialNumber.String()
	}
	if serial() != old.Leaf.SerialNumber.String() {
		t.Fatal("first handshake did not get the first certificate")
	}
	mu.Lock()
	cur = NewCertSet(map[string]*tls.Certificate{"a.test": &renewed}, nil)
	mu.Unlock()
	if serial() != renewed.Leaf.SerialNumber.String() {
		t.Fatal("handshake after the swap did not get the new certificate")
	}
}

func TestTLSTestFiles(t *testing.T) {
	ca := tlstest.NewCA(t)
	dir := t.TempDir()
	_, key := ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	pemData, err := os.ReadFile(ca.WriteCA(t, dir))
	if err != nil || !x509.NewCertPool().AppendCertsFromPEM(pemData) {
		t.Fatalf("CA file: %v", err)
	}
}

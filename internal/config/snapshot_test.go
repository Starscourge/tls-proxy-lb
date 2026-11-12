package config

import (
	"sync"
	"testing"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
)

func mustSnapshot(t *testing.T, src string) *Snapshot {
	t.Helper()
	c, err := Parse("test.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSnapshot(c)
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
	if s.Timeouts.HeaderRead.Milliseconds() != 2500 || s.Limits.MaxConns != 500 || len(s.Listeners) != 2 {
		t.Fatalf("settings not copied: %+v %+v %v", s.Timeouts, s.Limits, s.Listeners)
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
	if _, err := NewSnapshot(c); err == nil {
		t.Fatal("route to an unknown pool accepted")
	}
	c.Routes[0].Pool = "p"
	c.DefaultPool = "gone"
	if _, err := NewSnapshot(c); err == nil {
		t.Fatal("unknown default pool accepted")
	}
	c.DefaultPool = ""
	c.Timeouts.Idle = 0
	if _, err := NewSnapshot(c); err == nil {
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

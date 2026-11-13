package lb

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
)

// healthPool returns a pool of n backends with health targets from a
// checker that runs no active checks and ejects on one passive failure,
// for an hour. Ejecting a backend is then b.Health.ReportFailure().
func healthPool(t *testing.T, n int, alg Algorithm) *Pool {
	t.Helper()
	chk := health.NewChecker(health.Config{Options: health.Options{PassiveFailures: 1, Eject: time.Hour}})
	t.Cleanup(chk.Close)
	p, err := NewPool("p", addrsOf(backends(n)), PoolOptions{Algorithm: alg})
	if err != nil {
		t.Fatal(err)
	}
	p.Health = chk
	for _, b := range p.Backends {
		b.Health = chk.Target(b.Addr)
	}
	return p
}

func TestHealthyFollowsState(t *testing.T) {
	p := healthPool(t, 3, RoundRobin)
	bs := p.Backends
	if got := p.Healthy(); !slices.Equal(got, bs) {
		t.Fatalf("healthy %d of 3 at start", len(got))
	}
	bs[1].Health.ReportFailure()
	if got := p.Healthy(); !slices.Equal(got, []*Backend{bs[0], bs[2]}) {
		t.Fatalf("after ejecting b1: %v", addrsOf(got))
	}
	for range 10 {
		if b := p.Pick(&RequestInfo{}); b == bs[1] {
			t.Fatal("picked an ejected backend")
		}
	}
	bs[0].Health.ReportFailure()
	bs[2].Health.ReportFailure()
	if got := p.Healthy(); len(got) != 0 {
		t.Fatalf("healthy %v with every backend ejected", addrsOf(got))
	}
	if b := p.Pick(&RequestInfo{}); b != nil {
		t.Fatalf("picked %s with none healthy", b.Addr)
	}
}

// The ring skips an ejected backend: its keys move to the next backend on
// the ring and no other key moves.
func TestHealthyRingSkips(t *testing.T) {
	p := healthPool(t, 5, ConsistentHash)
	before := make([]*Backend, 1000)
	for i := range before {
		before[i] = p.Pick(&RequestInfo{ClientIP: ipKey(i)})
	}
	dead := p.Backends[2]
	dead.Health.ReportFailure()
	moved := 0
	for i := range before {
		after := p.Pick(&RequestInfo{ClientIP: ipKey(i)})
		if after == dead {
			t.Fatal("ring picked the ejected backend")
		}
		if after != before[i] {
			if before[i] != dead {
				t.Fatalf("key %d moved off %s, which is healthy", i, before[i].Addr)
			}
			moved++
		}
	}
	if moved == 0 {
		t.Fatal("no key was on the ejected backend")
	}
}

func TestPickExcluding(t *testing.T) {
	p := healthPool(t, 3, RoundRobin)
	bs := p.Backends
	var buf []*Backend
	for range 10 {
		if b := p.PickExcluding(&RequestInfo{}, []*Backend{bs[0]}, &buf); b == bs[0] || b == nil {
			t.Fatalf("picked %v excluding b0", b)
		}
	}
	bs[1].Health.ReportFailure()
	if b := p.PickExcluding(&RequestInfo{}, []*Backend{bs[0]}, &buf); b != bs[2] {
		t.Fatalf("picked %v, want the only healthy backend not excluded", b)
	}
	if b := p.PickExcluding(&RequestInfo{}, []*Backend{bs[0], bs[2]}, &buf); b != nil {
		t.Fatalf("picked %s with every healthy backend excluded", b.Addr)
	}
	// Least-connections and the ring honour the exclusion too.
	for _, alg := range []Algorithm{LeastConn, ConsistentHash} {
		p := healthPool(t, 3, alg)
		first := p.Pick(&RequestInfo{ClientIP: "192.0.2.1"})
		for range 10 {
			if b := p.PickExcluding(&RequestInfo{ClientIP: "192.0.2.1"}, []*Backend{first}, &buf); b == first || b == nil {
				t.Fatalf("%s: picked %v excluding %s", alg, b, first.Addr)
			}
		}
	}
}

// Picks from a pool with health checks allocate nothing between health
// changes, and retries allocate nothing once their buffer has grown.
func TestHealthyPickZeroAllocs(t *testing.T) {
	for _, alg := range Algorithms {
		p := healthPool(t, 5, alg)
		p.Backends[3].Health.ReportFailure()
		h := http.Header{}
		r := &RequestInfo{ClientIP: "192.0.2.7", Header: h}
		p.Pick(r) // builds the healthy set
		if n := testing.AllocsPerRun(1000, func() { p.Pick(r) }); n != 0 {
			t.Errorf("%s: Pick allocates %v times", alg, n)
		}
		buf := make([]*Backend, 0, 5)
		exclude := []*Backend{p.Backends[0]}
		if n := testing.AllocsPerRun(1000, func() { p.PickExcluding(r, exclude, &buf) }); n != 0 {
			t.Errorf("%s: PickExcluding allocates %v times", alg, n)
		}
	}
}

// With every backend healthy the pool's own slice is the healthy set.
func TestHealthyAllHealthyIsBackends(t *testing.T) {
	p := healthPool(t, 3, RoundRobin)
	if got := p.Healthy(); &got[0] != &p.Backends[0] {
		t.Fatal("healthy set copied with every backend healthy")
	}
	p2, _ := NewPool("q", []string{"a:1"}, PoolOptions{})
	if got := p2.Healthy(); &got[0] != &p2.Backends[0] {
		t.Fatal("pool without a checker did not return its backends")
	}
}

func addrsOf(bs []*Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Addr
	}
	return out
}

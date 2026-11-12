package lb

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func backends(n int) []*Backend {
	out := make([]*Backend, n)
	for i := range out {
		out[i] = NewBackend(fmt.Sprintf("10.0.0.%d:8000", i+1))
	}
	return out
}

func counts(picks []*Backend) map[*Backend]int {
	m := map[*Backend]int{}
	for _, b := range picks {
		m[b]++
	}
	return m
}

// assertZeroAllocs fails if one pick allocates.
func assertZeroAllocs(t *testing.T, bal Balancer, r *RequestInfo, healthy []*Backend) {
	t.Helper()
	if n := testing.AllocsPerRun(1000, func() { bal.Pick(r, healthy) }); n != 0 {
		t.Fatalf("Pick allocates %v times per call, want 0", n)
	}
}

func TestEmptyHealthyPicksNil(t *testing.T) {
	for name, bal := range map[string]Balancer{
		"round_robin": &RoundRobinBalancer{},
		"least_conn":  &LeastConnBalancer{},
	} {
		if b := bal.Pick(&RequestInfo{}, nil); b != nil {
			t.Errorf("%s: picked %v from no backends", name, b.Addr)
		}
	}
}

func TestRoundRobinExactUnderConcurrency(t *testing.T) {
	bs := backends(3)
	rr := &RoundRobinBalancer{}
	const workers, per = 30, 100
	picks := make([]*Backend, workers*per)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range per {
				picks[w*per+i] = rr.Pick(nil, bs)
			}
		})
	}
	wg.Wait()
	for _, b := range bs {
		if got := counts(picks)[b]; got != workers*per/3 {
			t.Errorf("%s got %d picks, want %d", b.Addr, got, workers*per/3)
		}
	}
}

func TestRoundRobinOrder(t *testing.T) {
	bs := backends(3)
	rr := &RoundRobinBalancer{}
	for i := range 7 {
		if got, want := rr.Pick(nil, bs), bs[i%3]; got != want {
			t.Fatalf("pick %d = %s, want %s", i, got.Addr, want.Addr)
		}
	}
}

func TestLeastConnPicksMinimum(t *testing.T) {
	bs := backends(3)
	bs[0].inflight.Store(5)
	bs[1].inflight.Store(2)
	bs[2].inflight.Store(7)
	lc := &LeastConnBalancer{}
	for i := range 10 {
		if got := lc.Pick(nil, bs); got != bs[1] {
			t.Fatalf("pick %d = %s (in flight %d), want %s", i, got.Addr, got.InFlight(), bs[1].Addr)
		}
	}
}

// TestLeastConnTiesRotate: with nothing in flight, every backend ties, and
// the rotating start spreads sequential picks exactly evenly.
func TestLeastConnTiesRotate(t *testing.T) {
	bs := backends(3)
	lc := &LeastConnBalancer{}
	var picks []*Backend
	for range 300 {
		picks = append(picks, lc.Pick(nil, bs))
	}
	for _, b := range bs {
		if got := counts(picks)[b]; got != 100 {
			t.Errorf("%s got %d picks, want 100", b.Addr, got)
		}
	}
}

// TestLeastConnTiesAmongMinimum: ties are broken among the minimum only.
func TestLeastConnTiesAmongMinimum(t *testing.T) {
	bs := backends(4)
	bs[0].inflight.Store(3)
	bs[2].inflight.Store(3)
	lc := &LeastConnBalancer{}
	got := map[*Backend]int{}
	for range 40 {
		got[lc.Pick(nil, bs)]++
	}
	if got[bs[0]] != 0 || got[bs[2]] != 0 || got[bs[1]] != 20 || got[bs[3]] != 20 {
		t.Fatalf("split %d/%d/%d/%d, want 0/20/0/20", got[bs[0]], got[bs[1]], got[bs[2]], got[bs[3]])
	}
}

// TestLeastConnFollowsLoad: picks that are held (Acquire without Release)
// keep the in-flight counts within one of each other.
func TestLeastConnFollowsLoad(t *testing.T) {
	bs := backends(3)
	lc := &LeastConnBalancer{}
	for range 30 {
		lc.Pick(nil, bs).Acquire()
	}
	for _, b := range bs {
		if b.InFlight() != 10 {
			t.Errorf("%s has %d in flight, want 10", b.Addr, b.InFlight())
		}
	}
}

func TestPickZeroAllocs(t *testing.T) {
	bs := backends(5)
	r := &RequestInfo{ClientIP: "192.0.2.7", Header: http.Header{}}
	assertZeroAllocs(t, &RoundRobinBalancer{}, r, bs)
	assertZeroAllocs(t, &LeastConnBalancer{}, r, bs)
}

func TestNewPool(t *testing.T) {
	p, err := NewPool("p", []string{"a:1", "b:2"}, PoolOptions{Algorithm: LeastConn})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Backends) != 2 || p.Backends[1].Addr != "b:2" {
		t.Fatalf("backends %+v", p.Backends)
	}
	if _, ok := p.Balancer.(*LeastConnBalancer); !ok {
		t.Fatalf("balancer %T", p.Balancer)
	}
	if b := p.Pick(&RequestInfo{}); b == nil {
		t.Fatal("pool picked nil")
	}
	if p, _ := NewPool("p", []string{"a:1"}, PoolOptions{}); p == nil {
		t.Fatal("default algorithm rejected")
	} else if _, ok := p.Balancer.(*RoundRobinBalancer); !ok {
		t.Fatalf("default balancer %T, want round robin", p.Balancer)
	}
	if _, err := NewPool("p", nil, PoolOptions{}); err == nil {
		t.Error("empty pool accepted")
	}
	if _, err := NewPool("p", []string{"a:1"}, PoolOptions{Algorithm: "random"}); err == nil {
		t.Error("unknown algorithm accepted")
	}
}

func benchPick(b *testing.B, bal Balancer, r *RequestInfo, bs []*Backend) {
	b.ReportAllocs()
	for b.Loop() {
		bal.Pick(r, bs)
	}
}

func benchPickParallel(b *testing.B, bal Balancer, r *RequestInfo, bs []*Backend) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bal.Pick(r, bs)
		}
	})
}

func BenchmarkRoundRobin(b *testing.B) {
	benchPick(b, &RoundRobinBalancer{}, &RequestInfo{}, backends(10))
}

func BenchmarkRoundRobinParallel(b *testing.B) {
	benchPickParallel(b, &RoundRobinBalancer{}, &RequestInfo{}, backends(10))
}

func BenchmarkLeastConn(b *testing.B) {
	benchPick(b, &LeastConnBalancer{}, &RequestInfo{}, backends(10))
}

func BenchmarkLeastConnParallel(b *testing.B) {
	benchPickParallel(b, &LeastConnBalancer{}, &RequestInfo{}, backends(10))
}

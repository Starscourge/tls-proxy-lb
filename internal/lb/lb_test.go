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

func TestPickZeroAllocs(t *testing.T) {
	bs := backends(5)
	r := &RequestInfo{ClientIP: "192.0.2.7", Header: http.Header{}}
	assertZeroAllocs(t, &RoundRobinBalancer{}, r, bs)
}

func TestNewPool(t *testing.T) {
	p, err := NewPool("p", []string{"a:1", "b:2"}, PoolOptions{Algorithm: RoundRobin})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Backends) != 2 || p.Backends[1].Addr != "b:2" {
		t.Fatalf("backends %+v", p.Backends)
	}
	if _, ok := p.Balancer.(*RoundRobinBalancer); !ok {
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

func benchPick(b *testing.B, bal Balancer, r *RequestInfo, n int) {
	bs := backends(n)
	b.ReportAllocs()
	for b.Loop() {
		bal.Pick(r, bs)
	}
}

func benchPickParallel(b *testing.B, bal Balancer, r *RequestInfo, n int) {
	bs := backends(n)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bal.Pick(r, bs)
		}
	})
}

func BenchmarkRoundRobin(b *testing.B) {
	benchPick(b, &RoundRobinBalancer{}, &RequestInfo{}, 10)
}

func BenchmarkRoundRobinParallel(b *testing.B) {
	benchPickParallel(b, &RoundRobinBalancer{}, &RequestInfo{}, 10)
}


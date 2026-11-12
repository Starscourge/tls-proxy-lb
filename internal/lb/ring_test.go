package lb

import (
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"
)

// ipKey returns the i-th of a run of distinct IPv4 client addresses.
func ipKey(i int) string {
	return fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
}

func without(bs []*Backend, drop *Backend) []*Backend {
	return slices.DeleteFunc(slices.Clone(bs), func(b *Backend) bool { return b == drop })
}

func TestRingSameKeySameBackend(t *testing.T) {
	bs := backends(5)
	r := NewRingBalancer(bs, 0, "")
	for i := range 1000 {
		req := &RequestInfo{ClientIP: ipKey(i)}
		first := r.Pick(req, bs)
		for range 5 {
			if got := r.Pick(req, bs); got != first {
				t.Fatalf("key %s: %s then %s", req.ClientIP, first.Addr, got.Addr)
			}
		}
	}
}

// TestRingIndependentOfOrder: the ring depends on the set of addresses, not
// their order or the Backend values, so a rebuilt ring (another instance, a
// reload) sends every key to the same address.
func TestRingIndependentOfOrder(t *testing.T) {
	bs := backends(5)
	shuffled := backends(5)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	r1 := NewRingBalancer(bs, 0, "")
	r2 := NewRingBalancer(shuffled, 0, "")
	for i := range 10000 {
		req := &RequestInfo{ClientIP: ipKey(i)}
		if a, b := r1.Pick(req, bs).Addr, r2.Pick(req, shuffled).Addr; a != b {
			t.Fatalf("key %s: %s on one ring, %s on the other", req.ClientIP, a, b)
		}
	}
}

func TestRingHeaderKey(t *testing.T) {
	bs := backends(5)
	// The configured name is canonicalised, so its case does not matter.
	r := NewRingBalancer(bs, 0, "x-user-id")
	h := http.Header{}
	h.Set("X-User-Id", "alice")
	viaHeader := &RequestInfo{ClientIP: "192.0.2.1", Header: h}
	if got := r.Key(viaHeader); got != "alice" {
		t.Fatalf("key %q, want the header value", got)
	}
	// The same user from another address reaches the same backend.
	other := &RequestInfo{ClientIP: "198.51.100.9", Header: h}
	if r.Pick(viaHeader, bs) != r.Pick(other, bs) {
		t.Fatal("same header value, different backends")
	}
	for name, hdr := range map[string]http.Header{
		"missing": {},
		"empty":   {"X-User-Id": {""}},
		"nil":     nil,
	} {
		req := &RequestInfo{ClientIP: "192.0.2.1", Header: hdr}
		if got := r.Key(req); got != "192.0.2.1" {
			t.Errorf("%s header: key %q, want the client IP", name, got)
		}
	}
}

// TestRingRemovalRemapsOneNth: removing one of N backends moves about 1/N
// of the keys, every moved key was on the removed backend, and skipping the
// backend as unhealthy gives exactly the ring rebuilt without it.
func TestRingRemovalRemapsOneNth(t *testing.T) {
	const keys = 10000
	const tolerance = 0.30 // moved fraction within ±30% of 1/N
	for _, n := range []int{3, 5, 10} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			bs := backends(n)
			full := NewRingBalancer(bs, 0, "")
			for _, removed := range bs {
				rest := without(bs, removed)
				rebuilt := NewRingBalancer(rest, 0, "")
				moved := 0
				for i := range keys {
					h := Hash64(fmt.Sprintf("user-%d", i))
					before := full.Lookup(h, bs)
					after := rebuilt.Lookup(h, rest)
					if skipped := full.Lookup(h, rest); skipped != after {
						t.Fatalf("key %d: skipping gives %s, rebuilding gives %s", i, skipped.Addr, after.Addr)
					}
					if before == after {
						continue
					}
					if before != removed {
						t.Fatalf("key %d moved from %s, which was not removed", i, before.Addr)
					}
					moved++
				}
				frac, want := float64(moved)/keys, 1/float64(n)
				t.Logf("remove %s: %d of %d keys moved (%.2f%%, 1/N = %.2f%%)", removed.Addr, moved, keys, 100*frac, 100*want)
				if math.Abs(frac-want) > tolerance*want {
					t.Errorf("remove %s: moved %.4f of keys, want %.4f ± %.0f%%", removed.Addr, frac, want, 100*tolerance)
				}
			}
		})
	}
}

// spread returns the largest relative deviation of any backend's key count
// from the mean, and the coefficient of variation (stddev / mean), for keys
// given by their hashes.
func spread(r *RingBalancer, bs []*Backend, hashes []uint64) (maxDev, cv float64) {
	c := make(map[*Backend]int, len(bs))
	for _, h := range hashes {
		c[r.Lookup(h, bs)]++
	}
	mean := float64(len(hashes)) / float64(len(bs))
	var sq float64
	for _, b := range bs {
		d := float64(c[b]) - mean
		sq += d * d
		maxDev = max(maxDev, math.Abs(d)/mean)
	}
	return maxDev, math.Sqrt(sq/float64(len(bs))) / mean
}

// addrSet returns n backends for one trial. Trial 0 is the lab's addresses
// (10.0.0.11:8000 upwards); later trials draw addresses from a source seeded
// with the trial number, so every run measures the same sets.
func addrSet(n, trial int) []*Backend {
	rng := rand.New(rand.NewPCG(uint64(trial), 1))
	out := make([]*Backend, n)
	for i := range out {
		addr := fmt.Sprintf("10.0.0.%d:8000", 11+i)
		if trial > 0 {
			addr = fmt.Sprintf("10.%d.%d.%d:%d", rng.IntN(256), rng.IntN(256), rng.IntN(256), 8000+rng.IntN(1000))
		}
		out[i] = NewBackend(addr)
	}
	return out
}

// TestRingSpread measures how evenly 100,000 client IPs spread over N
// backends for several virtual-node counts. One address set is noisy, so
// each row averages 20 sets: the lab's addresses and 19 seeded random ones.
// It logs the table recorded in docs/DESIGN.md and holds the default count
// to a bound. Every input is fixed, so the result is deterministic.
func TestRingSpread(t *testing.T) {
	const (
		keys   = 100000
		trials = 20
		// At DefaultVirtualNodes the most loaded or least loaded backend is,
		// averaged over the sets, within meanMaxAllowed of the mean, and in
		// any single set within worstAllowed.
		meanMaxAllowed = 0.15
		worstAllowed   = 0.25
	)
	hashes := make([]uint64, keys)
	for i := range hashes {
		hashes[i] = Hash64(ipKey(i))
	}
	t.Logf("%6s %3s | %8s %13s %13s | %12s", "vnodes", "N", "mean cv", "mean max dev", "worst max dev", "lab max dev")
	for _, v := range []int{1, 10, 100, DefaultVirtualNodes, 200} {
		for _, n := range []int{3, 5, 10} {
			var sumCV, sumMax, worst, lab float64
			for trial := range trials {
				bs := addrSet(n, trial)
				maxDev, cv := spread(NewRingBalancer(bs, v, ""), bs, hashes)
				sumCV, sumMax, worst = sumCV+cv, sumMax+maxDev, max(worst, maxDev)
				if trial == 0 {
					lab = maxDev
				}
			}
			meanCV, meanMax := sumCV/trials, sumMax/trials
			t.Logf("%6d %3d | %7.2f%% %12.2f%% %12.2f%% | %11.2f%%", v, n, 100*meanCV, 100*meanMax, 100*worst, 100*lab)
			if v == DefaultVirtualNodes && (meanMax > meanMaxAllowed || worst > worstAllowed) {
				t.Errorf("vnodes %d, N %d: max deviation %.1f%% on average, %.1f%% worst; want <= %.0f%% and %.0f%%",
					v, n, 100*meanMax, 100*worst, 100*meanMaxAllowed, 100*worstAllowed)
			}
		}
	}
}

func TestRingEmpty(t *testing.T) {
	bs := backends(3)
	if b := NewRingBalancer(bs, 0, "").Pick(&RequestInfo{ClientIP: "x"}, nil); b != nil {
		t.Errorf("picked %s with no healthy backends", b.Addr)
	}
	if b := NewRingBalancer(nil, 0, "").Pick(&RequestInfo{ClientIP: "x"}, bs); b != nil {
		t.Errorf("empty ring picked %s", b.Addr)
	}
	// Healthy backends that are not on the ring are never returned.
	if b := NewRingBalancer(bs, 0, "").Pick(&RequestInfo{ClientIP: "x"}, backends(3)); b != nil {
		t.Errorf("picked %s, which is not on the ring", b.Addr)
	}
}

func TestRingZeroAllocs(t *testing.T) {
	bs := backends(10)
	h := http.Header{}
	h.Set("X-User-Id", "alice")
	r := &RequestInfo{ClientIP: "192.0.2.7", Header: h}
	assertZeroAllocs(t, NewRingBalancer(bs, 0, ""), r, bs)
	assertZeroAllocs(t, NewRingBalancer(bs, 0, "X-User-Id"), r, bs)
	// Falling back from a missing header to the client IP.
	assertZeroAllocs(t, NewRingBalancer(bs, 0, "X-Other"), r, bs)
}

func TestNewPoolConsistentHash(t *testing.T) {
	p, err := NewPool("p", []string{"a:1", "b:2"}, PoolOptions{Algorithm: ConsistentHash, HashHeader: "x-k"})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := p.Balancer.(*RingBalancer)
	if !ok {
		t.Fatalf("balancer %T", p.Balancer)
	}
	if r.header != "X-K" || len(r.hashes) != 2*DefaultVirtualNodes {
		t.Fatalf("header %q, %d points", r.header, len(r.hashes))
	}
}

func BenchmarkRingClientIP(b *testing.B) {
	bs := backends(10)
	benchPick(b, NewRingBalancer(bs, 0, ""), &RequestInfo{ClientIP: "192.0.2.7"}, bs)
}

func BenchmarkRingHeader(b *testing.B) {
	h := http.Header{}
	h.Set("X-User-Id", "alice")
	bs := backends(10)
	benchPick(b, NewRingBalancer(bs, 0, "X-User-Id"), &RequestInfo{ClientIP: "192.0.2.7", Header: h}, bs)
}

func BenchmarkRingParallel(b *testing.B) {
	bs := backends(10)
	benchPickParallel(b, NewRingBalancer(bs, 0, ""), &RequestInfo{ClientIP: "192.0.2.7"}, bs)
}

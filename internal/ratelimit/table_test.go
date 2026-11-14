package ratelimit

import (
	"fmt"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// fakeClock is a Clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// v4 returns a distinct IPv4 address for each i below 2^32.
func v4(i uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
}

func TestTableBurstThenRate(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 5, Burst: 10})
	c := ip("192.0.2.1")
	for i := range 10 {
		if ok, _ := lim.Allow(c); !ok {
			t.Fatalf("burst request %d refused", i+1)
		}
	}
	if ok, wait := lim.Allow(c); ok || wait != 200*time.Millisecond {
		t.Fatalf("after burst: ok=%v wait=%v", ok, wait)
	}
	accepted := 0
	for range 10_000 {
		clk.Advance(time.Millisecond)
		if ok, _ := lim.Allow(c); ok {
			accepted++
		}
	}
	if accepted != 50 {
		t.Fatalf("accepted %d over 10s at 5/s, want exactly 50", accepted)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	tab := New(Options{})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{})
	if lim != nil {
		t.Fatal("disabled limit gave a non-nil limiter")
	}
	if ok, _ := lim.Allow(ip("192.0.2.1")); !ok {
		t.Fatal("nil limiter refused")
	}
	if tab.Len() != 0 {
		t.Fatal("nil limiter stored a bucket")
	}
}

func TestClientsAndRoutesAreSeparate(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk})
	defer tab.Close()
	a := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	b := tab.Limiter("b.test", Limit{Rate: 1, Burst: 1})
	c1, c2 := ip("192.0.2.1"), ip("192.0.2.2")
	if ok, _ := a.Allow(c1); !ok {
		t.Fatal("first request refused")
	}
	if ok, _ := a.Allow(c1); ok {
		t.Fatal("over the limit allowed")
	}
	if ok, _ := a.Allow(c2); !ok {
		t.Fatal("another client was limited by the first")
	}
	if ok, _ := b.Allow(c1); !ok {
		t.Fatal("another route was limited by the first")
	}
	if tab.Len() != 3 {
		t.Fatalf("Len = %d, want 3", tab.Len())
	}
}

func TestRouteStateSurvivesNewLimiter(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk})
	defer tab.Close()
	c := ip("192.0.2.1")
	old := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	old.Allow(c)
	// A reload builds a new limiter for the same route: the client's empty
	// bucket carries over.
	reloaded := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	if ok, _ := reloaded.Allow(c); ok {
		t.Fatal("a new limiter for the same route reset the client's bucket")
	}
}

func TestIPv6KeyedByPrefix64(t *testing.T) {
	tab := New(Options{Clock: newFakeClock()})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 2})
	if ok, _ := lim.Allow(ip("2001:db8:1:2::1")); !ok {
		t.Fatal("first refused")
	}
	if ok, _ := lim.Allow(ip("2001:db8:1:2:ffff:ffff:ffff:ffff%eth0")); !ok {
		t.Fatal("second refused")
	}
	if ok, _ := lim.Allow(ip("2001:db8:1:2:dead:beef::")); ok {
		t.Fatal("a third address in the same /64 dodged the limit")
	}
	if ok, _ := lim.Allow(ip("2001:db8:1:3::1")); !ok {
		t.Fatal("a different /64 was limited")
	}
}

func TestKeyAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7":                    "192.0.2.7",
		"::ffff:192.0.2.7":             "192.0.2.7",
		"2001:db8:a:b:c:d:e:f":         "2001:db8:a:b::",
		"fe80::1:2:3:4%eth0":           "fe80::",
		"2001:db8:a:b:ffff::1":         "2001:db8:a:b::",
		"::1":                          "::",
		"2001:db8:ffff:ffff:ffff::abc": "2001:db8:ffff:ffff::",
	} {
		if got := KeyAddr(ip(in)); got != ip(want) {
			t.Errorf("KeyAddr(%s) = %s, want %s", in, got, want)
		}
	}
	if got := KeyAddr(netip.Addr{}); got.IsValid() {
		t.Errorf("KeyAddr(zero) = %v", got)
	}
}

func TestSweepDropsFullBuckets(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk})
	defer tab.Close()
	slow := tab.Limiter("a.test", Limit{Rate: 1, Burst: 10})  // 10s to refill
	fast := tab.Limiter("b.test", Limit{Rate: 100, Burst: 1}) // 10ms
	for i := range 100 {
		slow.Allow(v4(uint32(i)))
		fast.Allow(v4(uint32(i)))
	}
	if tab.Len() != 200 {
		t.Fatalf("Len = %d, want 200", tab.Len())
	}
	if n := tab.Sweep(); n != 0 {
		t.Fatalf("swept %d buckets that were not full", n)
	}
	clk.Advance(10 * time.Millisecond)
	if n := tab.Sweep(); n != 100 {
		t.Fatalf("swept %d, want the 100 refilled fast buckets", n)
	}
	clk.Advance(time.Second - 10*time.Millisecond)
	if n := tab.Sweep(); n != 100 {
		t.Fatalf("swept %d, want the 100 slow buckets, full after one token", n)
	}
	if tab.Len() != 0 {
		t.Fatalf("Len = %d after sweeping everything", tab.Len())
	}
}

func TestSweepShrinksMaps(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	for i := range 100_000 {
		lim.Allow(v4(uint32(i)))
	}
	clk.Advance(time.Second)
	tab.Sweep()
	for i := range tab.shards {
		if s := &tab.shards[i]; s.peak != 0 || len(s.m) != 0 {
			t.Fatalf("shard %d: peak %d, len %d after a full sweep", i, s.peak, len(s.m))
		}
	}
}

func TestCapEvictsLeastRecentlyUsed(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk, MaxEntries: NumShards}) // one per shard
	defer tab.Close()
	if tab.MaxEntries() != NumShards {
		t.Fatalf("MaxEntries = %d", tab.MaxEntries())
	}
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	for i := range 10 * NumShards {
		lim.Allow(v4(uint32(i)))
		clk.Advance(time.Millisecond)
	}
	if tab.Len() > NumShards {
		t.Fatalf("Len = %d, over the cap %d", tab.Len(), NumShards)
	}
	if tab.Evictions() == 0 {
		t.Fatal("no evictions counted")
	}
}

func TestEvictionPrefersFullBuckets(t *testing.T) {
	clk := newFakeClock()
	tab := New(Options{Clock: clk, MaxEntries: NumShards})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 1})
	for i := range 10 * NumShards {
		lim.Allow(v4(uint32(i)))
		clk.Advance(time.Second) // every earlier bucket is full again
	}
	if tab.Evictions() != 0 {
		t.Fatalf("Evictions = %d; dropping full buckets loses nothing", tab.Evictions())
	}
}

// TestMillionKeysMemory is the Phase 6 "Done when" item: a million distinct
// keys keep the table under its cap, in entries and in heap bytes, and a
// second million grow it no further.
func TestMillionKeysMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a million buckets")
	}
	const (
		capEntries = 100_000
		keys       = 1_000_000
		// maxBytesPerEntry is the budget per bucket at the cap. A map
		// slot is 32 bytes (20-byte key padded to 24, 8-byte pointer)
		// plus a control byte, and the bucket an 8-byte allocation; under
		// eviction churn the deleted-slot markers keep a Swiss-table map
		// at about 2.5 slots per live entry. It measures 115 bytes.
		maxBytesPerEntry = 160
	)
	clk := newFakeClock()
	before := heapInUse()
	tab := New(Options{Clock: clk, MaxEntries: capEntries})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 10})
	fill := func(from int) {
		for i := from; i < from+keys; i++ {
			lim.Allow(v4(uint32(i)))
			if i%1000 == 0 {
				clk.Advance(time.Millisecond)
			}
		}
	}
	fill(0)
	grown := heapInUse() - before
	if tab.Len() > capEntries {
		t.Fatalf("Len = %d, over the cap %d", tab.Len(), capEntries)
	}
	t.Logf("%d keys, cap %d: Len %d, evictions %d, heap growth %d bytes (%.1f per capped entry)",
		keys, capEntries, tab.Len(), tab.Evictions(), grown, float64(grown)/capEntries)
	if grown > capEntries*maxBytesPerEntry {
		t.Fatalf("heap grew %d bytes, over %d entries x %d bytes", grown, capEntries, maxBytesPerEntry)
	}

	fill(keys)
	grown2 := heapInUse() - before
	runtime.KeepAlive(tab)
	t.Logf("after %d more keys: Len %d, heap growth %d bytes", keys, tab.Len(), grown2)
	if tab.Len() > capEntries || grown2 > grown+grown/20 {
		t.Fatalf("a second million keys grew the table: Len %d, heap %d -> %d bytes", tab.Len(), grown, grown2)
	}
}

func heapInUse() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

func TestConcurrentAllow(t *testing.T) {
	tab := New(Options{MaxEntries: 4 * NumShards, SweepInterval: time.Millisecond})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1000, Burst: 5})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 20_000 {
				lim.Allow(v4(uint32(g*1_000_000 + i%3000)))
			}
		})
	}
	wg.Wait()
	if tab.Len() > tab.MaxEntries() {
		t.Fatalf("Len = %d over cap %d", tab.Len(), tab.MaxEntries())
	}
	tab.Close()
	got := 0
	for i := range tab.shards {
		got += len(tab.shards[i].m)
	}
	if got != tab.Len() {
		t.Fatalf("entry counter %d disagrees with the maps' %d", tab.Len(), got)
	}
}

func TestCloseStopsSweeper(t *testing.T) {
	defer goleak.VerifyNone(t)
	tab := New(Options{SweepInterval: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	tab.Close()
	tab.Close() // idempotent
}

func TestAllowAllocs(t *testing.T) {
	tab := New(Options{})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1e6, Burst: 100})
	c := ip("2001:db8::1")
	lim.Allow(c)
	if n := testing.AllocsPerRun(1000, func() { lim.Allow(c) }); n != 0 {
		t.Fatalf("Allow allocates %v per call", n)
	}
}

func BenchmarkAllowOneClient(b *testing.B) {
	tab := New(Options{})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1e9, Burst: MaxBurst})
	c := ip("192.0.2.1")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow(c)
		}
	})
}

func BenchmarkAllowManyClients(b *testing.B) {
	tab := New(Options{})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1e9, Burst: MaxBurst})
	addrs := make([]netip.Addr, 4096)
	for i := range addrs {
		addrs[i] = v4(uint32(i) * 7919)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			lim.Allow(addrs[i%len(addrs)])
			i++
		}
	})
}

func ExampleLimiter_Allow() {
	tab := New(Options{})
	defer tab.Close()
	lim := tab.Limiter("a.test", Limit{Rate: 1, Burst: 2})
	for range 3 {
		ok, _ := lim.Allow(netip.MustParseAddr("192.0.2.1"))
		fmt.Println(ok)
	}
	// Output:
	// true
	// true
	// false
}

package ratelimit

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimitValidate(t *testing.T) {
	for _, tc := range []struct {
		l  Limit
		ok bool
	}{
		{Limit{}, true},
		{Limit{Rate: 0, Burst: 0}, true},
		{Limit{Rate: 5, Burst: 10}, true},
		{Limit{Rate: MinRate, Burst: MaxBurst}, true},
		{Limit{Rate: MaxRate, Burst: 1}, true},
		{Limit{Rate: -1, Burst: 1}, false},
		{Limit{Rate: MinRate / 2, Burst: 1}, false},
		{Limit{Rate: MaxRate * 2, Burst: 1}, false},
		{Limit{Rate: math.NaN(), Burst: 1}, false},
		{Limit{Rate: 5, Burst: 0}, false},
		{Limit{Rate: 5, Burst: MaxBurst + 1}, false},
	} {
		if err := tc.l.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: Validate() = %v, want ok=%v", tc.l, err, tc.ok)
		}
	}
}

func TestParamsNoOverflow(t *testing.T) {
	p := Limit{Rate: MinRate, Burst: MaxBurst}.params()
	if p.capacity <= 0 || p.capacity/int64(MaxBurst) != p.interval {
		t.Fatalf("capacity overflowed: %+v", p)
	}
	// Two centuries of uptime, the whole burst spent: full stays in range.
	now := int64(200 * 365 * 24 * time.Hour)
	b := newBucket(now)
	for range 3 {
		b.take(now, p)
	}
	if f := b.full.Load(); f <= now || f > now+p.capacity {
		t.Fatalf("full = %d, want in (now, now+capacity]", f)
	}
}

// TestBurstThenExactRate is the Phase 6 "Done when" item: with an injected
// clock, the whole burst is allowed at once, and after it the accepted rate
// is exactly the configured rate.
func TestBurstThenExactRate(t *testing.T) {
	l := Limit{Rate: 5, Burst: 10}
	p := l.params()
	if p.interval != int64(200*time.Millisecond) {
		t.Fatalf("interval = %v, want 200ms", time.Duration(p.interval))
	}
	b := newBucket(0)
	for i := range l.Burst {
		if ok, _ := b.take(0, p); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := b.take(0, p)
	if ok || wait != 200*time.Millisecond {
		t.Fatalf("request after the burst: ok=%v wait=%v, want refused, 200ms", ok, wait)
	}

	// One request every millisecond for 10 seconds: exactly rate × 10 pass,
	// one every 200ms on the dot.
	var accepted []int64
	for now := int64(time.Millisecond); now <= int64(10*time.Second); now += int64(time.Millisecond) {
		if ok, _ := b.take(now, p); ok {
			accepted = append(accepted, now)
		}
	}
	if len(accepted) != 50 {
		t.Fatalf("accepted %d in 10s, want exactly 50", len(accepted))
	}
	for i, at := range accepted {
		if want := int64(i+1) * int64(200*time.Millisecond); at != want {
			t.Fatalf("acceptance %d at %v, want %v", i, time.Duration(at), time.Duration(want))
		}
	}
}

// TestTokens: the single stored time gives the token count the classic
// form would hold.
func TestTokens(t *testing.T) {
	p := Limit{Rate: 10, Burst: 3}.params() // 100ms per token
	b := newBucket(0)
	ms := int64(time.Millisecond)
	if got := b.tokens(0, p); got != p.capacity {
		t.Fatalf("new bucket holds %d, want %d", got, p.capacity)
	}
	b.take(0, p)
	b.take(0, p)
	for _, tc := range []struct{ at, want int64 }{
		{0, p.capacity - 2*p.interval},
		{50 * ms, p.capacity - 2*p.interval + 50*ms},
		{200 * ms, p.capacity},
		{time.Hour.Nanoseconds(), p.capacity},
	} {
		if got := b.tokens(tc.at, p); got != tc.want {
			t.Errorf("tokens at %v = %d, want %d", time.Duration(tc.at), got, tc.want)
		}
	}
}

func TestRefillCapsAtBurst(t *testing.T) {
	l := Limit{Rate: 10, Burst: 3}
	p := l.params()
	b := newBucket(0)
	for range 3 {
		b.take(0, p)
	}
	// An hour idle earns back only the burst.
	now := int64(time.Hour)
	n := 0
	for {
		if ok, _ := b.take(now, p); !ok {
			break
		}
		n++
	}
	if n != 3 {
		t.Fatalf("after a long idle spell %d passed at once, want burst 3", n)
	}
}

func TestPartialRefillWait(t *testing.T) {
	p := Limit{Rate: 2, Burst: 1}.params() // 500ms per request
	b := newBucket(0)
	b.take(0, p)
	ok, wait := b.take(int64(300*time.Millisecond), p)
	if ok || wait != 200*time.Millisecond {
		t.Fatalf("ok=%v wait=%v, want refused with 200ms to go", ok, wait)
	}
	if ok, _ := b.take(int64(500*time.Millisecond), p); !ok {
		t.Fatal("refused once the interval had passed")
	}
}

func TestClockBackwards(t *testing.T) {
	p := Limit{Rate: 1, Burst: 1}.params()
	b := newBucket(int64(10 * time.Second))
	b.take(int64(10*time.Second), p)
	if ok, _ := b.take(int64(5*time.Second), p); ok {
		t.Fatal("a clock going backwards earned credit")
	}
	if ok, _ := b.take(int64(11*time.Second), p); !ok {
		t.Fatal("refused one interval after the last use")
	}
}

func TestLimitShrinks(t *testing.T) {
	big := Limit{Rate: 1, Burst: 100}.params()
	small := Limit{Rate: 1, Burst: 2}.params()
	b := newBucket(0)
	n := 0
	for {
		if ok, _ := b.take(0, small); !ok {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("under a burst of 2, %d passed", n)
	}
	// Credit spent under a larger limit is still owed after it shrinks.
	b = newBucket(0)
	for range 50 {
		b.take(0, big)
	}
	if ok, wait := b.take(0, small); ok || wait != 49*time.Second {
		t.Fatalf("after spending 50 of 100 then shrinking to 2: ok=%v wait=%v, want refused for 49s", ok, wait)
	}
}

func TestFullAt(t *testing.T) {
	p := Limit{Rate: 1, Burst: 2}.params()
	b := newBucket(0)
	if !b.fullAt(0) {
		t.Fatal("new bucket not full")
	}
	b.take(0, p)
	if b.fullAt(int64(999 * time.Millisecond)) {
		t.Fatal("full before an interval passed")
	}
	if !b.fullAt(int64(time.Second)) {
		t.Fatal("not full after an interval")
	}
}

// TestConcurrentTakeExact: goroutines racing on one bucket at one instant
// get exactly the burst between them, never more.
func TestConcurrentTakeExact(t *testing.T) {
	p := Limit{Rate: 1, Burst: 100}.params()
	b := newBucket(0)
	var granted atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				if ok, _ := b.take(0, p); ok {
					granted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if granted.Load() != 100 {
		t.Fatalf("granted %d, want exactly the burst of 100", granted.Load())
	}
}

func TestRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		want int
	}{
		{0, 1},
		{time.Nanosecond, 1},
		{5 * time.Millisecond, 1},
		{time.Second, 1},
		{time.Second + 1, 2},
		{90 * time.Second, 90},
	} {
		if got := RetryAfter(tc.wait); got != tc.want {
			t.Errorf("RetryAfter(%v) = %d, want %d", tc.wait, got, tc.want)
		}
	}
}

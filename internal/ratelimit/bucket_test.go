package ratelimit

import (
	"math"
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
	b := fullBucket(0, p)
	// A very long idle spell refills to capacity without overflowing.
	b.tokens = 0
	b.refill(math.MaxInt64, p.capacity)
	if b.tokens != p.capacity {
		t.Fatalf("tokens = %d, want %d", b.tokens, p.capacity)
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
	b := fullBucket(0, p)
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

func TestRefillCapsAtBurst(t *testing.T) {
	l := Limit{Rate: 10, Burst: 3}
	p := l.params()
	b := fullBucket(0, p)
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
	b := fullBucket(0, p)
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
	b := fullBucket(int64(10*time.Second), p)
	b.take(int64(10*time.Second), p)
	if ok, _ := b.take(int64(5*time.Second), p); ok {
		t.Fatal("a clock going backwards earned credit")
	}
	if b.last != int64(10*time.Second) {
		t.Fatalf("last moved back to %v", time.Duration(b.last))
	}
	if ok, _ := b.take(int64(11*time.Second), p); !ok {
		t.Fatal("refused one interval after the last use")
	}
}

func TestLimitShrinks(t *testing.T) {
	big := Limit{Rate: 1, Burst: 100}.params()
	small := Limit{Rate: 1, Burst: 2}.params()
	b := fullBucket(0, big)
	n := 0
	for {
		if ok, _ := b.take(0, small); !ok {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("after the burst shrank to 2, %d passed", n)
	}
}

func TestFullAt(t *testing.T) {
	p := Limit{Rate: 1, Burst: 2}.params()
	b := fullBucket(0, p)
	if !b.fullAt(0, p.capacity) {
		t.Fatal("new bucket not full")
	}
	b.take(0, p)
	if b.fullAt(int64(999*time.Millisecond), p.capacity) {
		t.Fatal("full before an interval passed")
	}
	if !b.fullAt(int64(time.Second), p.capacity) {
		t.Fatal("not full after an interval")
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

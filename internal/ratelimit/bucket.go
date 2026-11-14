package ratelimit

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// Bounds on a Limit, so the bucket arithmetic cannot overflow.
const (
	// MinRate is the lowest rate a Limit may set: one request per 1,000
	// seconds.
	MinRate = 0.001
	// MaxRate is the highest: one request per nanosecond.
	MaxRate = 1e9
	// MaxBurst is the largest burst.
	MaxBurst = 1_000_000
)

// Limit is a token bucket's settings. The zero Limit is no limit.
type Limit struct {
	// Rate is how many requests per second the bucket refills by. Zero
	// turns limiting off.
	Rate float64
	// Burst is how many requests the bucket holds when full, and so how
	// many may arrive at once.
	Burst int
}

// Enabled reports whether l limits anything.
func (l Limit) Enabled() bool { return l.Rate > 0 }

// Validate reports a Limit that is enabled but out of bounds.
func (l Limit) Validate() error {
	if l.Rate == 0 {
		return nil
	}
	if math.IsNaN(l.Rate) || l.Rate < MinRate || l.Rate > MaxRate {
		return fmt.Errorf("rate must be 0 (off) or between %g and %g per second, got %g", MinRate, MaxRate, l.Rate)
	}
	if l.Burst < 1 || l.Burst > MaxBurst {
		return fmt.Errorf("burst must be between 1 and %d, got %d", MaxBurst, l.Burst)
	}
	return nil
}

// params is a Limit in the units buckets count in: interval is the
// nanoseconds of credit one request costs, and capacity the credit of a full
// bucket. With Rate >= MinRate and Burst <= MaxBurst, capacity is at most
// 1e6 * 1e12 = 1e18, and full stays below now + 1e18, far from the int64
// limit for any uptime under two centuries.
type params struct {
	interval int64
	capacity int64
}

func (l Limit) params() params {
	iv := int64(math.Round(1e9 / l.Rate))
	if iv < 1 {
		iv = 1
	}
	return params{interval: iv, capacity: iv * int64(l.Burst)}
}

// bucket is one client's token bucket, held as a single number so it can be
// updated with compare-and-swap and never needs a lock of its own: full is
// the time, in nanoseconds since the table's epoch, at which the bucket
// will have refilled to capacity. It is the lazily refilled token bucket
// with its two fields, the token count and the time of the last refill,
// folded into one. At any time now
//
//	tokens(now) = capacity - max(0, full - now)
//
// and spending one request's interval moves full forward by that
// interval. The refill needs no timer: it is the passage of now towards
// full. (This form is also known as GCRA, the generic cell rate
// algorithm.) A bucket whose full has passed is the same as no bucket.
type bucket struct {
	full atomic.Int64
}

// newBucket returns a bucket that is full at now.
func newBucket(now int64) *bucket {
	b := &bucket{}
	b.full.Store(now)
	return b
}

// take spends one request's credit as of now if the bucket holds enough.
// Otherwise it returns how long until it will. It is safe for concurrent
// use: racing takes retry until each has seen the other's spend, so a
// bucket never grants more than its tokens.
func (b *bucket) take(now int64, p params) (ok bool, wait time.Duration) {
	for {
		f := b.full.Load()
		// debt is the credit missing from a full bucket.
		debt := max(f-now, 0)
		if over := debt + p.interval - p.capacity; over > 0 {
			// Also right after a limit shrank: credit spent under the
			// old, larger capacity is still owed.
			return false, time.Duration(over)
		}
		if b.full.CompareAndSwap(f, now+debt+p.interval) {
			return true, 0
		}
	}
}

// tokens returns the credit held at now, in nanoseconds.
func (b *bucket) tokens(now int64, p params) int64 {
	return p.capacity - max(b.full.Load()-now, 0)
}

// fullAt reports whether b has refilled to capacity by now.
func (b *bucket) fullAt(now int64) bool { return b.full.Load() <= now }

// RetryAfter rounds a wait up to whole seconds, at least 1, for the
// Retry-After header.
func RetryAfter(wait time.Duration) int {
	s := int((wait + time.Second - 1) / time.Second)
	return max(s, 1)
}

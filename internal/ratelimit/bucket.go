package ratelimit

import (
	"fmt"
	"math"
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
// bucket. With Rate <= MaxRate and Burst <= MaxBurst, capacity is at most
// 1e6 * 1e12 = 1e18, below the int64 limit.
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

// bucket is one client's token bucket: tokens is the credit held, in
// nanoseconds, as of last, which is nanoseconds since the table's epoch.
// The capacity belongs to the route, not the bucket, which keeps a table
// slot at 40 bytes.
type bucket struct {
	tokens int64
	last   int64
}

// fullBucket returns a bucket that is full at now.
func fullBucket(now int64, p params) bucket {
	return bucket{tokens: p.capacity, last: now}
}

// take refills b up to now and spends one request's credit if it holds
// enough. Otherwise it returns how long until it will.
func (b *bucket) take(now int64, p params) (ok bool, wait time.Duration) {
	b.refill(now, p.capacity)
	if b.tokens >= p.interval {
		b.tokens -= p.interval
		return true, 0
	}
	return false, time.Duration(p.interval - b.tokens)
}

// refill adds the credit earned since last, up to capacity. A clock that
// goes backwards earns nothing; last never moves back.
func (b *bucket) refill(now, capacity int64) {
	if elapsed := now - b.last; elapsed > 0 {
		// Compare before adding: tokens + elapsed could overflow after a
		// long idle spell.
		if elapsed >= capacity-b.tokens {
			b.tokens = capacity
		} else {
			b.tokens += elapsed
		}
		b.last = now
	}
	if b.tokens > capacity {
		// The limit shrank since the last use.
		b.tokens = capacity
	}
}

// fullAt reports whether b has refilled to capacity by now. Such a bucket
// is the same as a missing one.
func (b *bucket) fullAt(now, capacity int64) bool {
	elapsed := now - b.last
	return elapsed >= 0 && elapsed >= capacity-b.tokens
}

// RetryAfter rounds a wait up to whole seconds, at least 1, for the
// Retry-After header.
func RetryAfter(wait time.Duration) int {
	s := int((wait + time.Second - 1) / time.Second)
	return max(s, 1)
}

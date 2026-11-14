package ratelimit

import "time"

// Clock tells the time. Tests inject one to step time by hand.
type Clock interface {
	Now() time.Time
}

// SystemClock is the wall clock. Its times carry the monotonic reading, so
// the elapsed time between two of them is immune to wall-clock jumps.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

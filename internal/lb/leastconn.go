package lb

import "sync/atomic"

// LeastConnBalancer picks the backend with the fewest requests in flight.
// Ties go to the first tied backend at or after a rotating start position,
// so idle backends are used in turn rather than always the first one.
//
// The pick and the caller's Acquire are not one atomic step: two goroutines
// picking at the same moment can both choose the same backend. The error is
// at most the number of simultaneous picks and corrects itself on the next
// pick; a lock around pick-and-acquire would serialise every request in the
// pool to remove it.
type LeastConnBalancer struct {
	next atomic.Uint64
}

// Pick returns a backend with the minimum in-flight count.
func (lc *LeastConnBalancer) Pick(_ *RequestInfo, healthy []*Backend) *Backend {
	n := len(healthy)
	if n == 0 {
		return nil
	}
	start := int((lc.next.Add(1) - 1) % uint64(n))
	best := healthy[start]
	bestN := best.InFlight()
	for i := 1; i < n && bestN > 0; i++ {
		b := healthy[(start+i)%n]
		if c := b.InFlight(); c < bestN {
			best, bestN = b, c
		}
	}
	return best
}

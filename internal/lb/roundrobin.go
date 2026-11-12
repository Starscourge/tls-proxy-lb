package lb

import "sync/atomic"

// RoundRobinBalancer hands out backends in turn. Every pick takes a distinct
// value from one atomic counter, so k*n picks over n backends land exactly k
// on each, however many goroutines pick at once.
type RoundRobinBalancer struct {
	next atomic.Uint64
}

// Pick returns the next backend in rotation.
func (rr *RoundRobinBalancer) Pick(_ *RequestInfo, healthy []*Backend) *Backend {
	n := uint64(len(healthy))
	if n == 0 {
		return nil
	}
	return healthy[(rr.next.Add(1)-1)%n]
}

package lb

import "sync/atomic"

// Backend is one upstream server in a pool.
//
// Its in-flight counter is shared by every connection goroutine that sends
// it a request, and read by the least-connections balancer.
type Backend struct {
	// Addr is the host:port requests are sent to.
	Addr string

	inflight atomic.Int64
}

// NewBackend returns a backend for addr with no requests in flight.
func NewBackend(addr string) *Backend { return &Backend{Addr: addr} }

// Acquire records a request sent to the backend. Every Acquire is matched by
// one Release once the response is finished or has failed.
func (b *Backend) Acquire() { b.inflight.Add(1) }

// Release records the end of a request started with Acquire.
func (b *Backend) Release() { b.inflight.Add(-1) }

// InFlight returns the number of requests currently in flight.
func (b *Backend) InFlight() int64 { return b.inflight.Load() }

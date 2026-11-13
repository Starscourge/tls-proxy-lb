package lb

import (
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// Backend is one upstream server in a pool.
//
// Conns and Health belong to the address and are shared with every other
// backend, in any pool, that has the same one. Stats belong to the backend
// in its pool; their in-flight counter is shared by every connection
// goroutine that sends the backend a request, and read by the
// least-connections balancer.
type Backend struct {
	// Addr is the host:port requests are sent to.
	Addr string
	// Conns supplies connections to Addr. NewBackend gives it a pool that
	// does not reuse connections; config.NewSnapshot replaces that with
	// the registry's pool before the snapshot is published.
	Conns *upstream.Pool
	// Health is the address's health. Nil means always healthy, which is
	// what NewBackend leaves; config.NewSnapshot sets the checker's.
	Health *health.Target
	// Stats counts the backend's requests. NewBackend gives it fresh
	// stats; config.NewSnapshot replaces them with the metrics registry's.
	Stats *metrics.BackendStats
}

// NewBackend returns a backend for addr with no requests in flight.
func NewBackend(addr string) *Backend {
	return &Backend{Addr: addr, Conns: upstream.NewPool(addr, upstream.Options{}), Stats: &metrics.BackendStats{}}
}

// Acquire records a request sent to the backend. Every Acquire is matched by
// one Release once the response is finished or has failed.
func (b *Backend) Acquire() { b.Stats.InFlight.Add(1) }

// Release records the end of a request started with Acquire.
func (b *Backend) Release() { b.Stats.InFlight.Add(-1) }

// InFlight returns the number of requests currently in flight.
func (b *Backend) InFlight() int64 { return b.Stats.InFlight.Load() }

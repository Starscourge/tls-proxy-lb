package lb

import (
	"fmt"
	"net/http"
)

// RequestInfo is what a balancer may look at when choosing a backend.
type RequestInfo struct {
	// ClientIP is the client's address without a port.
	ClientIP string
	// Header is the request's header as the client sent it.
	Header http.Header
}

// Balancer chooses a backend for a request.
//
// healthy is the set of backends that may be chosen, in the pool's order. In
// Phase 2 it is always the whole pool; Phase 5 passes only healthy backends.
// Pick returns nil if healthy is empty. Implementations are safe for
// concurrent use and do not allocate.
type Balancer interface {
	Pick(r *RequestInfo, healthy []*Backend) *Backend
}

// Algorithm names a balancing algorithm as written in the config file.
type Algorithm string

// The supported algorithms.
const (
	RoundRobin Algorithm = "round_robin"
	LeastConn  Algorithm = "least_conn"
)

// Algorithms lists the supported algorithms, for error messages.
var Algorithms = []Algorithm{RoundRobin, LeastConn}

// Pool is a named, fixed set of backends and the balancer that spreads
// requests across them. A pool does not change once built.
type Pool struct {
	Name     string
	Backends []*Backend
	Balancer Balancer
}

// PoolOptions configures NewPool.
type PoolOptions struct {
	Algorithm Algorithm
}

// NewPool builds a pool with one backend per address.
func NewPool(name string, addrs []string, opts PoolOptions) (*Pool, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("lb: pool %q has no backends", name)
	}
	backends := make([]*Backend, len(addrs))
	for i, a := range addrs {
		backends[i] = NewBackend(a)
	}
	p := &Pool{Name: name, Backends: backends}
	switch opts.Algorithm {
	case RoundRobin, "":
		p.Balancer = &RoundRobinBalancer{}
	case LeastConn:
		p.Balancer = &LeastConnBalancer{}
	default:
		return nil, fmt.Errorf("lb: pool %q: unknown algorithm %q", name, opts.Algorithm)
	}
	return p, nil
}

// Pick chooses a backend for r from the whole pool.
func (p *Pool) Pick(r *RequestInfo) *Backend { return p.Balancer.Pick(r, p.Backends) }

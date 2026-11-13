package lb

import (
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
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
// healthy is the set of backends that may be chosen, in the pool's order:
// the pool's healthy backends, less any a retry has already tried. Pick
// returns nil if healthy is empty. Implementations are safe for
// concurrent use and do not allocate.
type Balancer interface {
	Pick(r *RequestInfo, healthy []*Backend) *Backend
}

// Algorithm names a balancing algorithm as written in the config file.
type Algorithm string

// The supported algorithms.
const (
	RoundRobin     Algorithm = "round_robin"
	LeastConn      Algorithm = "least_conn"
	ConsistentHash Algorithm = "consistent_hash"
)

// Algorithms lists the supported algorithms, for error messages.
var Algorithms = []Algorithm{RoundRobin, LeastConn, ConsistentHash}

// Pool is a named, fixed set of backends and the balancer that spreads
// requests across them. A pool does not change once it is published in a
// snapshot; what moves is atomic: health, counters, and the cached healthy
// set.
type Pool struct {
	Name     string
	Backends []*Backend
	Balancer Balancer
	// Health, if set, is the checker whose targets the backends' Health
	// fields come from. Its generation tells the pool when to rebuild its
	// healthy set. Nil means every backend is always healthy.
	Health *health.Checker
	// Stats counts the pool's requests. NewPool gives it fresh stats;
	// config.NewSnapshot replaces them with the metrics registry's.
	Stats *metrics.PoolStats

	healthy atomic.Pointer[healthySet]
}

// healthySet is the pool's healthy backends as of one health generation.
type healthySet struct {
	gen      uint64
	backends []*Backend
}

// PoolOptions configures NewPool.
type PoolOptions struct {
	Algorithm Algorithm
	// HashHeader keys ConsistentHash on a request header instead of the
	// client IP. Requests without the header fall back to the client IP.
	HashHeader string
	// VirtualNodes is the number of ring points per backend for
	// ConsistentHash. Zero means DefaultVirtualNodes.
	VirtualNodes int
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
	p := &Pool{Name: name, Backends: backends, Stats: &metrics.PoolStats{}}
	switch opts.Algorithm {
	case RoundRobin, "":
		p.Balancer = &RoundRobinBalancer{}
	case LeastConn:
		p.Balancer = &LeastConnBalancer{}
	case ConsistentHash:
		p.Balancer = NewRingBalancer(backends, opts.VirtualNodes, opts.HashHeader)
	default:
		return nil, fmt.Errorf("lb: pool %q: unknown algorithm %q", name, opts.Algorithm)
	}
	return p, nil
}

// Pick chooses a backend for r from the healthy ones, or returns nil if
// none is healthy.
func (p *Pool) Pick(r *RequestInfo) *Backend { return p.Balancer.Pick(r, p.Healthy()) }

// PickExcluding chooses a backend for r from the healthy ones not in
// exclude, or returns nil if there is none. buf is scratch space for the
// candidate list, kept by the caller so a retry allocates nothing once it
// has grown.
func (p *Pool) PickExcluding(r *RequestInfo, exclude []*Backend, buf *[]*Backend) *Backend {
	cand := (*buf)[:0]
	for _, b := range p.Healthy() {
		if !slices.Contains(exclude, b) {
			cand = append(cand, b)
		}
	}
	*buf = cand
	return p.Balancer.Pick(r, cand)
}

// Healthy returns the backends that may be picked, in pool order. The list
// is rebuilt only when the checker's generation has moved since it was
// last built, so between health changes it costs two atomic loads.
//
// The generation is read before the states. If a state changes in
// between, the list may already show it but is stored under the older
// generation, so the next call rebuilds it; a list is never kept that is
// older than its generation says.
func (p *Pool) Healthy() []*Backend {
	if p.Health == nil {
		return p.Backends
	}
	gen := p.Health.Gen()
	if s := p.healthy.Load(); s != nil && s.gen == gen {
		return s.backends
	}
	list := make([]*Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if b.Health.Healthy() {
			list = append(list, b)
		}
	}
	if len(list) == len(p.Backends) {
		list = p.Backends
	}
	p.healthy.Store(&healthySet{gen: gen, backends: list})
	return list
}

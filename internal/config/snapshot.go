package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/lb"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// Snapshot is the runtime form of one Config: pools built, routes indexed
// by host and certificates loaded. It never changes after NewSnapshot returns, so any number
// of goroutines may read it without locks. A request loads the current
// snapshot once and uses it to the end; a reload (Phase 7) builds a new
// snapshot and swaps the Store's pointer, and the old one lives on until
// its last request finishes.
//
// The pools' own atomic state (in-flight counters, rotation counters) is
// the only part that moves, and it belongs to the pools, not the snapshot.
type Snapshot struct {
	// Listeners, Timeouts, Limits, Upstream, Health and Admin are copied
	// from the Config. Callers must not modify Listeners.
	Listeners []Listener
	Timeouts  Timeouts
	Limits    Limits
	Upstream  Upstream
	Health    health.Options
	Admin     Admin
	// Certs are the route and default certificates, read by every TLS
	// handshake through tlsx.ServerConfig.
	Certs *tlsx.CertSet

	pools  map[string]*lb.Pool
	routes map[string]*lb.Pool
	def    *lb.Pool
}

// Runtime is the process-wide state a snapshot's pools are wired to. It
// outlives snapshots: a snapshot built after a reload gets the same
// connection pools, health targets and counters for the same backends.
// Any field may be nil, as for -check-config.
type Runtime struct {
	// Conns supplies each backend's connection pool, by address. Nil
	// leaves every backend dialling a new connection per request.
	Conns *upstream.Registry
	// Health supplies each backend's health target, by address. Nil
	// leaves every backend always healthy.
	Health *health.Checker
	// Metrics supplies each pool's and backend's stats. Nil leaves them
	// counting into stats no one reads.
	Metrics *metrics.Metrics
}

// NewSnapshot builds the pools and route table for c and loads its
// certificates. c must come from Parse or Load. Each pool and backend is
// wired to rt. Certificate problems (a
// file that does not load, a key that does not match, a certificate not
// valid for its route's host) are reported as FieldErrors, all of them
// joined; the other checks only catch a hand-built Config that names a pool
// it does not define.
func NewSnapshot(c *Config, rt Runtime) (*Snapshot, error) {
	if err := c.Timeouts.Validate(); err != nil {
		return nil, err
	}
	s := &Snapshot{
		Listeners: append([]Listener(nil), c.Listeners...),
		Timeouts:  c.Timeouts,
		Limits:    c.Limits,
		Upstream:  c.Upstream,
		Health:    c.Health,
		Admin:     c.Admin,
		pools:     make(map[string]*lb.Pool, len(c.Pools)),
		routes:    make(map[string]*lb.Pool, len(c.Routes)),
	}
	for _, pc := range c.Pools {
		p, err := lb.NewPool(pc.Name, pc.Backends, lb.PoolOptions{Algorithm: pc.Algorithm, HashHeader: pc.HashHeader})
		if err != nil {
			return nil, err
		}
		if rt.Metrics != nil {
			p.Stats = rt.Metrics.Pool(p.Name)
		}
		p.Health = rt.Health
		for _, b := range p.Backends {
			if rt.Conns != nil {
				b.Conns = rt.Conns.Pool(b.Addr)
			}
			if rt.Health != nil {
				b.Health = rt.Health.Target(b.Addr)
			}
			if rt.Metrics != nil {
				b.Stats = rt.Metrics.Backend(p.Name, b.Addr)
			}
		}
		s.pools[pc.Name] = p
	}
	for _, r := range c.Routes {
		p, ok := s.pools[r.Pool]
		if !ok {
			return nil, fmt.Errorf("config: route %q names unknown pool %q", r.Host, r.Pool)
		}
		s.routes[NormaliseHost(r.Host)] = p
	}
	if c.DefaultPool != "" {
		p, ok := s.pools[c.DefaultPool]
		if !ok {
			return nil, fmt.Errorf("config: default_pool names unknown pool %q", c.DefaultPool)
		}
		s.def = p
	}
	certs, err := loadCerts(c)
	if err != nil {
		return nil, err
	}
	s.Certs = certs
	return s, nil
}

// loadCerts reads every route certificate and the default one.
func loadCerts(c *Config) (*tlsx.CertSet, error) {
	var errs []error
	fail := func(field string, err error) {
		errs = append(errs, &FieldError{File: c.Source, Field: field, Msg: err.Error()})
	}
	byHost := make(map[string]*tls.Certificate)
	for i, r := range c.Routes {
		if r.Cert == "" {
			continue
		}
		host := NormaliseHost(r.Host)
		pair, err := tlsx.LoadPair(r.Cert, r.Key, host)
		if err != nil {
			fail(fmt.Sprintf("route[%d].cert", i), err)
			continue
		}
		byHost[host] = pair
	}
	var def *tls.Certificate
	if c.TLS.DefaultCert != "" {
		pair, err := tlsx.LoadPair(c.TLS.DefaultCert, c.TLS.DefaultKey, "")
		if err != nil {
			fail("tls.default_cert", err)
		}
		def = pair
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return tlsx.NewCertSet(byHost, def), nil
}

// Route returns the pool for a request's Host header value: the route for
// its normalised host, else the default pool, else nil.
func (s *Snapshot) Route(host string) *lb.Pool {
	if p, ok := s.routes[NormaliseHost(host)]; ok {
		return p
	}
	return s.def
}

// HasRoute reports whether host has a route of its own, not counting the
// default pool.
func (s *Snapshot) HasRoute(host string) bool {
	_, ok := s.routes[NormaliseHost(host)]
	return ok
}

// Pools returns every pool, sorted by name.
func (s *Snapshot) Pools() []*lb.Pool {
	out := make([]*lb.Pool, 0, len(s.pools))
	for _, p := range s.pools {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b *lb.Pool) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Pool returns the pool with the given name, or nil.
func (s *Snapshot) Pool(name string) *lb.Pool { return s.pools[name] }

// NormaliseHost puts a Host header value in the form routes are stored in:
// the port removed, IPv6 brackets removed, a trailing dot removed, and ASCII
// letters lower-cased. It allocates only when the host has upper-case
// letters.
func NormaliseHost(host string) string {
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i > 0 {
			host = host[1:i]
		}
	} else if i := strings.IndexByte(host, ':'); i >= 0 && strings.IndexByte(host[i+1:], ':') < 0 {
		// Exactly one colon: host:port. More than one is a bare IPv6
		// address, which has no port.
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	for i := 0; i < len(host); i++ {
		if c := host[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(host)
		}
	}
	return host
}

// Store holds the current snapshot. Readers call Load once per request;
// Store replaces the snapshot for every later Load.
type Store struct {
	p atomic.Pointer[Snapshot]
}

// NewStore returns a store holding s.
func NewStore(s *Snapshot) *Store {
	st := &Store{}
	st.p.Store(s)
	return st
}

// Load returns the current snapshot.
func (st *Store) Load() *Snapshot { return st.p.Load() }

// Store makes s the current snapshot.
func (st *Store) Store(s *Snapshot) { st.p.Store(s) }

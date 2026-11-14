// Package metrics holds the proxy's counters and exposes them, with the
// health and connection-pool state, in Prometheus format.
//
// The request path never calls into the Prometheus client except to
// observe latency. Every other count is a plain atomic on a stats value
// the request already holds (its pool's PoolStats, its backend's
// BackendStats), and one collector reads them all at scrape time. A request
// costs a few atomic adds and no allocation, and label lookups happen once
// per scrape, not once per request.
//
// Stats are keyed by pool name and backend address and live as long as the
// Metrics, so a config snapshot rebuilt for the same pools (Phase 7's
// reload) gets the same counters and they never go backwards.
package metrics

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// Status codes 100 to 599 are counted one by one; anything else a backend
// sends is counted as "other".
const (
	minCode = 100
	maxCode = 599
)

// Codes counts responses by status code. The zero value is ready to use.
type Codes struct {
	n [maxCode - minCode + 2]atomic.Uint64 // [0] is "other"
}

// Add counts one response with status code.
func (c *Codes) Add(code int) {
	i := 0
	if code >= minCode && code <= maxCode {
		i = code - minCode + 1
	}
	c.n[i].Add(1)
}

// Get returns the count for code.
func (c *Codes) Get(code int) uint64 {
	if code < minCode || code > maxCode {
		return c.n[0].Load()
	}
	return c.n[code-minCode+1].Load()
}

// each calls fn for every code counted at least once.
func (c *Codes) each(fn func(code string, n uint64)) {
	for i := range c.n {
		n := c.n[i].Load()
		if n == 0 {
			continue
		}
		if i == 0 {
			fn("other", n)
		} else {
			fn(strconv.Itoa(i-1+minCode), n)
		}
	}
}

// BackendStats are the counters of one backend in one pool.
type BackendStats struct {
	// InFlight is the number of requests sent to the backend whose
	// responses are not finished. Least-connections balances on it.
	InFlight atomic.Int64
	// Codes counts the responses the backend's requests ended with.
	Codes Codes
}

// RetryReason is why a request was sent again.
type RetryReason int

// The retry reasons.
const (
	// RetryStale: a reused connection failed before any response, and the
	// request went again to the same backend on a new connection.
	RetryStale RetryReason = iota
	// RetryDial: the dial failed; the request went to another backend.
	RetryDial
	// RetryError: the connection failed before a response; the request
	// went to another backend.
	RetryError
	// RetryTimeout: no response within the upstream response timeout; the
	// request went to another backend.
	RetryTimeout
	numRetryReasons
)

var retryReasonNames = [numRetryReasons]string{"stale_conn", "dial", "error", "timeout"}

func (r RetryReason) String() string { return retryReasonNames[r] }

// PoolStats are the counters of one pool.
type PoolStats struct {
	// Latency observes each request's total time, from its first byte to
	// the end of its response. Nil observes nothing.
	Latency prometheus.Observer
	// NoBackend counts responses to requests routed to the pool that no
	// backend answered: 503 when none was healthy, and the like.
	NoBackend Codes
	retries   [numRetryReasons]atomic.Uint64
}

// Retry counts one retry for reason.
func (p *PoolStats) Retry(reason RetryReason) { p.retries[reason].Add(1) }

// Retries returns the count of retries for reason.
func (p *PoolStats) Retries(reason RetryReason) uint64 { return p.retries[reason].Load() }

// Sources are the state the collector reads besides the stats Metrics
// holds. Each may be nil.
type Sources struct {
	Upstream *upstream.Registry
	Health   *health.Checker
	// ClientConns returns the number of open client connections.
	ClientConns func() int
	// ClientConnsRejected returns how many client connections were
	// refused at accept for the reason named ("max_conns", "per_ip").
	ClientConnsRejected func(reason string) uint64
	// RateLimit is the table of client token buckets.
	RateLimit *ratelimit.Table
}

// RejectReasons are the reasons ClientConnsRejected is asked about.
var RejectReasons = []string{"max_conns", "per_ip"}

// LatencyBuckets are the request latency histogram's bucket bounds in
// seconds: from half a millisecond, for a backend on the same host, to
// ten seconds, the default upstream response timeout.
var LatencyBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Metrics is the registry of the proxy's metrics.
type Metrics struct {
	reg     *prometheus.Registry
	latency *prometheus.HistogramVec
	src     Sources

	unrouted Codes

	badRequests [httpx.NumReasons]atomic.Uint64
	panics      atomic.Uint64
	acceptErrs  atomic.Uint64

	// reloads counts config reloads by outcome: [0] failed, [1] applied.
	reloads      [2]atomic.Uint64
	lastReloadOK atomic.Bool

	mu          sync.Mutex
	pools       map[string]*poolEntry
	rateLimited map[string]*atomic.Uint64
}

type poolEntry struct {
	stats    *PoolStats
	backends map[string]*BackendStats
}

// cpuClasses enables the runtime's CPU accounting (/cpu/classes/...) on top
// of the Go collector's defaults: go_cpu_classes_gc_total_cpu_seconds_total
// over go_cpu_classes_total_cpu_seconds_total is the garbage collector's
// share of the process's CPU, which the Phase 9 benchmarks report.
var cpuClasses = collectors.GoRuntimeMetricsRule{Matcher: regexp.MustCompile(`^/cpu/classes/`)}

// New returns a Metrics whose registry holds the proxy's metrics and the
// Go runtime and process collectors.
func New(src Sources) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "proxy_request_duration_seconds",
			Help:    "Time from a request's first byte to the end of its response, by pool.",
			Buckets: LatencyBuckets,
		}, []string{"pool"}),
		src:         src,
		pools:       make(map[string]*poolEntry),
		rateLimited: make(map[string]*atomic.Uint64),
	}
	m.lastReloadOK.Store(true)
	m.reg.MustRegister(
		collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(cpuClasses)),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.latency,
		collector{m},
	)
	return m
}

// Handler serves the metrics in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Gatherer returns the registry, for tests.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// Pool returns the stats of the named pool, creating them on first use. It
// is called while building a snapshot, not per request.
func (m *Metrics) Pool(name string) *PoolStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entry(name).stats
}

// Backend returns the stats of the backend addr in the named pool,
// creating them on first use.
func (m *Metrics) Backend(pool, addr string) *BackendStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(pool)
	b, ok := e.backends[addr]
	if !ok {
		b = &BackendStats{}
		e.backends[addr] = b
	}
	return b
}

// RateLimited returns the counter of requests to route refused with 429,
// creating it on first use. route is the route's host, "" for the default
// pool. It is called while building a snapshot, not per request.
func (m *Metrics) RateLimited(route string) *atomic.Uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.rateLimited[route]
	if !ok {
		c = new(atomic.Uint64)
		m.rateLimited[route] = c
	}
	return c
}

// BadRequest counts a request refused for r. A nil Metrics counts
// nothing.
func (m *Metrics) BadRequest(r httpx.Reason) {
	if m != nil && r < httpx.NumReasons {
		m.badRequests[r].Add(1)
	}
}

// BadRequests returns how many requests were refused for r.
func (m *Metrics) BadRequests(r httpx.Reason) uint64 { return m.badRequests[r].Load() }

// Panic counts a panic recovered in a connection goroutine. A nil Metrics
// counts nothing.
func (m *Metrics) Panic() {
	if m != nil {
		m.panics.Add(1)
	}
}

// Panics returns how many panics were recovered.
func (m *Metrics) Panics() uint64 { return m.panics.Load() }

// AcceptError counts an Accept that failed with an error other than a
// closed listener. A nil Metrics counts nothing.
func (m *Metrics) AcceptError() {
	if m != nil {
		m.acceptErrs.Add(1)
	}
}

// AcceptErrors returns how many Accepts failed.
func (m *Metrics) AcceptErrors() uint64 { return m.acceptErrs.Load() }

// Reload records the outcome of a config reload.
func (m *Metrics) Reload(ok bool) {
	i := 0
	if ok {
		i = 1
	}
	m.reloads[i].Add(1)
	m.lastReloadOK.Store(ok)
}

// Reloads returns how many config reloads were applied (ok) or failed.
func (m *Metrics) Reloads(ok bool) uint64 {
	if ok {
		return m.reloads[1].Load()
	}
	return m.reloads[0].Load()
}

// Retain drops the stats of every pool not in pools, of every backend not
// listed under its pool, and of every route not in routes ("" being the
// default pool's), after a reload removed them. Their series leave
// /metrics, as their health and upstream series do. A request still in
// flight on the old config counts into the dropped stats, and a pool,
// backend or route that comes back starts from zero.
func (m *Metrics) Retain(pools map[string][]string, routes []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, e := range m.pools {
		addrs, ok := pools[name]
		if !ok {
			delete(m.pools, name)
			m.latency.DeleteLabelValues(name)
			continue
		}
		for addr := range e.backends {
			if !slices.Contains(addrs, addr) {
				delete(e.backends, addr)
			}
		}
	}
	for route := range m.rateLimited {
		if !slices.Contains(routes, route) {
			delete(m.rateLimited, route)
		}
	}
}

// Unrouted counts responses to requests that reached no pool: 400, 404,
// 408, 421, 431 and the like. A nil Metrics returns nil.
func (m *Metrics) Unrouted() *Codes {
	if m == nil {
		return nil
	}
	return &m.unrouted
}

// entry returns the pool's entry, creating it; m.mu must be held.
func (m *Metrics) entry(name string) *poolEntry {
	e, ok := m.pools[name]
	if !ok {
		e = &poolEntry{
			stats:    &PoolStats{Latency: m.latency.WithLabelValues(name)},
			backends: make(map[string]*BackendStats),
		}
		m.pools[name] = e
	}
	return e
}

// Descriptions of the collected metrics.
var (
	descRequests = prometheus.NewDesc("proxy_requests_total",
		"Requests answered, by pool, backend and status code. Backend is empty when no backend answered; pool and backend are empty when the request reached no pool.",
		[]string{"pool", "backend", "code"}, nil)
	descInFlight = prometheus.NewDesc("proxy_requests_in_flight",
		"Requests sent to a backend whose responses are not finished.",
		[]string{"pool", "backend"}, nil)
	descRetries = prometheus.NewDesc("proxy_retries_total",
		"Requests sent again: on a new connection to the same backend (stale_conn) or to another backend (dial, error, timeout).",
		[]string{"pool", "reason"}, nil)
	descUpstreamConns = prometheus.NewDesc("proxy_upstream_connections_total",
		"Upstream connections handed to requests: newly dialled (failed dials included) or reused from the idle pool.",
		[]string{"backend", "kind"}, nil)
	descUpstreamIdle = prometheus.NewDesc("proxy_upstream_idle_connections",
		"Idle upstream connections kept for reuse.",
		[]string{"backend"}, nil)
	descHealthy = prometheus.NewDesc("proxy_backend_healthy",
		"1 if the backend is in rotation, 0 if it is unhealthy or ejected.",
		[]string{"backend"}, nil)
	descState = prometheus.NewDesc("proxy_backend_state",
		"1 for the backend's current health state, 0 for the others.",
		[]string{"backend", "state"}, nil)
	descTransitions = prometheus.NewDesc("proxy_backend_transitions_total",
		"Health state changes, by the state entered.",
		[]string{"backend", "to"}, nil)
	descClientConns = prometheus.NewDesc("proxy_client_connections",
		"Open client connections.", nil, nil)
	descClientConnsRejected = prometheus.NewDesc("proxy_client_connections_rejected_total",
		"Client connections closed at accept: at the total cap (max_conns) or at the client address's cap (per_ip).",
		[]string{"reason"}, nil)
	descRateLimited = prometheus.NewDesc("proxy_rate_limited_requests_total",
		"Requests refused with 429 by the per-client rate limit, by route host; empty for the default pool.",
		[]string{"route"}, nil)
	descRateLimitEntries = prometheus.NewDesc("proxy_rate_limit_buckets",
		"Client token buckets held in the rate-limit table.", nil, nil)
	descRateLimitCap = prometheus.NewDesc("proxy_rate_limit_buckets_max",
		"The most client token buckets the rate-limit table holds.", nil, nil)
	descRateLimitEvictions = prometheus.NewDesc("proxy_rate_limit_evictions_total",
		"Buckets dropped before they had refilled, to keep the table under its cap.", nil, nil)
	descReloads = prometheus.NewDesc("proxy_config_reloads_total",
		"Config reloads on SIGHUP, by result: success (applied) or failure (the old config kept serving).",
		[]string{"result"}, nil)
	descBadRequests = prometheus.NewDesc("proxy_bad_requests_total",
		"Requests refused before forwarding because their framing or headers were ambiguous, malformed or over a limit, by reason.",
		[]string{"reason"}, nil)
	descPanics = prometheus.NewDesc("proxy_panics_total",
		"Panics recovered in a connection goroutine; each closed its connection and left the process running.", nil, nil)
	descAcceptErrors = prometheus.NewDesc("proxy_accept_errors_total",
		"Accepts that failed, out of file descriptors for example, each followed by a backoff.", nil, nil)
	descLastReloadOK = prometheus.NewDesc("proxy_config_last_reload_successful",
		"1 if the last config reload was applied, or none has been tried; 0 if it failed.", nil, nil)
)

// collector reads Metrics' stats and its Sources at scrape time. It is
// unchecked (Describe sends nothing), because its label values are only
// known then.
type collector struct{ m *Metrics }

func (collector) Describe(chan<- *prometheus.Desc) {}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	m := c.m
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}

	m.unrouted.each(func(code string, n uint64) { counter(descRequests, n, "", "", code) })
	counter(descReloads, m.reloads[1].Load(), "success")
	counter(descReloads, m.reloads[0].Load(), "failure")
	lastOK := 0.0
	if m.lastReloadOK.Load() {
		lastOK = 1
	}
	gauge(descLastReloadOK, lastOK)
	for r := httpx.ReasonNone + 1; r < httpx.NumReasons; r++ {
		counter(descBadRequests, m.badRequests[r].Load(), r.String())
	}
	counter(descPanics, m.panics.Load())
	counter(descAcceptErrors, m.acceptErrs.Load())
	m.mu.Lock()
	names := make([]string, 0, len(m.pools))
	for name := range m.pools {
		names = append(names, name)
	}
	slices.Sort(names)
	type backendStat struct {
		addr string
		s    *BackendStats
	}
	type poolStat struct {
		name     string
		s        *PoolStats
		backends []backendStat
	}
	pools := make([]poolStat, 0, len(names))
	for _, name := range names {
		e := m.pools[name]
		ps := poolStat{name: name, s: e.stats}
		for addr, b := range e.backends {
			ps.backends = append(ps.backends, backendStat{addr, b})
		}
		pools = append(pools, ps)
	}
	type routeStat struct {
		route string
		n     *atomic.Uint64
	}
	limited := make([]routeStat, 0, len(m.rateLimited))
	for route, n := range m.rateLimited {
		limited = append(limited, routeStat{route, n})
	}
	m.mu.Unlock()
	for _, r := range limited {
		counter(descRateLimited, r.n.Load(), r.route)
	}

	for _, p := range pools {
		p.s.NoBackend.each(func(code string, n uint64) { counter(descRequests, n, p.name, "", code) })
		for r := range numRetryReasons {
			counter(descRetries, p.s.retries[r].Load(), p.name, r.String())
		}
		for _, b := range p.backends {
			b.s.Codes.each(func(code string, n uint64) { counter(descRequests, n, p.name, b.addr, code) })
			gauge(descInFlight, float64(b.s.InFlight.Load()), p.name, b.addr)
		}
	}
	if r := m.src.Upstream; r != nil {
		for _, p := range r.Pools() {
			counter(descUpstreamConns, p.Dials(), p.Addr(), "dial")
			counter(descUpstreamConns, p.Reuses(), p.Addr(), "reuse")
			gauge(descUpstreamIdle, float64(p.Idle()), p.Addr())
		}
	}
	if h := m.src.Health; h != nil {
		for _, t := range h.Targets() {
			st := t.State()
			healthy := 0.0
			if st == health.Healthy {
				healthy = 1
			}
			gauge(descHealthy, healthy, t.Addr())
			for _, s := range health.States {
				v := 0.0
				if s == st {
					v = 1
				}
				gauge(descState, v, t.Addr(), s.String())
				counter(descTransitions, t.Transitions(s), t.Addr(), s.String())
			}
		}
	}
	if f := m.src.ClientConns; f != nil {
		gauge(descClientConns, float64(f()))
	}
	if f := m.src.ClientConnsRejected; f != nil {
		for _, r := range RejectReasons {
			counter(descClientConnsRejected, f(r), r)
		}
	}
	if t := m.src.RateLimit; t != nil {
		gauge(descRateLimitEntries, float64(t.Len()))
		gauge(descRateLimitCap, float64(t.MaxEntries()))
		counter(descRateLimitEvictions, t.Evictions())
	}
}

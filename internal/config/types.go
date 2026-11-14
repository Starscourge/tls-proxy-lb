package config

import (
	"fmt"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
	"github.com/Starscourge/tls-proxy-lb/internal/lb"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// Config is a parsed and validated config file, with defaults filled in.
// Parse and Load produce it; NewSnapshot turns it into the runtime form.
type Config struct {
	// Source is the name the file was parsed under, for error messages.
	Source string
	// Listeners are where client connections are accepted, in file order.
	Listeners []Listener
	// Routes map a request's host to a pool, in file order.
	Routes []Route
	// DefaultPool serves hosts no route names. Empty means they get 404.
	DefaultPool string
	// Pools are sorted by name.
	Pools []Pool
	// TrustedProxies are the peers whose X-Forwarded-For names the client,
	// and who are exempt from the per-IP connection cap. Empty trusts no
	// one.
	TrustedProxies ratelimit.Trusted
	TLS            TLS
	Timeouts       Timeouts
	Limits         Limits
	RateLimit      RateLimit
	Upstream       Upstream
	Health         health.Options
	Admin          Admin
	Log            Log
}

// Listener modes.
const (
	// ModeHTTPS terminates TLS and proxies. It is the default.
	ModeHTTPS = "https"
	// ModeHTTP proxies plain HTTP.
	ModeHTTP = "http"
	// ModeRedirect answers every request with a redirect to HTTPS.
	ModeRedirect = "redirect"
)

// Modes lists the listener modes, default first.
var Modes = []string{ModeHTTPS, ModeHTTP, ModeRedirect}

// Listener is one address to accept client connections on.
type Listener struct {
	// Address is host:port. Port 0 picks a free port, which tests use.
	Address string
	// Mode is ModeHTTPS, ModeHTTP or ModeRedirect.
	Mode string
	// RedirectPort, for ModeRedirect only, is the port put in the HTTPS
	// URL. 443 is left out of the URL.
	RedirectPort int
}

// TLS holds the settings shared by every HTTPS listener.
type TLS struct {
	// DefaultCert and DefaultKey, if set, are served to clients whose SNI
	// names no route with a certificate, and to clients that send no SNI.
	// Without them those handshakes fail with unrecognized_name. Paths are
	// resolved like route certificates.
	DefaultCert, DefaultKey string
}

// Route sends requests for one host to one pool.
type Route struct {
	// Host is normalised: lower case, no port, no trailing dot, IPv6
	// literals without brackets.
	Host string
	Pool string
	// Cert and Key are the PEM certificate chain and key files for Host,
	// both set or both empty. A relative path is resolved against the
	// directory of the config file.
	Cert, Key string
	// RateLimit, if set, replaces [rate_limit]'s default for this route.
	// A zero Rate turns limiting off here.
	RateLimit *ratelimit.Limit
}

// Pool is a named set of backends and how to balance across them.
type Pool struct {
	Name      string
	Algorithm lb.Algorithm
	// HashHeader, for ConsistentHash only, keys the ring on this header
	// instead of the client IP. It is in canonical form.
	HashHeader string
	// Backends are host:port addresses.
	Backends []string
}

// Timeouts bounds every blocking step of a client connection.
type Timeouts struct {
	// Handshake bounds the TLS handshake on a new HTTPS connection.
	Handshake time.Duration
	// HeaderRead covers a whole request header block from its first byte,
	// and the wait for the first request on a new connection.
	HeaderRead time.Duration
	// Idle is how long a keep-alive connection may wait for its next request.
	Idle time.Duration
	// ClientIO is the longest stall allowed while reading a request body
	// from the client or writing a response to it. It restarts whenever
	// bytes move.
	ClientIO time.Duration
	// UpstreamConnect bounds the dial to a backend.
	UpstreamConnect time.Duration
	// UpstreamResponse bounds the wait for response headers after the last
	// request byte is sent, and any stall while writing the request to or
	// reading the response from the backend.
	UpstreamResponse time.Duration
	// BodyGrace is how long a request body may take before
	// Limits.MinBodyRate applies to it.
	BodyGrace time.Duration
	// Shutdown is how long open connections may take to finish after
	// SIGTERM before the rest are closed. Zero closes them at once. The
	// value in force is the current snapshot's when the signal arrives.
	Shutdown time.Duration
}

// DefaultTimeouts returns the defaults from PLAN.md's config sketch, with
// ClientIO matching nginx's client_body_timeout and send_timeout,
// Handshake equal to HeaderRead, and Shutdown short enough that the drain
// and teardown finish inside docker stop's 10-second grace period.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Handshake:        5 * time.Second,
		HeaderRead:       5 * time.Second,
		Idle:             60 * time.Second,
		ClientIO:         60 * time.Second,
		UpstreamConnect:  1 * time.Second,
		UpstreamResponse: 10 * time.Second,
		BodyGrace:        10 * time.Second,
		Shutdown:         8 * time.Second,
	}
}

// Validate reports the first timeout that is not positive, or a negative
// Shutdown.
func (t Timeouts) Validate() error {
	if t.Shutdown < 0 {
		return fmt.Errorf("timeout Shutdown must not be negative, got %v", t.Shutdown)
	}
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"Handshake", t.Handshake},
		{"HeaderRead", t.HeaderRead},
		{"Idle", t.Idle},
		{"ClientIO", t.ClientIO},
		{"UpstreamConnect", t.UpstreamConnect},
		{"UpstreamResponse", t.UpstreamResponse},
		{"BodyGrace", t.BodyGrace},
	} {
		if f.d <= 0 {
			return fmt.Errorf("timeout %s must be positive, got %v", f.name, f.d)
		}
	}
	return nil
}

// Limits caps resources per process and per request.
type Limits struct {
	// MaxConns is the most client connections open at once; more are
	// closed at accept. A reload applies it to later accepts.
	MaxConns int
	// MaxHeaderBytes caps a request's line plus headers; larger requests
	// get 431.
	MaxHeaderBytes int
	// MaxHeaderCount caps the header fields in a request (431). Zero is no
	// cap.
	MaxHeaderCount int64
	// MaxRequestLine caps the request line in bytes (414). Zero is no cap
	// beyond MaxHeaderBytes.
	MaxRequestLine int64
	// MaxBodyBytes caps a request body (413): a Content-Length over it is
	// refused before anything is sent upstream, and a chunked body is cut
	// off when it passes it. Zero, the default, is no cap: bodies stream
	// through fixed buffers, so their size costs the proxy nothing; set it
	// for backends that read bodies whole.
	MaxBodyBytes int64
	// MaxRequestsPerConn is how many requests one client connection may
	// carry; the last response says Connection: close. Zero is no cap.
	MaxRequestsPerConn int64
	// MinBodyRate is the slowest a request body may arrive, in bytes per
	// second averaged from its start, once Timeouts.BodyGrace has passed
	// (408). Zero turns it off.
	MinBodyRate int64
	// MaxConnsPerIP is the most client connections open at once from one
	// address (an IPv6 /64), checked at accept before the TLS handshake.
	// Trusted proxies are exempt. Zero means no cap. Like MaxConns, a
	// reload applies it to later accepts.
	MaxConnsPerIP int64
}

// DefaultLimits returns the limits used when the file sets none. The
// header count follows Apache's LimitRequestFields, the request line
// nginx's large_client_header_buffers, requests per connection nginx's
// keepalive_requests, and the body rate Apache's mod_reqtimeout MinRate.
func DefaultLimits() Limits {
	return Limits{MaxConns: 10000, MaxHeaderBytes: 64 << 10, MaxHeaderCount: 100, MaxRequestLine: 8192,
		MaxRequestsPerConn: 1000, MinBodyRate: 500}
}

// Request returns the limits httpx.ReadRequest enforces.
func (l Limits) Request() httpx.RequestLimits {
	return httpx.RequestLimits{
		RawLimits:    httpx.RawLimits{MaxHeaderCount: int(l.MaxHeaderCount), MaxRequestLine: int(l.MaxRequestLine)},
		MaxBodyBytes: l.MaxBodyBytes,
	}
}

// RateLimit configures per-client token buckets.
type RateLimit struct {
	// Default is the limit for routes that set none, and for requests
	// served by default_pool. The zero Limit is off.
	Default ratelimit.Limit
	// MaxEntries caps the number of buckets held, across all routes.
	MaxEntries int
	// Sweep is how often buckets that have refilled to full are dropped.
	Sweep time.Duration
}

// DefaultRateLimit returns the settings used when the file sets none:
// limiting off.
func DefaultRateLimit() RateLimit {
	return RateLimit{MaxEntries: ratelimit.DefaultMaxEntries, Sweep: 10 * time.Second}
}

// Upstream controls the keep-alive pool of connections to backends.
type Upstream struct {
	// MaxIdlePerBackend is the most idle connections kept open to one
	// backend address. Zero turns reuse off: every request gets a new
	// connection and asks the backend to close it.
	MaxIdlePerBackend int
	// IdleTimeout is how long an idle connection may wait for its next
	// request before it is closed. It should be shorter than the backends'
	// own keep-alive timeouts, so the proxy is usually the side to close.
	IdleTimeout time.Duration
	// MaxRetries is how many other backends a failed request may be tried
	// on, 0 to 2. See conn's retry rules for which failures qualify.
	MaxRetries int
}

// Options returns the settings in the form upstream.NewRegistry takes.
func (u Upstream) Options() upstream.Options {
	return upstream.Options{MaxIdle: u.MaxIdlePerBackend, IdleTimeout: u.IdleTimeout}
}

// DefaultUpstream returns the pool settings used when the file sets none.
func DefaultUpstream() Upstream {
	return Upstream{MaxIdlePerBackend: 100, IdleTimeout: 30 * time.Second, MaxRetries: 1}
}

// Admin configures the admin listener, which serves pprof. It applies from
// startup and is not reloaded.
type Admin struct {
	// Address is a loopback host:port. Empty means no admin listener.
	Address string
	// MutexProfileFraction is passed to runtime.SetMutexProfileFraction:
	// on average 1 in this many mutex contention events is sampled. Zero
	// leaves mutex profiling off.
	MutexProfileFraction int
	// BlockProfileRate is passed to runtime.SetBlockProfileRate: one
	// blocking event is sampled per this many nanoseconds spent blocked.
	// Zero leaves block profiling off.
	BlockProfileRate int
}

// Log controls how log lines reach stdout. It applies from startup and is
// not reloaded.
type Log struct {
	// BufferBytes is the size of the buffer log lines collect in before a
	// write to stdout. Zero writes every line as it is logged.
	BufferBytes int
	// FlushInterval is the longest a line waits in the buffer, and so the
	// most a crash can lose.
	FlushInterval time.Duration
}

// DefaultLog returns the log settings used when the file sets none.
func DefaultLog() Log {
	return Log{BufferBytes: 64 << 10, FlushInterval: time.Second}
}

// KeepStartup copies into c the settings that apply from startup and are
// not reloaded, from running, the config the process is using, and
// returns the file keys of those that c had changed. A reload calls it
// before building the new snapshot, so the snapshot holds the values in
// force, and logs the keys: they apply after a restart.
//
// Startup-only: [admin] and [log] (the admin listener and the log buffer
// are made once), [health] (the probers' settings), upstream
// max_idle_per_backend and idle_timeout_ms (the connection pools'), and
// rate_limit max_entries and sweep_ms (the bucket table's).
func (c *Config) KeepStartup(running *Config) []string {
	var changed []string
	keep := func(key string, differs bool, restore func()) {
		if differs {
			changed = append(changed, key)
			restore()
		}
	}
	keep("admin", c.Admin != running.Admin, func() { c.Admin = running.Admin })
	keep("log", c.Log != running.Log, func() { c.Log = running.Log })
	keep("health", c.Health != running.Health, func() { c.Health = running.Health })
	keep("upstream.max_idle_per_backend", c.Upstream.MaxIdlePerBackend != running.Upstream.MaxIdlePerBackend,
		func() { c.Upstream.MaxIdlePerBackend = running.Upstream.MaxIdlePerBackend })
	keep("upstream.idle_timeout_ms", c.Upstream.IdleTimeout != running.Upstream.IdleTimeout,
		func() { c.Upstream.IdleTimeout = running.Upstream.IdleTimeout })
	keep("rate_limit.max_entries", c.RateLimit.MaxEntries != running.RateLimit.MaxEntries,
		func() { c.RateLimit.MaxEntries = running.RateLimit.MaxEntries })
	keep("rate_limit.sweep_ms", c.RateLimit.Sweep != running.RateLimit.Sweep,
		func() { c.RateLimit.Sweep = running.RateLimit.Sweep })
	return changed
}

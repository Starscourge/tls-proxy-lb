package config

import (
	"fmt"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
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
	Pools    []Pool
	TLS      TLS
	Timeouts Timeouts
	Limits   Limits
	Upstream Upstream
	Admin    Admin
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
}

// DefaultTimeouts returns the defaults from PLAN.md's config sketch, with
// ClientIO matching nginx's client_body_timeout and send_timeout, and
// Handshake equal to HeaderRead.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Handshake:        5 * time.Second,
		HeaderRead:       5 * time.Second,
		Idle:             60 * time.Second,
		ClientIO:         60 * time.Second,
		UpstreamConnect:  1 * time.Second,
		UpstreamResponse: 10 * time.Second,
	}
}

// Validate reports the first timeout that is not positive.
func (t Timeouts) Validate() error {
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
	// closed at accept. It applies from startup and is not reloaded.
	MaxConns int
	// MaxHeaderBytes caps a request's line plus headers; larger requests
	// get 431.
	MaxHeaderBytes int
}

// DefaultLimits returns the limits used when the file sets none.
func DefaultLimits() Limits {
	return Limits{MaxConns: 10000, MaxHeaderBytes: 64 << 10}
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
}

// Options returns the settings in the form upstream.NewRegistry takes.
func (u Upstream) Options() upstream.Options {
	return upstream.Options{MaxIdle: u.MaxIdlePerBackend, IdleTimeout: u.IdleTimeout}
}

// DefaultUpstream returns the pool settings used when the file sets none.
func DefaultUpstream() Upstream {
	return Upstream{MaxIdlePerBackend: 100, IdleTimeout: 30 * time.Second}
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

package config

import (
	"fmt"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
)

// Config is a parsed and validated config file, with defaults filled in.
// Parse and Load produce it; NewSnapshot turns it into the runtime form.
type Config struct {
	// Listeners are the addresses to accept client connections on. Port 0
	// picks a free port, which tests use.
	Listeners []string
	// Routes map a request's host to a pool, in file order.
	Routes []Route
	// DefaultPool serves hosts no route names. Empty means they get 404.
	DefaultPool string
	// Pools are sorted by name.
	Pools    []Pool
	Timeouts Timeouts
	Limits   Limits
}

// Route sends requests for one host to one pool.
type Route struct {
	// Host is normalised: lower case, no port, no trailing dot, IPv6
	// literals without brackets.
	Host string
	Pool string
	// Cert and Key are the certificate and key files for Host. They are
	// kept but not used until Phase 3 adds TLS.
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
// ClientIO matching nginx's client_body_timeout and send_timeout.
func DefaultTimeouts() Timeouts {
	return Timeouts{
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

package conn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Timeouts bounds every blocking step of a client connection. Phase 2 reads
// them from the [timeouts] table of the config file.
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

// isTimeout reports whether err is a deadline or dial timeout.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

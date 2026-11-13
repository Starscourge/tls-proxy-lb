// Package upstream manages connections to backends: dialling them, and the
// keep-alive pool that lets one connection carry many requests.
package upstream

import (
	"bufio"
	"context"
	"net"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
)

// Conn is a connection to a backend with its buffered reader and writer.
// The reader and writer stay with the connection while it is idle in a
// Pool: bytes the reader holds belong to this connection and no other.
//
// A Conn is used by one request at a time. While a request body streams,
// its writer goroutine shares the Conn with the goroutine reading the
// response, and either may Close it. The reader and writer come from
// httpx's buffer pools and go back with Discard, which only the owner may
// call, once no other goroutine can touch the Conn.
type Conn struct {
	net.Conn
	// R reads responses. Reads have no deadline of their own; the caller
	// sets one before reading.
	R *bufio.Reader
	// W writes requests. Each write to the socket pushes both the read and
	// write deadlines ioTimeout into the future, so a response timeout counts
	// from the last request byte sent.
	W *bufio.Writer

	dw     httpx.DeadlineWriter
	cr     countingReader
	cw     countingWriter
	reused bool
	// idleSince is when the connection was parked, set by Put.
	idleSince time.Time

	// raw and peek serve the liveness check, built once per connection so
	// the check allocates nothing.
	raw      syscall.RawConn
	peek     func(fd uintptr) bool
	peekLive bool
}

// Dial connects to addr, giving up after connectTimeout or when ctx ends.
func Dial(ctx context.Context, addr string, connectTimeout, ioTimeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: connectTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Conn{Conn: nc}
	c.dw = httpx.DeadlineWriter{Conn: nc, Timeout: ioTimeout, ExtendRead: true}
	c.cr.r = nc
	c.R = httpx.NewReader(&c.cr)
	c.cw.w = &c.dw
	c.W = httpx.NewWriter(&c.cw)
	return c, nil
}

// Discard closes the connection and returns its buffers to the pool. The
// caller must own c outright: no request writer still running, no body
// still reading from it. Calling it again does nothing.
func (c *Conn) Discard() {
	if c.R == nil {
		return
	}
	c.Close()
	httpx.PutReader(c.R)
	httpx.PutWriter(c.W)
	c.R, c.W = nil, nil
}

// Reused reports whether the connection carried an earlier request.
func (c *Conn) Reused() bool { return c.reused }

// BytesRead returns how many bytes have been read from the socket since
// the connection was dialled or last taken from a pool. Zero after a failed
// request means the backend sent nothing at all in reply.
func (c *Conn) BytesRead() int64 { return c.cr.n }

// BytesWritten returns how many bytes have been written to the socket since
// the connection was dialled or last taken from a pool. Zero after a failed
// write means no part of the request left the proxy.
func (c *Conn) BytesWritten() int64 { return c.cw.n }

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w *httpx.DeadlineWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r net.Conn
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

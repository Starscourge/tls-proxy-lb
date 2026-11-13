// Package upstream manages connections to backends: dialling them, and the
// keep-alive pool that lets one connection carry many requests.
package upstream

import (
	"bufio"
	"context"
	"net"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
)

// Buffer sizes for the reader and writer kept with each connection. Bodies
// bypass both: large reads go straight into the copy buffer and large writes
// go through httpx.DeadlineWriter's pooled buffer.
const (
	readBufferSize  = 4 << 10
	writeBufferSize = 4 << 10
)

// Conn is a connection to a backend with its buffered reader and writer.
// The reader and writer stay with the connection while it is idle in a
// Pool: bytes the reader holds belong to this connection and no other.
//
// A Conn is used by one request at a time. While a request body streams,
// its writer goroutine shares the Conn with the goroutine reading the
// response, and either may Close it.
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
	reused bool
	// idleSince is when the connection was parked, set by Put.
	idleSince time.Time
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
	c.R = bufio.NewReaderSize(&c.cr, readBufferSize)
	c.W = bufio.NewWriterSize(&c.dw, writeBufferSize)
	return c, nil
}

// Reused reports whether the connection carried an earlier request.
func (c *Conn) Reused() bool { return c.reused }

// BytesRead returns how many bytes have been read from the socket since
// the connection was dialled or last taken from a pool. Zero after a failed
// request means the backend sent nothing at all in reply.
func (c *Conn) BytesRead() int64 { return c.cr.n }

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

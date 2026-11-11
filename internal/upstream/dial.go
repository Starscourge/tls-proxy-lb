// Package upstream manages connections to backends. Phase 1 dials a fresh
// connection per request; Phase 4 adds the keep-alive pool around Conn.
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
type Conn struct {
	net.Conn
	// R reads responses. Reads have no deadline of their own; the caller
	// sets one before reading.
	R *bufio.Reader
	// W writes requests. Each write to the socket pushes both the read and
	// write deadlines ioTimeout into the future, so a response timeout counts
	// from the last request byte sent.
	W *bufio.Writer
}

// Dial connects to addr, giving up after connectTimeout or when ctx ends.
func Dial(ctx context.Context, addr string, connectTimeout, ioTimeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: connectTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	dw := &httpx.DeadlineWriter{Conn: c, Timeout: ioTimeout, ExtendRead: true}
	return &Conn{
		Conn: c,
		R:    bufio.NewReaderSize(c, readBufferSize),
		W:    bufio.NewWriterSize(dw, writeBufferSize),
	}, nil
}

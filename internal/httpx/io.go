package httpx

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// CopyBufferSize is the size of the pooled buffers bodies are copied through.
const CopyBufferSize = 32 << 10

var copyBufPool = sync.Pool{New: func() any {
	b := make([]byte, CopyBufferSize)
	return &b
}}

// ErrAborted is returned by a Body read after Abort.
var ErrAborted = errors.New("httpx: body aborted")

// ErrHeaderTooLarge is returned by a HeaderLimitReader once its limit is used.
var ErrHeaderTooLarge = errors.New("httpx: header block too large")

// Body wraps a request or response body as it streams through the proxy.
//
// Every Read first pushes the connection's read deadline timeout into the
// future, so a transfer that keeps making progress never times out and one
// that stalls does. Close never drains: net/http's bodies read to EOF on
// Close, which would make a client that hangs up mid-download cost the proxy
// the rest of the response.
//
// A Body is read by one goroutine at a time. Abort may be called from any
// goroutine. N, EOF and Err may be called once the reader is done.
type Body struct {
	r       io.Reader
	conn    net.Conn
	timeout time.Duration
	aborted atomic.Bool

	n   int64
	eof bool
	err error
}

// NewBody wraps r. If conn is non-nil and timeout positive, each Read extends
// conn's read deadline by timeout.
func NewBody(r io.Reader, conn net.Conn, timeout time.Duration) *Body {
	return &Body{r: r, conn: conn, timeout: timeout}
}

func (b *Body) Read(p []byte) (int, error) {
	if b.conn != nil && b.timeout > 0 {
		if err := b.conn.SetReadDeadline(time.Now().Add(b.timeout)); err != nil {
			b.setErr(err)
			return 0, err
		}
	}
	// Checked after the deadline is set: Abort sets the flag and then a past
	// deadline, so either this sees the flag or the read sees that deadline.
	if b.aborted.Load() {
		b.setErr(ErrAborted)
		return 0, ErrAborted
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	switch {
	case err == io.EOF:
		b.eof = true
	case err != nil:
		b.setErr(err)
	}
	return n, err
}

func (b *Body) setErr(err error) {
	if b.err == nil {
		b.err = err
	}
}

// Close does nothing; see the type comment.
func (b *Body) Close() error { return nil }

// Abort makes the current and every later Read fail promptly with ErrAborted
// or a timeout. It is safe to call from another goroutine.
func (b *Body) Abort() {
	b.aborted.Store(true)
	if b.conn != nil {
		_ = b.conn.SetReadDeadline(time.Now())
	}
}

// N returns the number of body bytes read.
func (b *Body) N() int64 { return b.n }

// EOF reports whether the body was read to its end.
func (b *Body) EOF() bool { return b.eof }

// Err returns the first read error other than io.EOF, or nil.
func (b *Body) Err() error { return b.err }

// DeadlineWriter writes to a connection, pushing its write deadline Timeout
// into the future before each write. With ExtendRead it moves the read
// deadline too, so that a response timeout counts from the last byte of the
// request written rather than from its first.
//
// Timeout may be changed between writes by the goroutine that owns the
// writer.
type DeadlineWriter struct {
	Conn       net.Conn
	Timeout    time.Duration
	ExtendRead bool
}

func (w *DeadlineWriter) Write(p []byte) (int, error) {
	t := time.Now().Add(w.Timeout)
	var err error
	if w.ExtendRead {
		err = w.Conn.SetDeadline(t)
	} else {
		err = w.Conn.SetWriteDeadline(t)
	}
	if err != nil {
		return 0, err
	}
	return w.Conn.Write(p)
}

// ReadFrom copies r to the connection through a pooled fixed-size buffer.
// A bufio.Writer on top hands large copies here once its own buffer is
// empty, so bodies go out in CopyBufferSize writes instead of the
// bufio.Writer's buffer size.
func (w *DeadlineWriter) ReadFrom(r io.Reader) (int64, error) {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	buf := *bp
	var n int64
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				return n, ew
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return n, nil
		}
		if er != nil {
			return n, er
		}
	}
}

// HeaderLimitReader sits between a connection and its bufio.Reader and caps
// how many bytes can be read while a header block is parsed. Without it a
// header line of unbounded length would be buffered whole. Bytes the
// bufio.Reader already holds are not counted, so the effective limit can be
// exceeded by up to that buffer's size.
type HeaderLimitReader struct {
	R         io.Reader
	remaining int64
	limited   bool
}

// Limit allows n more bytes before reads fail with ErrHeaderTooLarge.
func (l *HeaderLimitReader) Limit(n int64) { l.remaining, l.limited = n, true }

// Unlimit removes the limit, for reading bodies.
func (l *HeaderLimitReader) Unlimit() { l.limited = false }

// Exhausted reports whether the limit was reached.
func (l *HeaderLimitReader) Exhausted() bool { return l.limited && l.remaining <= 0 }

func (l *HeaderLimitReader) Read(p []byte) (int, error) {
	if !l.limited {
		return l.R.Read(p)
	}
	if l.remaining <= 0 {
		return 0, ErrHeaderTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.R.Read(p)
	l.remaining -= int64(n)
	return n, err
}

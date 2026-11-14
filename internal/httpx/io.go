package httpx

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrAborted is returned by a Body read after Abort.
var ErrAborted = errors.New("httpx: body aborted")

// ErrHeaderTooLarge is returned by a HeaderLimitReader once its limit is used.
var ErrHeaderTooLarge = errors.New("httpx: header block too large")

// ErrBodyTooLarge is returned by a Body read that would pass its size cap.
var ErrBodyTooLarge = errors.New("httpx: request body over max_body_bytes")

// ErrBodyTooSlow is returned by a Body read that missed the minimum data
// rate.
var ErrBodyTooSlow = errors.New("httpx: request body below min_body_rate")

// Body wraps a request or response body as it streams through the proxy.
//
// Every Read first pushes the connection's read deadline timeout into the
// future, so a transfer that keeps making progress never times out and one
// that stalls does. Close never drains: net/http's bodies read to EOF on
// Close, which would make a client that hangs up mid-download cost the proxy
// the rest of the response.
//
// A request body may also be held to a size cap and a minimum data rate
// (SetLimits), and have its trailer cleaned at EOF (SetRequest).
//
// A Body is read by one goroutine at a time. Abort may be called from any
// goroutine. N, EOF and Err may be called once the reader is done.
type Body struct {
	r       io.Reader
	conn    net.Conn
	timeout time.Duration
	aborted atomic.Bool

	// max caps the bytes read; 0 is no cap. minRate is the slowest rate
	// in bytes per second a body may arrive at once grace has passed
	// since start; 0 is no minimum.
	max     int64
	minRate int64
	start   time.Time
	grace   time.Duration
	// req, if set, has its Trailer cleaned at EOF.
	req *http.Request

	n   int64
	eof bool
	err error
}

// NewBody wraps r. If conn is non-nil and timeout positive, each Read extends
// conn's read deadline by timeout.
func NewBody(r io.Reader, conn net.Conn, timeout time.Duration) *Body {
	b := &Body{}
	b.Reset(r, conn, timeout)
	return b
}

// Reset makes b a new Body over r, as NewBody would, so one value can serve
// request after request. No other goroutine may be using b.
func (b *Body) Reset(r io.Reader, conn net.Conn, timeout time.Duration) {
	b.r, b.conn, b.timeout = r, conn, timeout
	b.aborted.Store(false)
	b.max, b.minRate, b.grace, b.start, b.req = 0, 0, 0, time.Time{}, nil
	b.n, b.eof, b.err = 0, false, nil
}

// SetLimits holds b to at most max bytes (0: no cap), and, from start plus
// grace on, to at least minRate bytes per second on average since start (0:
// no minimum). The rate works as Apache's mod_reqtimeout MinRate does:
// every byte received moves the deadline 1/minRate seconds later, so a
// steady upload is never cut off and one that trickles is, however often
// it sends a byte. The stall timeout still applies on top.
func (b *Body) SetLimits(max, minRate int64, start time.Time, grace time.Duration) {
	b.max, b.minRate, b.start, b.grace = max, minRate, start, grace
}

// SetRequest makes b clean req's trailer (SanitizeTrailer) when the body
// reaches EOF, before Request.Write sends it on.
func (b *Body) SetRequest(req *http.Request) { b.req = req }

func (b *Body) Read(p []byte) (int, error) {
	rateBound := false
	if b.conn != nil && b.timeout > 0 {
		deadline := time.Now().Add(b.timeout)
		if b.minRate > 0 {
			// Split so n·10^9 cannot overflow for bodies over 9 GB.
			earned := time.Duration(b.n/b.minRate)*time.Second +
				time.Duration(b.n%b.minRate)*time.Second/time.Duration(b.minRate)
			due := b.start.Add(b.grace + earned)
			if due.Before(deadline) {
				deadline, rateBound = due, true
			}
		}
		if err := b.conn.SetReadDeadline(deadline); err != nil {
			b.setErr(err)
			return 0, err
		}
	}
	if b.max > 0 {
		left := b.max - b.n
		// One byte past the cap is asked for, to tell a body that ends
		// exactly at it from one that goes on.
		if int64(len(p)) > left+1 {
			p = p[:left+1]
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
	if b.max > 0 && b.n > b.max {
		n -= int(b.n - b.max)
		b.n = b.max
		b.setErr(ErrBodyTooLarge)
		return n, ErrBodyTooLarge
	}
	switch {
	case err == io.EOF:
		b.eof = true
		if b.req != nil {
			SanitizeTrailer(b.req.Trailer)
		}
	case err != nil && rateBound && isTimeout(err) && !b.aborted.Load():
		err = ErrBodyTooSlow
		b.setErr(err)
	case err != nil:
		// A failure after Abort is the abort's doing (usually the past
		// deadline it set), not the peer's.
		if b.aborted.Load() {
			err = ErrAborted
		}
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

// Err returns the first read error other than io.EOF, or nil. It is
// ErrAborted if reads failed because of Abort.
func (b *Body) Err() error { return b.err }

// DeadlineWriter writes to a connection, pushing its write deadline Timeout
// into the future before each write. With ExtendRead it moves the read
// deadline too, so that a response timeout counts from the last byte of the
// request written rather than from its first.
//
// Timeout may be changed between writes by the goroutine that owns the
// writer.
//
// Bodies reach it through a bufio.Writer, whose large writes go straight
// through when its buffer is empty, so they are written in the size of the
// copy buffer that net/http's Request.Write and Response.Write take from
// their internal pool (32 KiB).
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

// HeaderLimitReader sits between a connection and its bufio.Reader and caps
// how many bytes can be read while a header block is parsed. Without it a
// header line of unbounded length would be buffered whole. Bytes the
// bufio.Reader already holds are not counted, so the effective limit can be
// exceeded by up to that buffer's size.
//
// While limited it can also record what it reads (Record), so the header
// block can be checked as it arrived, before net/http normalised it.
type HeaderLimitReader struct {
	R         io.Reader
	remaining int64
	limited   bool
	rec       *[]byte
}

// Limit allows n more bytes before reads fail with ErrHeaderTooLarge.
func (l *HeaderLimitReader) Limit(n int64) { l.remaining, l.limited = n, true }

// Record appends every byte read from now until Unlimit to *buf.
func (l *HeaderLimitReader) Record(buf *[]byte) { l.rec = buf }

// Unlimit removes the limit, for reading bodies, and stops recording.
func (l *HeaderLimitReader) Unlimit() { l.limited, l.rec = false, nil }

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
	if l.rec != nil {
		*l.rec = append(*l.rec, p[:n]...)
	}
	return n, err
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

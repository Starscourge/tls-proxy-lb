// Package logbuf buffers log output, as nginx's access_log buffer= and
// flush= do.
//
// slog's handlers hold a mutex around each Write to their output. With an
// unbuffered file that Write is a system call per log line, made with the
// lock held, and every connection goroutine queues behind it: under load it
// was the most contended lock in the proxy. Through a Writer, Write is a
// copy into memory and never does I/O; a goroutine of the Writer's own
// writes full buffers out, so slog's lock is never held across a system
// call.
//
// The cost is durability: lines still in the buffer when the process dies
// without Close are lost, up to one flush interval's worth.
package logbuf

import (
	"errors"
	"io"
	"sync"
	"time"
)

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("logbuf: writer closed")

// Writer buffers writes to an underlying writer. It is safe for concurrent
// use, and keeps every Write whole and in order.
//
// There are two buffers. Writers append to one; when it would overflow it
// is handed to the flusher goroutine and the other takes its place. A
// writer waits only if that one fills too before the flusher has finished,
// which is backpressure from a slow output. A Write larger than a buffer is
// copied and handed over on its own.
//
// Goroutines: one flusher, started by New and stopped by Close. It also
// writes out a partly full buffer every interval.
type Writer struct {
	w    io.Writer
	size int

	mu      sync.Mutex
	cond    sync.Cond // signalled when the flusher finishes a write
	buf     []byte    // being filled
	spare   []byte    // nil while the flusher holds the other buffer
	pending []byte    // handed over and not yet written; nil if none
	err     error     // first error from w; later writes fail with it
	closed  bool

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// New returns a Writer with two buffers of size bytes that writes to w
// when a buffer fills and at least every interval. Close must be called to
// stop its flusher and write what is left.
func New(w io.Writer, size int, interval time.Duration) *Writer {
	b := &Writer{
		w:     w,
		size:  size,
		buf:   make([]byte, 0, size),
		spare: make([]byte, 0, size),
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	b.cond.L = &b.mu
	go b.flusher(interval)
	return b
}

// Write buffers p. It fails with the error of an earlier write to the
// underlying writer, if any, or ErrClosed.
func (b *Writer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.err != nil {
			return 0, b.err
		}
		if b.closed {
			return 0, ErrClosed
		}
		if len(b.buf)+len(p) <= b.size {
			b.buf = append(b.buf, p...)
			return len(p), nil
		}
		if b.pending != nil || b.spare == nil {
			// Both buffers are taken: wait for the flusher.
			b.cond.Wait()
			continue
		}
		if len(b.buf) > 0 {
			b.handOffLocked()
			continue
		}
		// Empty buffer and p still does not fit: p goes on its own.
		b.pending = append([]byte(nil), p...)
		b.signal()
		return len(p), nil
	}
}

// handOffLocked gives the current buffer to the flusher and starts filling
// the spare. mu must be held, pending empty and spare present.
func (b *Writer) handOffLocked() {
	b.pending, b.buf, b.spare = b.buf, b.spare, nil
	b.signal()
}

func (b *Writer) signal() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Close stops the flusher once it has written everything buffered, and
// returns the first write error, if any. Calling it again returns the same.
func (b *Writer) Close() error {
	b.once.Do(func() { close(b.stop) })
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *Writer) flusher(interval time.Duration) {
	defer close(b.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var stopping bool
		select {
		case <-b.kick:
		case <-t.C:
			b.mu.Lock()
			if b.pending == nil && b.spare != nil && len(b.buf) > 0 {
				b.handOffLocked()
			}
			b.mu.Unlock()
		case <-b.stop:
			stopping = true
		}
		b.writePending()
		if stopping {
			b.mu.Lock()
			b.closed = true
			if len(b.buf) > 0 && b.err == nil {
				b.pending, b.buf = b.buf, nil
			}
			b.mu.Unlock()
			b.writePending()
			b.mu.Lock()
			b.cond.Broadcast() // wake writers waiting on a full buffer
			b.mu.Unlock()
			return
		}
	}
}

// writePending writes the handed-over buffer, if there is one, and makes
// it the spare again.
func (b *Writer) writePending() {
	b.mu.Lock()
	p := b.pending
	if p == nil {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	_, err := b.w.Write(p)

	b.mu.Lock()
	b.pending = nil
	if b.spare == nil && cap(p) == b.size {
		b.spare = p[:0]
	} else if b.spare == nil {
		b.spare = make([]byte, 0, b.size)
	}
	if err != nil && b.err == nil {
		b.err = err
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

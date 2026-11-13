// Package logbuf buffers log output, as nginx's access_log buffer= and
// flush= do.
//
// slog's handlers hold a mutex around each Write to their output. With an
// unbuffered file that Write is a system call per log line, made with the
// lock held, and every connection goroutine queues behind it: under load it
// was the most contended lock in the proxy. Through a Writer the lock
// covers a copy into memory; the system call happens once per buffer, or
// once per flush interval when traffic is light.
//
// The cost is durability: lines still in the buffer when the process dies
// without Close are lost, up to one flush interval's worth.
package logbuf

import (
	"io"
	"sync"
	"time"
)

// Writer buffers writes to an underlying writer. It is safe for
// concurrent use. Writes are kept whole: a buffer is flushed before a write
// that would not fit, and a write larger than the buffer goes straight
// through.
//
// Goroutines: one flusher, started by New and stopped by Close.
type Writer struct {
	mu   sync.Mutex
	w    io.Writer
	buf  []byte
	err  error // first error from w; later writes are dropped
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// New returns a Writer with a buffer of size bytes that writes to w when
// the buffer fills and at least every interval. Close must be called to
// stop its flusher and write what is left.
func New(w io.Writer, size int, interval time.Duration) *Writer {
	b := &Writer{w: w, buf: make([]byte, 0, size), stop: make(chan struct{}), done: make(chan struct{})}
	go b.flusher(interval)
	return b
}

// Write buffers p. It reports the error of an earlier flush, if any.
func (b *Writer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	if len(b.buf)+len(p) > cap(b.buf) {
		if err := b.flushLocked(); err != nil {
			return 0, err
		}
		if len(p) > cap(b.buf) {
			n, err := b.w.Write(p)
			if err != nil {
				b.err = err
			}
			return n, err
		}
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Flush writes out whatever is buffered.
func (b *Writer) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushLocked()
}

func (b *Writer) flushLocked() error {
	if b.err != nil || len(b.buf) == 0 {
		return b.err
	}
	_, err := b.w.Write(b.buf)
	b.buf = b.buf[:0]
	if err != nil {
		b.err = err
	}
	return err
}

// Close stops the flusher and writes out what is buffered. Writes after
// Close are buffered but only written by an explicit Flush. Calling it
// again just flushes.
func (b *Writer) Close() error {
	b.once.Do(func() { close(b.stop) })
	<-b.done
	return b.Flush()
}

func (b *Writer) flusher(interval time.Duration) {
	defer close(b.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			_ = b.Flush()
		}
	}
}

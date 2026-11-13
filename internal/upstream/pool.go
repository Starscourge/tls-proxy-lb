package upstream

import (
	"context"
	"sync"
	"time"
)

// Options configures the pools of a Registry.
type Options struct {
	// MaxIdle is the most idle connections kept per backend address. Zero
	// turns reuse off: Put closes every connection.
	MaxIdle int
	// IdleTimeout is how long a connection may sit idle before it is
	// closed. It must be positive when MaxIdle is.
	IdleTimeout time.Duration
}

// Pool keeps idle connections to one backend address for reuse.
//
// The idle connections form a stack: Put pushes, Get pops the most recently
// used. Under steady load the same few connections stay hot and the rest
// sink to the bottom, where they reach the idle timeout and are closed, so
// the pool shrinks to what the load needs. A queue would cycle through
// every connection and keep them all just young enough to survive.
//
// The mutex guards only the stack and is never held across I/O.
type Pool struct {
	addr string
	opts Options

	mu     sync.Mutex
	idle   []*Conn // oldest first
	closed bool
}

// NewPool returns an empty pool for addr. A pool made with MaxIdle zero
// dials for every Get and closes on every Put.
func NewPool(addr string, opts Options) *Pool {
	return &Pool{addr: addr, opts: opts}
}

// Addr returns the backend address the pool dials.
func (p *Pool) Addr() string { return p.addr }

// Reuse reports whether the pool keeps connections for reuse. When it does
// not, requests should ask the backend to close.
func (p *Pool) Reuse() bool { return p.opts.MaxIdle > 0 }

// Get returns an idle connection that still looks usable, or dials a new
// one. Idle connections past the idle timeout, and ones the backend has
// closed or sent unsolicited bytes on, are closed and skipped.
//
// A connection that passes the check can still have been closed by the
// backend a moment later; the caller retries on a fresh connection from
// Dial when a reused one fails before any response byte arrives.
func (p *Pool) Get(ctx context.Context, connectTimeout, ioTimeout time.Duration) (*Conn, error) {
	for {
		c := p.pop()
		if c == nil {
			return p.Dial(ctx, connectTimeout, ioTimeout)
		}
		if !usable(c) {
			c.Close()
			continue
		}
		c.reused = true
		c.cr.n = 0
		c.dw.Timeout = ioTimeout
		return c, nil
	}
}

// Dial returns a new connection, bypassing the idle stack.
func (p *Pool) Dial(ctx context.Context, connectTimeout, ioTimeout time.Duration) (*Conn, error) {
	return Dial(ctx, p.addr, connectTimeout, ioTimeout)
}

// pop takes the most recently used idle connection. If that one is past
// the idle timeout, every connection under it is older still, so all of
// them are closed.
func (p *Pool) pop() *Conn {
	p.mu.Lock()
	n := len(p.idle)
	if n == 0 {
		p.mu.Unlock()
		return nil
	}
	c := p.idle[n-1]
	p.idle[n-1] = nil
	p.idle = p.idle[:n-1]
	var expired []*Conn
	if time.Since(c.idleSince) >= p.opts.IdleTimeout {
		expired = append(p.idle, c)
		p.idle = nil
		c = nil
	}
	p.mu.Unlock()
	closeAll(expired)
	return c
}

// Put parks c for reuse after a request whose response was read to its
// end, or closes it if the pool is full, closed or not keeping
// connections. The caller must not use c afterwards.
func (p *Pool) Put(c *Conn) {
	if p.opts.MaxIdle <= 0 {
		c.Close()
		return
	}
	// A parked connection has no deadline: the stale check on Get must be
	// able to look at it, and the next request sets its own.
	if err := c.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return
	}
	c.idleSince = time.Now()
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.opts.MaxIdle {
		p.mu.Unlock()
		c.Close()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

// Idle returns the number of idle connections.
func (p *Pool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// reap closes the connections idle since before now minus the idle
// timeout. They sit at the bottom of the stack.
func (p *Pool) reap(now time.Time) {
	cutoff := now.Add(-p.opts.IdleTimeout)
	p.mu.Lock()
	i := 0
	for i < len(p.idle) && !p.idle[i].idleSince.After(cutoff) {
		i++
	}
	expired := append([]*Conn(nil), p.idle[:i]...)
	if i > 0 {
		n := copy(p.idle, p.idle[i:])
		clear(p.idle[n:])
		p.idle = p.idle[:n]
	}
	p.mu.Unlock()
	closeAll(expired)
}

// Close closes every idle connection, and every connection Put afterwards.
func (p *Pool) Close() {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.closed = true
	p.mu.Unlock()
	closeAll(idle)
}

func closeAll(cs []*Conn) {
	for _, c := range cs {
		c.Close()
	}
}

// usable reports whether an idle connection can carry a request: nothing
// buffered that no request asked for, and the socket neither closed by the
// backend nor holding unread bytes.
func usable(c *Conn) bool {
	return c.R.Buffered() == 0 && alive(c.Conn)
}

package upstream

import (
	"sync"
	"time"
)

// minReapInterval keeps a tiny idle timeout from turning the reaper into a
// busy loop.
const minReapInterval = 10 * time.Millisecond

// Registry holds one Pool per backend address, so every pool that lists an
// address shares its idle connections: a TCP connection to 10.0.0.11:8000
// serves any request for that address, whichever balancer chose it. The
// registry outlives config snapshots; a snapshot built after a reload asks
// for the same addresses and gets the same pools.
//
// Goroutines: one reaper, started by NewRegistry when reuse is on and
// stopped by Close. It wakes every half idle timeout and closes connections
// past it, so idle backends do not keep sockets open. Get skips expired
// connections on its own; the reaper only bounds how long they linger.
type Registry struct {
	opts Options

	mu     sync.Mutex
	pools  map[string]*Pool
	closed bool

	stop chan struct{}
	done chan struct{}
}

// NewRegistry returns an empty registry whose pools use opts. Close must be
// called to stop its reaper.
func NewRegistry(opts Options) *Registry {
	r := &Registry{opts: opts, pools: make(map[string]*Pool), stop: make(chan struct{}), done: make(chan struct{})}
	if opts.MaxIdle <= 0 {
		close(r.done)
		return r
	}
	interval := max(opts.IdleTimeout/2, minReapInterval)
	go r.reaper(interval)
	return r
}

// Pool returns the pool for addr, creating it on first use. It is called
// while building a snapshot, not per request.
func (r *Registry) Pool(addr string) *Pool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pools[addr]
	if !ok {
		p = NewPool(addr, r.opts)
		if r.closed {
			p.Close()
		}
		r.pools[addr] = p
	}
	return p
}

// Close stops the reaper and closes every pool's idle connections.
// Connections in use are closed when their requests end, by Put.
func (r *Registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	pools := r.snapshot()
	r.mu.Unlock()
	close(r.stop)
	<-r.done
	for _, p := range pools {
		p.Close()
	}
}

// snapshot copies the pool list; r.mu must be held.
func (r *Registry) snapshot() []*Pool {
	out := make([]*Pool, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p)
	}
	return out
}

func (r *Registry) reaper(interval time.Duration) {
	defer close(r.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			pools := r.snapshot()
			r.mu.Unlock()
			for _, p := range pools {
				p.reap(now)
			}
		}
	}
}

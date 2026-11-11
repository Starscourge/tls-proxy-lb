// Package conn owns client connections: the accept loop, the tracker that
// counts, lists and caps them, and the per-connection proxy loop.
package conn

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// State is where a tracked connection is in its life.
type State int32

// Connection states.
const (
	StateNew    State = iota // accepted, first request not started
	StateIdle                // waiting for the next request on a keep-alive connection
	StateActive              // reading, proxying or answering a request
	StateClosed              // done
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateIdle:
		return "idle"
	case StateActive:
		return "active"
	case StateClosed:
		return "closed"
	}
	return "unknown"
}

// TrackedConn is a client connection registered with a Tracker.
type TrackedConn struct {
	net.Conn
	id      uint64
	started time.Time
	state   atomic.Int32
}

// ID returns the tracker-assigned connection number, unique per tracker.
func (c *TrackedConn) ID() uint64 { return c.id }

// State returns the connection's current state.
func (c *TrackedConn) State() State { return State(c.state.Load()) }

// SetState records the connection's state. Only the connection's own
// goroutine calls it; other goroutines read it through State or List.
func (c *TrackedConn) SetState(s State) { c.state.Store(int32(s)) }

// ConnInfo describes one open connection, as returned by Tracker.List.
type ConnInfo struct {
	ID      uint64
	Remote  string
	Started time.Time
	State   State
}

// Tracker counts and lists open client connections and enforces the cap on
// them. It is shared by the accept loop, which adds connections, and the
// connection goroutines, which remove themselves; a mutex guards the set and
// atomics hold the counters.
type Tracker struct {
	max int

	mu     sync.Mutex
	conns  map[*TrackedConn]struct{}
	closed bool
	wg     sync.WaitGroup

	nextID   atomic.Uint64
	accepted atomic.Uint64
	rejected atomic.Uint64
}

// NewTracker returns a tracker that admits at most max connections at once.
// max <= 0 means no cap.
func NewTracker(max int) *Tracker {
	return &Tracker{max: max, conns: make(map[*TrackedConn]struct{})}
}

// Add registers c. It returns false, and registers nothing, if the tracker is
// at its cap or closed; the caller then closes c. Every successful Add must
// be matched by one Remove.
func (t *Tracker) Add(c net.Conn) (*TrackedConn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (t.max > 0 && len(t.conns) >= t.max) {
		t.rejected.Add(1)
		return nil, false
	}
	tc := &TrackedConn{Conn: c, id: t.nextID.Add(1), started: time.Now()}
	t.conns[tc] = struct{}{}
	t.wg.Add(1)
	t.accepted.Add(1)
	return tc, true
}

// Remove unregisters tc. It does not close it.
func (t *Tracker) Remove(tc *TrackedConn) {
	tc.SetState(StateClosed)
	t.mu.Lock()
	_, ok := t.conns[tc]
	delete(t.conns, tc)
	t.mu.Unlock()
	if ok {
		t.wg.Done()
	}
}

// Len returns the number of open connections.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// Accepted returns how many connections Add admitted in total.
func (t *Tracker) Accepted() uint64 { return t.accepted.Load() }

// Rejected returns how many connections Add turned away.
func (t *Tracker) Rejected() uint64 { return t.rejected.Load() }

// List returns a description of every open connection, in no fixed order.
func (t *Tracker) List() []ConnInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ConnInfo, 0, len(t.conns))
	for tc := range t.conns {
		out = append(out, ConnInfo{ID: tc.id, Remote: tc.RemoteAddr().String(), Started: tc.started, State: tc.State()})
	}
	return out
}

// CloseAll stops admitting connections and closes every open one, which
// unblocks their goroutines. Phase 7 replaces this hard stop with a drain.
func (t *Tracker) CloseAll() {
	t.mu.Lock()
	t.closed = true
	conns := make([]*TrackedConn, 0, len(t.conns))
	for tc := range t.conns {
		conns = append(conns, tc)
	}
	t.mu.Unlock()
	for _, tc := range conns {
		tc.Close()
	}
}

// Wait blocks until every added connection has been removed.
func (t *Tracker) Wait() { t.wg.Wait() }

// Package conn owns client connections: the accept loop, the tracker that
// counts, lists and caps them, and the per-connection proxy loop.
package conn

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
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
	t       *Tracker
	id      uint64
	started time.Time
	state   atomic.Int32
	// peer is the remote IP, unmapped; invalid for a connection that is
	// not TCP. perIP is its key in the tracker's per-IP counts, invalid if
	// the connection is not counted there.
	peer  netip.Addr
	perIP netip.Addr
	// upstream is the backend connection the current request is using,
	// so CloseAll can cut off a request waiting on a backend, which no
	// client read or write would interrupt.
	upstream atomic.Pointer[upstream.Conn]
}

// SetUpstream records the backend connection the current request is
// using, or nil once it has been parked or discarded.
func (c *TrackedConn) SetUpstream(uc *upstream.Conn) { c.upstream.Store(uc) }

// Peer returns the remote IP address, IPv4-mapped addresses unmapped. It is
// the zero Addr for a connection that is not over IP, such as a pipe.
func (c *TrackedConn) Peer() netip.Addr { return c.peer }

// ID returns the tracker-assigned connection number, unique per tracker.
func (c *TrackedConn) ID() uint64 { return c.id }

// State returns the connection's current state.
func (c *TrackedConn) State() State { return State(c.state.Load()) }

// SetState records the connection's state. Only the connection's own
// goroutine calls it; other goroutines read it through State or List.
func (c *TrackedConn) SetState(s State) { c.state.Store(int32(s)) }

// Draining reports whether the server is shutting down: the connection
// should finish the request it is on, announce Connection: close, and take
// no other.
func (c *TrackedConn) Draining() bool { return c.t != nil && c.t.draining.Load() }

// ConnInfo describes one open connection, as returned by Tracker.List.
type ConnInfo struct {
	ID      uint64
	Remote  string
	Started time.Time
	State   State
}

// Limits are the caps a Tracker enforces at accept.
type Limits struct {
	// MaxConns is the most connections open at once. Zero means no cap.
	MaxConns int
	// MaxPerIP is the most connections open at once from one address, an
	// IPv6 address counting as its /64 (ratelimit.KeyAddr). It stops one
	// client from taking every slot and spending the handshake CPU. Zero
	// means no cap.
	MaxPerIP int
	// Exempt are addresses MaxPerIP does not apply to: trusted proxies,
	// which carry many clients' connections.
	Exempt ratelimit.Trusted
}

// RejectReason is why Add turned a connection away.
type RejectReason int

// The reject reasons.
const (
	// RejectNone: the connection was admitted.
	RejectNone RejectReason = iota
	// RejectMaxConns: the tracker was at MaxConns, or closed.
	RejectMaxConns
	// RejectPerIP: the client's address was at MaxPerIP.
	RejectPerIP
	numRejectReasons
)

var rejectReasonNames = [numRejectReasons]string{"", "max_conns", "per_ip"}

func (r RejectReason) String() string { return rejectReasonNames[r] }

// Tracker counts and lists open client connections and enforces the caps on
// them. It is shared by the accept loop, which adds connections, and the
// connection goroutines, which remove themselves; a mutex guards the set and
// the per-IP counts, and atomics hold the counters, the limits and the
// draining flag.
type Tracker struct {
	limits   atomic.Pointer[Limits]
	draining atomic.Bool

	mu     sync.Mutex
	conns  map[*TrackedConn]struct{}
	perIP  map[netip.Addr]int
	closed bool
	wg     sync.WaitGroup

	nextID   atomic.Uint64
	accepted atomic.Uint64
	rejected [numRejectReasons]atomic.Uint64
}

// NewTracker returns a tracker that enforces l.
func NewTracker(l Limits) *Tracker {
	t := &Tracker{conns: make(map[*TrackedConn]struct{}), perIP: make(map[netip.Addr]int)}
	t.SetLimits(l)
	return t
}

// SetLimits replaces the caps checked by later calls to Add. Connections
// already open are not closed, even if they are now over a cap.
func (t *Tracker) SetLimits(l Limits) { t.limits.Store(&l) }

// Limits returns the caps in force.
func (t *Tracker) Limits() Limits { return *t.limits.Load() }

// Add registers c. It returns false, and registers nothing, if the tracker is
// at a cap or closed, with the reason; the caller then closes c. Every
// successful Add must be matched by one Remove.
func (t *Tracker) Add(c net.Conn) (*TrackedConn, RejectReason) {
	peer := remoteIP(c)
	l := t.limits.Load()
	var key netip.Addr
	if l.MaxPerIP > 0 && peer.IsValid() && !l.Exempt.Contains(peer) {
		key = ratelimit.KeyAddr(peer)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (l.MaxConns > 0 && len(t.conns) >= l.MaxConns) {
		t.rejected[RejectMaxConns].Add(1)
		return nil, RejectMaxConns
	}
	if key.IsValid() {
		if t.perIP[key] >= l.MaxPerIP {
			t.rejected[RejectPerIP].Add(1)
			return nil, RejectPerIP
		}
		t.perIP[key]++
	}
	tc := &TrackedConn{Conn: c, t: t, id: t.nextID.Add(1), started: time.Now(), peer: peer, perIP: key}
	t.conns[tc] = struct{}{}
	t.wg.Add(1)
	t.accepted.Add(1)
	return tc, RejectNone
}

// remoteIP returns c's remote IP, unmapped, or the zero Addr.
func remoteIP(c net.Conn) netip.Addr {
	switch a := c.RemoteAddr().(type) {
	case *net.TCPAddr:
		return a.AddrPort().Addr().Unmap()
	case nil:
		return netip.Addr{}
	default:
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			return ap.Addr().Unmap()
		}
		return netip.Addr{}
	}
}

// Remove unregisters tc. It does not close it.
func (t *Tracker) Remove(tc *TrackedConn) {
	tc.SetState(StateClosed)
	t.mu.Lock()
	_, ok := t.conns[tc]
	delete(t.conns, tc)
	if ok && tc.perIP.IsValid() {
		if n := t.perIP[tc.perIP] - 1; n > 0 {
			t.perIP[tc.perIP] = n
		} else {
			delete(t.perIP, tc.perIP)
		}
	}
	t.mu.Unlock()
	if ok {
		t.wg.Done()
	}
}

// ConnsFrom returns the number of open connections counted against a's
// per-IP cap.
func (t *Tracker) ConnsFrom(a netip.Addr) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.perIP[ratelimit.KeyAddr(a)]
}

// Len returns the number of open connections.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// Accepted returns how many connections Add admitted in total.
func (t *Tracker) Accepted() uint64 { return t.accepted.Load() }

// Rejected returns how many connections Add turned away, for any reason.
func (t *Tracker) Rejected() uint64 {
	return t.rejected[RejectMaxConns].Load() + t.rejected[RejectPerIP].Load()
}

// RejectedByName returns how many connections Add turned away for the
// reason whose String is name, or 0 for an unknown name.
func (t *Tracker) RejectedByName(name string) uint64 {
	for r := RejectNone + 1; r < numRejectReasons; r++ {
		if r.String() == name {
			return t.rejected[r].Load()
		}
	}
	return 0
}

// RejectedFor returns how many connections Add turned away for reason.
func (t *Tracker) RejectedFor(reason RejectReason) uint64 {
	if reason <= RejectNone || reason >= numRejectReasons {
		return 0
	}
	return t.rejected[reason].Load()
}

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

// Drain starts a graceful shutdown. Every connection's Draining reports
// true from now on, and connections waiting idle for their next request
// are woken by a read deadline in the past, so they end at once. A
// connection still on its first request (StateNew) is left to send it.
//
// A connection going idle sets its state, then its idle deadline, then
// checks Draining. Drain sets the flag before it looks at states, so either
// the connection sees the flag, or Drain sees it idle and its deadline,
// set later, wins.
func (t *Tracker) Drain() {
	t.draining.Store(true)
	t.mu.Lock()
	idle := make([]*TrackedConn, 0, len(t.conns))
	for tc := range t.conns {
		if tc.State() == StateIdle {
			idle = append(idle, tc)
		}
	}
	t.mu.Unlock()
	now := time.Now()
	for _, tc := range idle {
		_ = tc.SetReadDeadline(now)
	}
}

// Draining reports whether Drain has been called.
func (t *Tracker) Draining() bool { return t.draining.Load() }

// CloseAll stops admitting connections and closes every open one, which
// unblocks their goroutines. It is the hard stop, used directly or when a
// drain runs out of time.
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
		// A connection being parked at this moment may be closed after
		// it is back in the idle pool; the pool's liveness check discards
		// it on the next Get.
		if uc := tc.upstream.Load(); uc != nil {
			uc.Close()
		}
	}
}

// Wait blocks until every added connection has been removed.
func (t *Tracker) Wait() { t.wg.Wait() }

// Done returns a channel closed once every added connection has been
// removed. The goroutine that closes it returns at the same moment, so the
// tracker must not be added to after Done is called (the server has closed
// its listeners).
func (t *Tracker) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	return done
}

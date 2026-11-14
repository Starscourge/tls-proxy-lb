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
	// peer is the remote IP, unmapped; invalid for a connection that is
	// not TCP. perIP is its key in the tracker's per-IP counts, invalid if
	// the connection is not counted there.
	peer  netip.Addr
	perIP netip.Addr
}

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
// the per-IP counts, and atomics hold the counters.
type Tracker struct {
	limits Limits

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
	return &Tracker{limits: l, conns: make(map[*TrackedConn]struct{}), perIP: make(map[netip.Addr]int)}
}

// Add registers c. It returns false, and registers nothing, if the tracker is
// at a cap or closed, with the reason; the caller then closes c. Every
// successful Add must be matched by one Remove.
func (t *Tracker) Add(c net.Conn) (*TrackedConn, RejectReason) {
	peer := remoteIP(c)
	var key netip.Addr
	if t.limits.MaxPerIP > 0 && peer.IsValid() && !t.limits.Exempt.Contains(peer) {
		key = ratelimit.KeyAddr(peer)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (t.limits.MaxConns > 0 && len(t.conns) >= t.limits.MaxConns) {
		t.rejected[RejectMaxConns].Add(1)
		return nil, RejectMaxConns
	}
	if key.IsValid() {
		if t.perIP[key] >= t.limits.MaxPerIP {
			t.rejected[RejectPerIP].Add(1)
			return nil, RejectPerIP
		}
		t.perIP[key]++
	}
	tc := &TrackedConn{Conn: c, id: t.nextID.Add(1), started: time.Now(), peer: peer, perIP: key}
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

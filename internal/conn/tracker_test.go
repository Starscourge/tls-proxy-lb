package conn

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func pipeConn(t *testing.T) net.Conn {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a
}

func TestTrackerCap(t *testing.T) {
	tr := NewTracker(Limits{MaxConns: 2})
	c1, ok1 := add(tr, pipeConn(t))
	_, ok2 := add(tr, pipeConn(t))
	_, ok3 := add(tr, pipeConn(t))
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("adds = %v %v %v, want true true false", ok1, ok2, ok3)
	}
	if tr.Len() != 2 || tr.Accepted() != 2 || tr.Rejected() != 1 {
		t.Fatalf("len %d accepted %d rejected %d", tr.Len(), tr.Accepted(), tr.Rejected())
	}
	tr.Remove(c1)
	tr.Remove(c1) // a second Remove is harmless
	if _, ok := add(tr, pipeConn(t)); !ok {
		t.Fatal("slot not freed by Remove")
	}
}

func TestTrackerNoCap(t *testing.T) {
	tr := NewTracker(Limits{})
	for range 100 {
		if _, ok := add(tr, pipeConn(t)); !ok {
			t.Fatal("uncapped tracker refused a connection")
		}
	}
}

func TestTrackerListAndState(t *testing.T) {
	tr := NewTracker(Limits{})
	a, _ := add(tr, pipeConn(t))
	b, _ := add(tr, pipeConn(t))
	b.SetState(StateActive)
	if a.ID() == b.ID() {
		t.Fatal("IDs not unique")
	}
	states := map[uint64]State{}
	for _, ci := range tr.List() {
		states[ci.ID] = ci.State
	}
	if len(states) != 2 || states[a.ID()] != StateNew || states[b.ID()] != StateActive {
		t.Fatalf("List states = %v", states)
	}
	tr.Remove(a)
	if a.State() != StateClosed || len(tr.List()) != 1 {
		t.Fatalf("after Remove: state %v, list %d", a.State(), len(tr.List()))
	}
}

// TestTrackerCloseAll: CloseAll closes open connections, which unblocks
// their readers, refuses new ones, and Wait returns once all are removed.
func TestTrackerCloseAll(t *testing.T) {
	tr := NewTracker(Limits{})
	var wg sync.WaitGroup
	for range 5 {
		tc, _ := add(tr, pipeConn(t))
		wg.Go(func() {
			defer tr.Remove(tc)
			tc.Read(make([]byte, 1)) // blocks until closed
		})
	}
	tr.CloseAll()
	done := make(chan struct{})
	go func() { tr.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after CloseAll")
	}
	wg.Wait()
	if _, ok := add(tr, pipeConn(t)); ok {
		t.Fatal("Add succeeded after CloseAll")
	}
}

func TestTrackerConcurrent(t *testing.T) {
	tr := NewTracker(Limits{MaxConns: 50})
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if tc, ok := add(tr, pipeConn(t)); ok {
				tc.SetState(StateActive)
				_ = tr.List()
				tr.Remove(tc)
			}
		})
	}
	wg.Wait()
	if tr.Len() != 0 || tr.Accepted()+tr.Rejected() != 200 {
		t.Fatalf("len %d, accepted+rejected %d", tr.Len(), tr.Accepted()+tr.Rejected())
	}
}

// add calls tr.Add and reports whether c was admitted.
func add(tr *Tracker, c net.Conn) (*TrackedConn, bool) {
	tc, reason := tr.Add(c)
	return tc, reason == RejectNone
}

// addrConn is a pipe that claims to come from a TCP address.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

func connFrom(t *testing.T, ipport string) net.Conn {
	return addrConn{Conn: pipeConn(t), remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(ipport))}
}

func TestTrackerPerIPCap(t *testing.T) {
	tr := NewTracker(Limits{MaxPerIP: 2, Exempt: ratelimit.Trusted{netip.MustParsePrefix("10.0.0.0/8")}})
	a1, ok1 := add(tr, connFrom(t, "192.0.2.1:1000"))
	_, ok2 := add(tr, connFrom(t, "[::ffff:192.0.2.1]:1001")) // the same client, mapped
	if !ok1 || !ok2 {
		t.Fatal("connections under the per-IP cap refused")
	}
	if a1.Peer() != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("Peer = %v", a1.Peer())
	}
	if _, reason := tr.Add(connFrom(t, "192.0.2.1:1002")); reason != RejectPerIP {
		t.Fatalf("third connection from one IP: reason %v, want per_ip", reason)
	}
	if _, ok := add(tr, connFrom(t, "192.0.2.2:1000")); !ok {
		t.Fatal("another IP refused")
	}
	// Trusted proxies are exempt.
	for i := range 5 {
		if _, ok := add(tr, connFrom(t, fmt.Sprintf("10.1.2.3:%d", 2000+i))); !ok {
			t.Fatalf("trusted proxy connection %d refused", i+1)
		}
	}
	if got := tr.RejectedFor(RejectPerIP); got != 1 {
		t.Fatalf("RejectedFor(per_ip) = %d, want 1", got)
	}
	if tr.Rejected() != 1 || tr.RejectedFor(RejectMaxConns) != 0 {
		t.Fatalf("Rejected %d, max_conns %d", tr.Rejected(), tr.RejectedFor(RejectMaxConns))
	}
	// Closing one frees its slot.
	tr.Remove(a1)
	tr.Remove(a1)
	if n := tr.ConnsFrom(netip.MustParseAddr("192.0.2.1")); n != 1 {
		t.Fatalf("ConnsFrom = %d after Remove, want 1", n)
	}
	if _, ok := add(tr, connFrom(t, "192.0.2.1:1003")); !ok {
		t.Fatal("slot not freed by Remove")
	}
}

func TestTrackerPerIPCapIPv6Prefix(t *testing.T) {
	tr := NewTracker(Limits{MaxPerIP: 1})
	if _, ok := add(tr, connFrom(t, "[2001:db8:1:2::1]:1000")); !ok {
		t.Fatal("first refused")
	}
	if _, reason := tr.Add(connFrom(t, "[2001:db8:1:2:ffff::9]:1000")); reason != RejectPerIP {
		t.Fatalf("another address in the same /64: %v, want per_ip", reason)
	}
	if _, ok := add(tr, connFrom(t, "[2001:db8:1:3::1]:1000")); !ok {
		t.Fatal("another /64 refused")
	}
}

func TestTrackerPerIPEntriesFreed(t *testing.T) {
	tr := NewTracker(Limits{MaxPerIP: 3})
	var tcs []*TrackedConn
	for i := range 100 {
		tc, ok := add(tr, connFrom(t, fmt.Sprintf("192.0.2.%d:80", i)))
		if !ok {
			t.Fatal("refused")
		}
		tcs = append(tcs, tc)
	}
	for _, tc := range tcs {
		tr.Remove(tc)
	}
	tr.mu.Lock()
	n := len(tr.perIP)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d per-IP entries left after every connection closed", n)
	}
}

func TestTrackerMaxConnsReason(t *testing.T) {
	tr := NewTracker(Limits{MaxConns: 1, MaxPerIP: 5})
	add(tr, connFrom(t, "192.0.2.1:1"))
	if _, reason := tr.Add(connFrom(t, "192.0.2.2:1")); reason != RejectMaxConns {
		t.Fatalf("reason %v, want max_conns", reason)
	}
	if n := tr.ConnsFrom(netip.MustParseAddr("192.0.2.2")); n != 0 {
		t.Fatalf("a refused connection was counted: %d", n)
	}
}

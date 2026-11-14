package conn

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// shutdownAsync runs s.Shutdown with timeout d and returns its result on a
// channel.
func shutdownAsync(s *Server, d time.Duration) <-chan error {
	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		errc <- s.Shutdown(ctx)
	}()
	return errc
}

// A request in flight when Shutdown starts completes, its response says
// Connection: close, the connection then closes, new connections are
// refused, and Shutdown returns nil.
func TestShutdownDrainsInFlight(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET /slow?ms=500 HTTP/1.1\r\nHost: a\r\n\r\n")
	waitFor(t, "request in flight", func() bool {
		l := s.Tracker().List()
		return len(l) == 1 && l[0].State == StateActive
	})
	start := time.Now()
	errc := shutdownAsync(s, 5*time.Second)
	waitFor(t, "draining", s.Tracker().Draining)
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("dial succeeded during the drain")
	}
	resp, _ := cl.read("GET")
	if resp.StatusCode != 200 || !resp.Close {
		t.Fatalf("status %d, close %v; want 200 with Connection: close", resp.StatusCode, resp.Close)
	}
	cl.expectClosed(time.Second)
	if err := <-errc; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Shutdown took %v after the response", el)
	}
}

// An idle keep-alive connection is closed at once, not after the idle
// timeout.
func TestShutdownClosesIdle(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 || resp.Close {
		t.Fatalf("status %d, close %v", resp.StatusCode, resp.Close)
	}
	waitFor(t, "idle", func() bool {
		l := s.Tracker().List()
		return len(l) == 1 && l[0].State == StateIdle
	})
	start := time.Now()
	if err := <-shutdownAsync(s, 5*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("idle connection took %v to close", el)
	}
	cl.expectClosed(time.Second)
}

// A connection that went idle while the drain was starting is still
// closed, whichever happened first: run many times to catch the race
// between the state change and Drain.
func TestShutdownIdleRace(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	for range 50 {
		addr, s, _ := startProxy(t, baddr, nil)
		cl := dialClient(t, addr)
		cl.send(get("a"))
		errc := make(chan error, 1)
		// Shutdown races the connection's switch to idle after the
		// response.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errc <- s.Shutdown(ctx)
		}()
		// The response may or may not get out first, depending on when
		// the drain started; either way the connection must close and
		// Shutdown must not wait for the 60-second idle timeout.
		cl.c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.Copy(io.Discard, cl.br); isTimeout(err) {
			t.Fatal("connection still open after the drain")
		}
		if err := <-errc; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}
}

// When the deadline passes first, what remains is closed and Shutdown
// returns the context's error.
func TestShutdownDeadline(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET /slow?ms=5000 HTTP/1.1\r\nHost: a\r\n\r\n")
	waitFor(t, "request in flight", func() bool {
		l := s.Tracker().List()
		return len(l) == 1 && l[0].State == StateActive
	})
	start := time.Now()
	err := <-shutdownAsync(s, 300*time.Millisecond)
	el := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v, want DeadlineExceeded", err)
	}
	if el < 300*time.Millisecond || el > time.Second {
		t.Fatalf("Shutdown took %v, want about 300ms", el)
	}
	if n := s.Tracker().Len(); n != 0 {
		t.Fatalf("%d connections left", n)
	}
	cl.expectClosed(time.Second)
}

// A new connection's first request is served during the drain, with
// Connection: close.
func TestShutdownServesFirstRequest(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	waitFor(t, "accepted", func() bool { return s.Tracker().Len() == 1 })
	errc := shutdownAsync(s, 5*time.Second)
	waitFor(t, "draining", s.Tracker().Draining)
	cl.send(get("a"))
	resp, _ := cl.read("GET")
	if resp.StatusCode != 200 || !resp.Close {
		t.Fatalf("status %d, close %v; want 200 with Connection: close", resp.StatusCode, resp.Close)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// CloseListener stops one accept loop; its connections carry on, and the
// server's other listeners are untouched.
func TestCloseListener(t *testing.T) {
	h := &echoHandler{}
	s := NewServer(Limits{}, nil)
	t.Cleanup(func() { s.Close() })
	lns := make([]net.Listener, 2)
	errcs := make([]chan error, 2)
	for i := range lns {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns[i], errcs[i] = ln, make(chan error, 1)
		go func() { errcs[i] <- s.Serve(ln, h) }()
	}
	addr0, addr1 := lns[0].Addr().String(), lns[1].Addr().String()
	c := dial(t, addr0)
	if err := roundTrip(t, c, "before"); err != nil {
		t.Fatal(err)
	}
	if !s.CloseListener(lns[0]) {
		t.Fatal("CloseListener: listener not found")
	}
	if err := <-errcs[0]; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve returned %v, want ErrServerClosed", err)
	}
	if c2, err := net.DialTimeout("tcp", addr0, time.Second); err == nil {
		c2.Close()
		t.Fatal("dial to the closed listener succeeded")
	}
	if err := roundTrip(t, c, "after"); err != nil {
		t.Fatalf("existing connection: %v", err)
	}
	if err := roundTrip(t, dial(t, addr1), "other"); err != nil {
		t.Fatalf("other listener: %v", err)
	}
	if s.CloseListener(lns[0]) {
		t.Fatal("CloseListener found a listener already closed")
	}
	s.Close()
	if err := <-errcs[1]; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve returned %v, want ErrServerClosed", err)
	}
}

// SetLimits applies to connections accepted afterwards.
func TestTrackerSetLimits(t *testing.T) {
	tr := NewTracker(Limits{MaxConns: 1})
	a, _ := net.Pipe()
	defer a.Close()
	tc, r := tr.Add(a)
	if r != RejectNone {
		t.Fatalf("first Add: %v", r)
	}
	if _, r := tr.Add(a); r != RejectMaxConns {
		t.Fatalf("second Add at the cap: %v", r)
	}
	tr.SetLimits(Limits{MaxConns: 2})
	tc2, r := tr.Add(a)
	if r != RejectNone {
		t.Fatalf("Add after raising the cap: %v", r)
	}
	tr.SetLimits(Limits{MaxConns: 1})
	if _, r := tr.Add(a); r != RejectMaxConns {
		t.Fatalf("Add after lowering the cap: %v", r)
	}
	if n := tr.Len(); n != 2 {
		t.Fatalf("lowering the cap closed connections: %d open", n)
	}
	if got := tr.Limits().MaxConns; got != 1 {
		t.Fatalf("Limits().MaxConns = %d", got)
	}
	tr.Remove(tc)
	tr.Remove(tc2)
}

// The exemption list is read at accept, so a reloaded trusted_proxies
// applies to new connections.
func TestTrackerSetLimitsExempt(t *testing.T) {
	tr := NewTracker(Limits{MaxPerIP: 1})
	peer := fakeConn{addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1}}
	tc, _ := tr.Add(peer)
	if _, r := tr.Add(peer); r != RejectPerIP {
		t.Fatalf("second Add from one IP: %v", r)
	}
	trusted, err := ratelimit.ParseTrusted([]string{"10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	tr.SetLimits(Limits{MaxPerIP: 1, Exempt: trusted})
	tc2, r := tr.Add(peer)
	if r != RejectNone {
		t.Fatalf("Add from an exempt IP: %v", r)
	}
	tr.Remove(tc2)
	tr.Remove(tc)
	if n := tr.ConnsFrom(netip.MustParseAddr("10.0.0.1")); n != 0 {
		t.Fatalf("per-IP count %d after removing both", n)
	}
}

// fakeConn is a net.Conn with only a remote address.
type fakeConn struct {
	net.Conn
	addr net.Addr
}

func (c fakeConn) RemoteAddr() net.Addr { return c.addr }

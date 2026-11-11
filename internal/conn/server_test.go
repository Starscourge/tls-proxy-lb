package conn

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// echoHandler echoes until the client closes or the connection is closed.
type echoHandler struct{ served atomic.Int64 }

func (h *echoHandler) ServeConn(_ context.Context, c *TrackedConn) {
	h.served.Add(1)
	c.SetState(StateActive)
	io.Copy(c, c)
}

func startServer(t *testing.T, h Handler, maxConns int) (*Server, string, chan error) {
	t.Helper()
	s := NewServer(h, maxConns, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()
	return s, ln.Addr().String(), errc
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func roundTrip(t *testing.T, c net.Conn, msg string) error {
	t.Helper()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	_, err := io.ReadFull(c, buf)
	if err == nil && string(buf) != msg {
		t.Fatalf("echo %q, want %q", buf, msg)
	}
	return err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerServesAndCloses(t *testing.T) {
	h := &echoHandler{}
	s, addr, errc := startServer(t, h, 0)
	conns := make([]net.Conn, 3)
	for i := range conns {
		conns[i] = dial(t, addr)
		if err := roundTrip(t, conns[i], "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Tracker().Len(); n != 3 {
		t.Fatalf("tracker has %d connections, want 3", n)
	}
	// One client leaves: its goroutine ends and the tracker forgets it.
	conns[0].Close()
	waitFor(t, "tracker to drop the closed connection", func() bool { return s.Tracker().Len() == 2 })

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve returned %v, want ErrServerClosed", err)
	}
	if n := s.Tracker().Len(); n != 0 {
		t.Fatalf("%d connections left after Close", n)
	}
	// Remaining clients see their connections closed.
	conns[1].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conns[1].Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after Close")
	}
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after Close")
	}
}

func TestServerConnectionCap(t *testing.T) {
	h := &echoHandler{}
	s, addr, errc := startServer(t, h, 2)
	defer func() { s.Close(); <-errc }()
	a, b := dial(t, addr), dial(t, addr)
	for _, c := range []net.Conn{a, b} {
		if err := roundTrip(t, c, "x"); err != nil {
			t.Fatal(err)
		}
	}
	third := dial(t, addr)
	third.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := third.Read(make([]byte, 1)); err == nil {
		t.Fatal("third connection was served over a cap of 2")
	}
	if s.Tracker().Rejected() != 1 {
		t.Fatalf("rejected = %d, want 1", s.Tracker().Rejected())
	}
	// Freeing a slot admits the next client.
	a.Close()
	waitFor(t, "slot to free", func() bool { return s.Tracker().Len() == 1 })
	if err := roundTrip(t, dial(t, addr), "y"); err != nil {
		t.Fatalf("connection after freeing a slot: %v", err)
	}
}

func TestServeAfterClose(t *testing.T) {
	s := NewServer(&echoHandler{}, 0, nil)
	s.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	if err := s.Serve(ln); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve after Close = %v", err)
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("listener left open")
	}
}

func TestServerTwoListeners(t *testing.T) {
	h := &echoHandler{}
	s, addr1, errc1 := startServer(t, h, 0)
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	errc2 := make(chan error, 1)
	go func() { errc2 <- s.Serve(ln2) }()
	for _, addr := range []string{addr1, ln2.Addr().String()} {
		if err := roundTrip(t, dial(t, addr), "z"); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for _, errc := range []chan error{errc1, errc2} {
		if err := <-errc; !errors.Is(err, ErrServerClosed) {
			t.Fatalf("Serve = %v", err)
		}
	}
}

package upstream

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// server accepts connections and hands each server side to the test.
type server struct {
	addr  string
	conns chan net.Conn
}

func newServer(t *testing.T) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{addr: ln.Addr().String(), conns: make(chan net.Conn, 64)}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns <- c
		}
	})
	t.Cleanup(func() {
		ln.Close()
		wg.Wait()
		close(s.conns)
		for c := range s.conns {
			c.Close()
		}
	})
	return s
}

// accept returns the server side of the next connection.
func (s *server) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-s.conns:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no connection accepted")
		return nil
	}
}

// noAccept checks that no new connection arrives.
func (s *server) noAccept(t *testing.T) {
	t.Helper()
	select {
	case c := <-s.conns:
		c.Close()
		t.Fatal("unexpected new connection")
	case <-time.After(50 * time.Millisecond):
	}
}

func get(t *testing.T, p *Pool) *Conn {
	t.Helper()
	c, err := p.Get(context.Background(), time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func isClosed(c *Conn) bool {
	_, err := c.Write([]byte("x"))
	return err != nil
}

func TestPoolReusesMostRecentFirst(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{MaxIdle: 4, IdleTimeout: time.Minute})
	a, b := get(t, p), get(t, p)
	s.accept(t)
	sb := s.accept(t)
	if a.Reused() || b.Reused() {
		t.Fatal("fresh connections report reused")
	}
	// A byte read on b counts until b is parked and taken again.
	sb.Write([]byte("z"))
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.R.ReadByte(); err != nil || b.BytesRead() != 1 {
		t.Fatalf("read: %v, BytesRead %d", err, b.BytesRead())
	}
	p.Put(a)
	p.Put(b)
	if p.Idle() != 2 {
		t.Fatalf("idle %d, want 2", p.Idle())
	}
	if got := get(t, p); got != b || !got.Reused() || got.BytesRead() != 0 {
		t.Fatalf("first Get: same as b %v, reused %v, BytesRead %d", got == b, got.Reused(), got.BytesRead())
	}
	if got := get(t, p); got != a {
		t.Fatal("second Get did not return a")
	}
	s.noAccept(t)
}

func TestPoolMaxIdle(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{MaxIdle: 2, IdleTimeout: time.Minute})
	cs := []*Conn{get(t, p), get(t, p), get(t, p)}
	for _, c := range cs {
		p.Put(c)
	}
	if p.Idle() != 2 || isClosed(cs[0]) || isClosed(cs[1]) || !isClosed(cs[2]) {
		t.Fatalf("idle %d; closed %v %v %v, want only the third", p.Idle(), isClosed(cs[0]), isClosed(cs[1]), isClosed(cs[2]))
	}
}

func TestPoolReuseOff(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{})
	if p.Reuse() {
		t.Fatal("Reuse with MaxIdle 0")
	}
	c := get(t, p)
	p.Put(c)
	if p.Idle() != 0 || !isClosed(c) {
		t.Fatal("Put kept a connection with reuse off")
	}
}

// Get skips connections past the idle timeout, and with them every older
// one, and dials.
func TestPoolGetSkipsExpired(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{MaxIdle: 4, IdleTimeout: 50 * time.Millisecond})
	a, b := get(t, p), get(t, p)
	s.accept(t)
	s.accept(t)
	p.Put(a)
	p.Put(b)
	time.Sleep(80 * time.Millisecond)
	c := get(t, p)
	if c == a || c == b || c.Reused() {
		t.Fatal("Get returned an expired connection")
	}
	s.accept(t)
	if p.Idle() != 0 || !isClosed(a) || !isClosed(b) {
		t.Fatalf("expired connections left: idle %d", p.Idle())
	}
}

// A connection the backend closed, or sent bytes on while idle, is
// discarded by Get, which moves on to the next one.
func TestPoolGetSkipsStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(server net.Conn, c *Conn)
	}{
		{"closed by backend", func(sc net.Conn, _ *Conn) { sc.Close() }},
		{"reset by backend", func(sc net.Conn, _ *Conn) {
			sc.(*net.TCPConn).SetLinger(0)
			sc.Close()
		}},
		{"unsolicited bytes", func(sc net.Conn, _ *Conn) { sc.Write([]byte("HTTP/1.1 408 Request Timeout\r\n\r\n")) }},
		{"bytes left in the reader", func(sc net.Conn, c *Conn) {
			sc.Write([]byte("ab"))
			c.SetReadDeadline(time.Now().Add(time.Second))
			c.R.ReadByte()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			p := NewPool(s.addr, Options{MaxIdle: 4, IdleTimeout: time.Minute})
			good, bad := get(t, p), get(t, p)
			s.accept(t)
			sbad := s.accept(t)
			tc.spoil(sbad, bad)
			p.Put(good)
			p.Put(bad)
			// Let the FIN, reset or bytes arrive.
			time.Sleep(20 * time.Millisecond)
			if c := get(t, p); c != good {
				t.Fatal("Get did not skip the stale connection")
			}
			if !isClosed(bad) {
				t.Fatal("stale connection not closed")
			}
			s.noAccept(t)
		})
	}
}

// The liveness check does not block on a healthy idle connection, even one
// whose last read deadline has long passed.
func TestAliveDoesNotBlock(t *testing.T) {
	s := newServer(t)
	c, err := Dial(context.Background(), s.addr, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s.accept(t)
	c.SetDeadline(time.Time{})
	t0 := time.Now()
	if !alive(c.Conn) {
		t.Fatal("healthy connection reported dead")
	}
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("alive took %v", d)
	}
}

func TestPoolReap(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{MaxIdle: 4, IdleTimeout: time.Minute})
	old, young := get(t, p), get(t, p)
	p.Put(old)
	time.Sleep(10 * time.Millisecond)
	mid := time.Now()
	time.Sleep(10 * time.Millisecond)
	p.Put(young)
	// As if the idle timeout had passed for old but not for young.
	p.reap(mid.Add(time.Minute))
	if p.Idle() != 1 || !isClosed(old) || isClosed(young) {
		t.Fatalf("idle %d; old closed %v, young closed %v", p.Idle(), isClosed(old), isClosed(young))
	}
	p.reap(time.Now().Add(2 * time.Minute))
	if p.Idle() != 0 || !isClosed(young) {
		t.Fatal("reap left an expired connection")
	}
}

func TestPoolClose(t *testing.T) {
	s := newServer(t)
	p := NewPool(s.addr, Options{MaxIdle: 4, IdleTimeout: time.Minute})
	a, b := get(t, p), get(t, p)
	p.Put(a)
	p.Close()
	p.Put(b)
	if p.Idle() != 0 || !isClosed(a) || !isClosed(b) {
		t.Fatal("closed pool kept connections")
	}
}

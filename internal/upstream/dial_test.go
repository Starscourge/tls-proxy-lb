package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestDialAndExchange(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		io.ReadFull(c, buf)
		c.Write([]byte("pong"))
	}()
	c, err := Dial(context.Background(), ln.Addr().String(), time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.W.WriteString("ping")
	if err := c.W.Flush(); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c.R, got); err != nil || string(got) != "pong" {
		t.Fatalf("read %q, %v", got, err)
	}
	<-done
}

func TestDialRefused(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, err := Dial(context.Background(), addr, time.Second, time.Second)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want connection refused", err)
	}
}

func TestDialTimeout(t *testing.T) {
	addr := hangingAddr(t)
	t0 := time.Now()
	_, err := Dial(context.Background(), addr, 200*time.Millisecond, time.Second)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if d := time.Since(t0); d < 150*time.Millisecond || d > 2*time.Second {
		t.Fatalf("dial gave up after %v, want about 200ms", d)
	}
}

// hangingAddr returns an address where new connections hang in SYN_SENT: a
// listener with backlog 0 that never accepts, with its one queue slot taken.
func hangingAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	addr := (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sa.(*syscall.SockaddrInet4).Port}).String()
	// Fill the accept queue; later SYNs are dropped.
	for range 4 {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		t.Cleanup(func() { c.Close() })
	}
	return addr
}

package conn

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// panicOnFirst panics on the first connection and echoes on the rest.
type panicOnFirst struct{ n atomic.Int64 }

func (h *panicOnFirst) ServeConn(ctx context.Context, c *TrackedConn) {
	if h.n.Add(1) == 1 {
		panic("deliberate")
	}
	(&echoHandler{}).ServeConn(ctx, c)
}

// Done when: a panic in a connection goroutine is recovered, logged and
// counted; its connection is closed and the server keeps serving.
func TestServerRecoversPanic(t *testing.T) {
	logs := &logBuf{}
	s := NewServer(Limits{}, slog.New(slog.NewJSONHandler(logs, nil)))
	var panics atomic.Int64
	s.OnPanic = func() { panics.Add(1) }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, &panicOnFirst{}) }()
	defer func() { s.Close(); <-errc }()

	c := dial(t, ln.Addr().String())
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("panicking connection not closed: %v", err)
	}
	waitFor(t, "connection removed", func() bool { return s.Tracker().Len() == 0 })
	if panics.Load() != 1 {
		t.Fatalf("OnPanic called %d times", panics.Load())
	}
	recs := logs.records("panic serving connection; closing it")
	if len(recs) != 1 || recs[0]["panic"] != "deliberate" || !strings.Contains(recs[0]["stack"].(string), "panicOnFirst") {
		t.Fatalf("panic log %v", recs)
	}
	if err := roundTrip(t, dial(t, ln.Addr().String()), "still serving"); err != nil {
		t.Fatalf("after the panic: %v", err)
	}
}

// Done when: a deliberately panicking path in the proxy itself (the request
// handler on the connection goroutine, and the request-body writer on its
// own goroutine) is recovered and counted, and the proxy keeps serving.
func TestProxyRecoversPanic(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	m := metrics.New(metrics.Sources{})
	addr, s, logs := startProxyWith(t, newSnapshot(t, parseConfig(t, "default_pool = \"p\"\n[pool.p]\nbackends = [{ address = \""+baddr+"\" }]\n")),
		func(p *Proxy) {
			p.Metrics = m
			p.onRequest = func(r *http.Request) {
				if r.URL.Path == "/panic" {
					panic("in the request path")
				}
			}
			p.afterRequestWrite = func() { panic("in the request writer") }
		})
	s.OnPanic = m.Panic

	// The connection goroutine.
	cl := dialClient(t, addr)
	cl.send("GET /panic HTTP/1.1\r\nHost: a\r\n\r\n")
	cl.expectClosed(2 * time.Second)
	waitFor(t, "first panic counted", func() bool { return m.Panics() == 1 })

	// The writer goroutine, which only runs for a request with a body. It
	// fails that request; the connection goroutine answers and closes.
	cl = dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\n\r\nabc")
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if resp, err := http.ReadResponse(cl.br, nil); err == nil && resp.StatusCode == 200 {
		t.Fatal("request whose writer panicked answered 200")
	}
	waitFor(t, "second panic counted", func() bool { return m.Panics() == 2 })
	if n := len(logs.records("panic writing a request upstream; failing it")); n != 1 {
		t.Fatalf("%d writer panics logged", n)
	}
	waitFor(t, "connections closed", func() bool { return s.Tracker().Len() == 0 })

	// Still serving.
	cl = dialClient(t, addr)
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("after the panics: status %d", resp.StatusCode)
	}
}

// emfileListener fails every Accept with EMFILE, as a listener does when
// the process is out of file descriptors, and counts the calls.
type emfileListener struct {
	net.Listener
	calls  atomic.Int64
	closed chan struct{}
}

func (l *emfileListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
}

func (l *emfileListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *emfileListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

// Accept failing with EMFILE backs off, doubling from 5 ms, instead of
// spinning; the failure is counted each time and logged at most once a
// second; and closing the server ends a long backoff at once.
func TestAcceptBackoff(t *testing.T) {
	logs := &logBuf{}
	s := NewServer(Limits{}, slog.New(slog.NewJSONHandler(logs, nil)))
	var errs atomic.Int64
	s.OnAcceptError = func() { errs.Add(1) }
	ln := &emfileListener{closed: make(chan struct{})}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, &echoHandler{}) }()

	// 5+10+20+40+80+160+320+640 ms = 1,275 ms: eight or nine calls in
	// 1.4 s, where a fixed 10 ms retry would make about 140.
	time.Sleep(1400 * time.Millisecond)
	calls := ln.calls.Load()
	if calls < 6 || calls > 11 {
		t.Fatalf("%d Accept calls in 1.4 s, want about 9", calls)
	}
	if errs.Load() != calls {
		t.Fatalf("%d errors counted for %d calls", errs.Load(), calls)
	}
	if n := len(logs.records("accept failed; backing off")); n < 1 || n > 2 {
		t.Fatalf("%d log lines in 1.4 s, want at most one a second", n)
	}
	start := time.Now()
	s.Close()
	if err := <-errc; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve: %v", err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("Close waited %v for the backoff", el)
	}
}

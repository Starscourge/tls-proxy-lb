package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/conn"
)

// ProxyOptions configures StartProxy. Zero fields take the proxy's defaults,
// so a test sets only what it is about.
type ProxyOptions struct {
	Timeouts       conn.Timeouts
	MaxConns       int
	MaxHeaderBytes int
	// Logger receives the access log. Nil logs to the test output.
	Logger *slog.Logger
}

// Proxy is a proxy running in-process on a random localhost port.
type Proxy struct {
	*conn.Server
	Addr string
	URL  string
}

// StartProxy starts a proxy in front of backend. It is closed when the test
// ends.
func StartProxy(t testing.TB, backend string, opts ProxyOptions) *Proxy {
	t.Helper()
	def := conn.DefaultTimeouts()
	to := conn.Timeouts{
		HeaderRead:       orDefault(opts.Timeouts.HeaderRead, def.HeaderRead),
		Idle:             orDefault(opts.Timeouts.Idle, def.Idle),
		ClientIO:         orDefault(opts.Timeouts.ClientIO, def.ClientIO),
		UpstreamConnect:  orDefault(opts.Timeouts.UpstreamConnect, def.UpstreamConnect),
		UpstreamResponse: orDefault(opts.Timeouts.UpstreamResponse, def.UpstreamResponse),
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(t.Output(), nil))
	}
	p := &conn.Proxy{Backend: backend, Timeouts: to, MaxHeaderBytes: opts.MaxHeaderBytes, Logger: logger}
	if err := p.Validate(); err != nil {
		t.Fatalf("proxy config: %v", err)
	}
	srv := conn.NewServer(p, opts.MaxConns, logger)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-errc; !errors.Is(err, conn.ErrServerClosed) {
			t.Errorf("proxy Serve: %v", err)
		}
	})
	addr := ln.Addr().String()
	return &Proxy{Server: srv, Addr: addr, URL: "http://" + addr}
}

func orDefault(d, def time.Duration) time.Duration {
	if d != 0 {
		return d
	}
	return def
}

// RawBackend is a TCP server whose connections are handled by test code,
// for backends that misbehave in ways the test backend cannot.
type RawBackend struct {
	Addr     string
	Accepted atomic.Int64
}

// StartRawBackend runs handle in its own goroutine for each accepted
// connection. When the test ends the listener and every connection are
// closed and the handlers are waited for, so handle must return once its
// connection is closed.
func StartRawBackend(t testing.TB, handle func(c net.Conn)) *RawBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rb := &RawBackend{Addr: ln.Addr().String()}
	var (
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
		wg    sync.WaitGroup
	)
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			rb.Accepted.Add(1)
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
			wg.Go(func() {
				defer func() {
					c.Close()
					mu.Lock()
					delete(conns, c)
					mu.Unlock()
				}()
				handle(c)
			})
		}
	})
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return rb
}

// LogBuffer collects JSON log lines for assertions.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLogBuffer returns a buffer and a logger writing JSON lines into it.
func NewLogBuffer() (*LogBuffer, *slog.Logger) {
	lb := &LogBuffer{}
	return lb, slog.New(slog.NewJSONHandler(lb, nil))
}

func (lb *LogBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

// Records returns every logged line with message msg, decoded.
func (lb *LogBuffer) Records(msg string) []map[string]any {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(lb.buf.Bytes()))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return out
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
}

// RefusedAddr returns a localhost address that refuses connections: a
// socket bound but not listening, held until the test ends so no other
// socket can take the port.
func RefusedAddr(t testing.TB) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sa.(*syscall.SockaddrInet4).Port}).String()
}

// discard returns a logger that drops everything, for tests whose request
// volume would flood the test output.
func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

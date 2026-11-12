package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
)

// ProxyOptions configures StartProxy and StartProxyConfig. Zero fields take
// the proxy's defaults, so a test sets only what it is about.
type ProxyOptions struct {
	// Timeouts are written to the config's [timeouts] table, in whole
	// milliseconds.
	Timeouts       config.Timeouts
	MaxConns       int
	MaxHeaderBytes int
	// Logger receives the access log. Nil logs to the test output.
	Logger *slog.Logger
}

// Proxy is a proxy running in-process on a random localhost port.
type Proxy struct {
	*conn.Server
	// Snapshot is the config the proxy serves, for reaching its pools.
	Snapshot *config.Snapshot
	// ConfigPath is the generated config file.
	ConfigPath string
	Addr       string
	URL        string
}

// StartProxy starts a proxy whose default pool holds backend alone. It is
// closed when the test ends.
func StartProxy(t testing.TB, backend string, opts ProxyOptions) *Proxy {
	t.Helper()
	return StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(backend), opts)
}

// StartProxyConfig starts a proxy from a generated config file. body holds
// the top-level keys, routes and pools; the harness appends a listener on a
// random localhost port and the [timeouts] and [limits] tables from opts, so
// body must not contain those. The file goes through config.Load like the
// real one. The proxy is closed when the test ends.
func StartProxyConfig(t testing.TB, body string, opts ProxyOptions) *Proxy {
	t.Helper()
	var src strings.Builder
	src.WriteString(body)
	src.WriteString("\n[[listener]]\naddress = \"127.0.0.1:0\"\n\n[timeouts]\n")
	for _, f := range []struct {
		key string
		d   time.Duration
	}{
		{"header_read_ms", opts.Timeouts.HeaderRead},
		{"idle_ms", opts.Timeouts.Idle},
		{"client_io_ms", opts.Timeouts.ClientIO},
		{"upstream_connect_ms", opts.Timeouts.UpstreamConnect},
		{"upstream_response_ms", opts.Timeouts.UpstreamResponse},
	} {
		if f.d != 0 {
			fmt.Fprintf(&src, "%s = %d\n", f.key, f.d.Milliseconds())
		}
	}
	src.WriteString("\n[limits]\n")
	if opts.MaxConns != 0 {
		fmt.Fprintf(&src, "max_conns = %d\n", opts.MaxConns)
	}
	if opts.MaxHeaderBytes != 0 {
		fmt.Fprintf(&src, "max_header_bytes = %d\n", opts.MaxHeaderBytes)
	}

	path := filepath.Join(t.TempDir(), "proxy.toml")
	if err := os.WriteFile(path, []byte(src.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("proxy config:\n%s\n%v", src.String(), err)
	}
	snap, err := config.NewSnapshot(cfg)
	if err != nil {
		t.Fatalf("proxy snapshot: %v", err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(t.Output(), nil))
	}
	p := &conn.Proxy{Config: config.NewStore(snap), Logger: logger}
	if err := p.Validate(); err != nil {
		t.Fatalf("proxy: %v", err)
	}
	srv := conn.NewServer(snap.Limits.MaxConns, logger)
	ln, err := net.Listen("tcp", snap.Listeners[0])
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln, p) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-errc; !errors.Is(err, conn.ErrServerClosed) {
			t.Errorf("proxy Serve: %v", err)
		}
	})
	addr := ln.Addr().String()
	return &Proxy{Server: srv, Snapshot: snap, ConfigPath: path, Addr: addr, URL: "http://" + addr}
}

// Backends renders a pool's backends key for the given addresses.
func Backends(addrs ...string) string {
	var b strings.Builder
	b.WriteString("backends = [\n")
	for _, a := range addrs {
		fmt.Fprintf(&b, "  { address = %q },\n", a)
	}
	b.WriteString("]\n")
	return b.String()
}

// Addrs returns the backends' addresses.
func Addrs(bs []*Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Addr
	}
	return out
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

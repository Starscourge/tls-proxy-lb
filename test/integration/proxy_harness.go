package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/admin"
	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// Schemes are the listener modes the Phase 1 and 2 tests run under.
var Schemes = []string{config.ModeHTTP, config.ModeHTTPS}

// ForEachScheme runs fn as one subtest per scheme, so a test written once
// covers plain HTTP and HTTPS.
func ForEachScheme(t *testing.T, fn func(t *testing.T, scheme string)) {
	t.Helper()
	for _, scheme := range Schemes {
		t.Run(scheme, func(t *testing.T) { fn(t, scheme) })
	}
}

// ProxyOptions configures StartProxy and StartProxyConfig. Zero fields take
// the proxy's defaults, so a test sets only what it is about.
type ProxyOptions struct {
	// Scheme is "http" (the default) or "https". For "https" the harness
	// issues a CA and a default certificate for *.test, so every host under
	// .test completes a handshake unless the config gives it a certificate
	// of its own.
	Scheme string
	// NoDefaultCert leaves the *.test default certificate out (HTTPS only),
	// so names without a certificate of their own fail the handshake.
	NoDefaultCert bool
	// Timeouts are written to the config's [timeouts] table, in whole
	// milliseconds.
	Timeouts       config.Timeouts
	MaxConns       int
	MaxHeaderBytes int
	// MaxIdlePerBackend and UpstreamIdle go to the [upstream] table;
	// NoReuse writes max_idle_per_backend = 0, turning reuse off.
	MaxIdlePerBackend int
	UpstreamIdle      time.Duration
	NoReuse           bool
	// MaxRetries, if set, goes to upstream.max_retries.
	MaxRetries *int
	// Health, if set, is written to the [health] table. Nil turns active
	// and passive checks off, so tests about other things see neither
	// probe requests nor ejections.
	Health *health.Options
	// Admin starts the admin listener (/metrics, /status) on a random
	// loopback port.
	Admin bool
	// Logger receives the access log. Nil logs to the test output.
	Logger *slog.Logger
}

// Proxy is a proxy running in-process on a random localhost port.
type Proxy struct {
	*conn.Server
	// Snapshot is the config the proxy serves, for reaching its pools.
	Snapshot *config.Snapshot
	// Health and Metrics are the proxy's health checker and metrics.
	Health  *health.Checker
	Metrics *metrics.Metrics
	// AdminURL is http://host:port of the admin listener, if
	// ProxyOptions.Admin was set.
	AdminURL string
	// ConfigPath is the generated config file; Dir is its directory, where
	// certificates can be written for the config to name.
	ConfigPath string
	Dir        string
	// Scheme is "http" or "https".
	Scheme string
	// CA signed the default certificate (HTTPS only). Tests may issue more
	// certificates with it.
	CA   *tlstest.CA
	Addr string
	// URL is Scheme://a.test:port. Clients from Client resolve every host
	// to the proxy, so the host in a URL is what the proxy sees as SNI and
	// Host.
	URL string
}

// StartProxy starts a proxy whose default pool holds backend alone. It is
// closed when the test ends.
func StartProxy(t testing.TB, backend string, opts ProxyOptions) *Proxy {
	t.Helper()
	return StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(backend), opts)
}

// StartProxyConfig starts a proxy from a generated config file. body holds
// the top-level keys, routes and pools; the harness appends a listener on a
// random localhost port and the [timeouts], [limits] and [upstream] tables
// from opts, so body must not contain those. The file goes through config.Load like the
// real one. The proxy is closed when the test ends.
//
// For HTTPS the harness also appends a [tls] table, so body must not
// contain one. Relative certificate paths in body resolve against the
// config's directory; use StartProxyDir to write certificates there first.
func StartProxyConfig(t testing.TB, body string, opts ProxyOptions) *Proxy {
	t.Helper()
	return StartProxyDir(t, t.TempDir(), nil, body, opts)
}

// StartProxyDir is StartProxyConfig with the config written into dir, so
// body can name certificate files the test wrote there, signed by ca (a
// new CA if nil).
func StartProxyDir(t testing.TB, dir string, ca *tlstest.CA, body string, opts ProxyOptions) *Proxy {
	t.Helper()
	if opts.Scheme == "" {
		opts.Scheme = config.ModeHTTP
	}
	var src strings.Builder
	src.WriteString(body)
	fmt.Fprintf(&src, "\n[[listener]]\naddress = \"127.0.0.1:0\"\nmode = %q\n", opts.Scheme)
	if opts.Scheme == config.ModeHTTPS {
		if ca == nil {
			ca = tlstest.NewCA(t)
		}
		if !opts.NoDefaultCert {
			ca.WriteFiles(t, dir, "harness-default", tlstest.LeafOptions{}, "*.test")
			src.WriteString("\n[tls]\ndefault_cert = \"harness-default.crt\"\ndefault_key = \"harness-default.key\"\n")
		}
	}
	src.WriteString("\n[timeouts]\n")
	for _, f := range []struct {
		key string
		d   time.Duration
	}{
		{"handshake_ms", opts.Timeouts.Handshake},
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

	src.WriteString("\n[upstream]\n")
	switch {
	case opts.NoReuse:
		src.WriteString("max_idle_per_backend = 0\n")
	case opts.MaxIdlePerBackend != 0:
		fmt.Fprintf(&src, "max_idle_per_backend = %d\n", opts.MaxIdlePerBackend)
	}
	if opts.UpstreamIdle != 0 {
		fmt.Fprintf(&src, "idle_timeout_ms = %d\n", opts.UpstreamIdle.Milliseconds())
	}
	if opts.MaxRetries != nil {
		fmt.Fprintf(&src, "max_retries = %d\n", *opts.MaxRetries)
	}

	src.WriteString("\n[health]\n")
	if h := opts.Health; h != nil {
		fmt.Fprintf(&src, "path = %q\ninterval_ms = %d\ntimeout_ms = %d\nrise = %d\nfall = %d\npassive_failures = %d\neject_ms = %d\n",
			h.Path, h.Interval.Milliseconds(), h.Timeout.Milliseconds(), h.Rise, h.Fall, h.PassiveFailures, h.Eject.Milliseconds())
	} else {
		src.WriteString("interval_ms = 0\npassive_failures = 0\n")
	}

	path := filepath.Join(dir, "proxy.toml")
	if err := os.WriteFile(path, []byte(src.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("proxy config:\n%s\n%v", src.String(), err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(t.Output(), nil))
	}
	// The same wiring as cmd/proxy's run.
	reg := upstream.NewRegistry(cfg.Upstream.Options())
	t.Cleanup(reg.Close)
	chk := health.NewChecker(health.Config{
		Options: cfg.Health,
		Logger:  logger,
		OnEject: func(addr string) { reg.Pool(addr).Flush() },
	})
	t.Cleanup(chk.Close)
	srv := conn.NewServer(cfg.Limits.MaxConns, logger)
	m := metrics.New(metrics.Sources{Upstream: reg, Health: chk, ClientConns: srv.Tracker().Len})
	snap, err := config.NewSnapshot(cfg, config.Runtime{Conns: reg, Health: chk, Metrics: m})
	if err != nil {
		t.Fatalf("proxy snapshot: %v", err)
	}
	store := config.NewStore(snap)
	p := &conn.Proxy{Config: store, Logger: logger, Metrics: m}
	if opts.Scheme == config.ModeHTTPS {
		p.TLS = tlsx.ServerConfig(func() *tlsx.CertSet { return store.Load().Certs })
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("proxy: %v", err)
	}
	ln, err := net.Listen("tcp", snap.Listeners[0].Address)
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
	px := &Proxy{
		Server: srv, Snapshot: snap, Health: chk, Metrics: m,
		ConfigPath: path, Dir: dir, Scheme: opts.Scheme, CA: ca, Addr: addr,
		URL: opts.Scheme + "://a.test:" + port(addr),
	}
	if opts.Admin {
		adm, err := admin.Listen(context.Background(), admin.Options{
			Address: "127.0.0.1:0",
			Metrics: m.Handler(),
			Status:  admin.StatusSource{Config: store, Health: chk, Conns: reg},
		}, logger)
		if err != nil {
			t.Fatal(err)
		}
		admc := make(chan error, 1)
		go func() { admc <- adm.Serve() }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			adm.Shutdown(ctx)
			if err := <-admc; err != nil {
				t.Errorf("admin Serve: %v", err)
			}
		})
		px.AdminURL = "http://" + adm.Addr()
	}
	return px
}

// Status fetches and decodes the admin /status page. The proxy must have
// been started with ProxyOptions.Admin.
func (p *Proxy) Status(t testing.TB) admin.Status {
	t.Helper()
	if p.AdminURL == "" {
		t.Fatal("Status: proxy started without Admin")
	}
	c := &http.Client{Timeout: 5 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Get(p.AdminURL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st admin.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("/status: %v", err)
	}
	return st
}

// BackendStatus returns addr's entry in st, or fails the test.
func BackendStatus(t testing.TB, st admin.Status, addr string) admin.BackendStatus {
	t.Helper()
	for _, b := range st.Backends {
		if b.Address == addr {
			return b
		}
	}
	t.Fatalf("%s not in /status", addr)
	return admin.BackendStatus{}
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// URLFor returns Scheme://host:port, the proxy's URL under another host.
func (p *Proxy) URLFor(host string) string {
	return p.Scheme + "://" + host + ":" + port(p.Addr)
}

// Client returns an HTTP client that sends every request to the proxy,
// whatever host its URL names, and for HTTPS trusts only the harness CA.
// The URL's host is the SNI. Idle connections are closed when the test
// ends.
func (p *Proxy) Client(t testing.TB) *http.Client {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, p.Addr)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   64,
	}
	if p.CA != nil {
		tr.TLSClientConfig = p.CA.ClientConfig("")
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

// Dial opens a connection to the proxy for writing raw HTTP: plain TCP, or
// for HTTPS a TLS connection with host as SNI, handshake done. tune, if
// set, adjusts the TCP socket before the handshake. The connection is
// closed when the test ends.
func (p *Proxy) Dial(t testing.TB, host string, tune func(*net.TCPConn)) net.Conn {
	t.Helper()
	c, err := p.DialErr(host, tune)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// DialErr is Dial for goroutines other than the test's: it returns the
// error, and the caller closes the connection.
func (p *Proxy) DialErr(host string, tune func(*net.TCPConn)) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", p.Addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if tune != nil {
		tune(c.(*net.TCPConn))
	}
	if p.Scheme != config.ModeHTTPS {
		return c, nil
	}
	tc := tls.Client(c, p.CA.ClientConfig(host))
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("TLS handshake with SNI %q: %w", host, err)
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
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

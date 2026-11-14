package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// logBuf collects JSON log lines; it is safe for concurrent use.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// records returns every log record whose msg starts with prefix.
func (l *logBuf) records(prefix string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.buf.String()) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && strings.HasPrefix(fmt.Sprint(r["msg"]), prefix) {
			out = append(out, r)
		}
	}
	return out
}

// startBackend runs a test backend named name and returns its address.
func startBackend(t *testing.T, name string) string {
	t.Helper()
	b, err := testbackend.New(testbackend.Config{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- b.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Shutdown(ctx)
		<-errc
	})
	return ln.Addr().String()
}

// freeAddr returns a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// plainConfig is a config with one plain HTTP listener on addr and a
// default pool of backends, plus extra appended.
func plainConfig(addr string, backends []string, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "default_pool = \"p\"\n\n[[listener]]\naddress = %q\nmode = \"http\"\n\n[pool.p]\nbackends = [", addr)
	for i, a := range backends {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "{ address = %q }", a)
	}
	b.WriteString("]\n\n[health]\ninterval_ms = 0\npassive_failures = 0\n")
	b.WriteString(extra)
	return b.String()
}

// harness is a running App and the config file it reloads.
type harness struct {
	t    *testing.T
	app  *App
	path string
	logs *logBuf
	l    Listening
}

// start writes src to a config file and runs an App on it until the test
// ends.
func start(t *testing.T, src string) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "proxy.toml"), logs: &logBuf{}}
	h.write(src)
	cfg, err := config.Load(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.app, err = New(cfg, Options{Logger: slog.New(slog.NewJSONHandler(h.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	h.l, err = h.app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.app.Shutdown(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return h
}

// write replaces the config file atomically, as a deploy tool would.
func (h *harness) write(src string) {
	h.t.Helper()
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		h.t.Fatal(err)
	}
}

// reload writes src and reloads it.
func (h *harness) reload(src string) error {
	h.t.Helper()
	h.write(src)
	return h.app.Reload(h.path)
}

// get sends one request on a new connection to addr and returns the
// status and the backend that answered.
func get(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	c := dialHTTP(t, addr)
	defer c.Close()
	resp := c.do(t, "GET "+path+" HTTP/1.1\r\nHost: a.test\r\nConnection: close\r\n\r\n")
	return resp.StatusCode, resp.Header.Get(testbackend.HeaderBackend)
}

// httpConn is a raw client connection that can carry several requests.
type httpConn struct {
	net.Conn
	br *bufio.Reader
}

func dialHTTP(t *testing.T, addr string) *httpConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &httpConn{Conn: c, br: bufio.NewReader(c)}
}

// do sends raw and reads the response, body included.
func (c *httpConn) do(t *testing.T, raw string) *http.Response {
	t.Helper()
	resp, err := c.try(raw)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (c *httpConn) try(raw string) (*http.Response, error) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, err
}

const keepAliveGet = "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n"

func addrsOf[T interface{ Addr() string }](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = x.Addr()
	}
	return out
}

// A reload changes the pool for new requests; the removed backend's prober,
// idle connections and series are gone, and the reload is counted.
func TestReloadSwapsPool(t *testing.T) {
	b1, b2 := startBackend(t, "b1"), startBackend(t, "b2")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	addr := h.l.Addrs[0]
	// A kept-alive connection parks an upstream connection to b1.
	c := dialHTTP(t, addr)
	if resp := c.do(t, keepAliveGet); resp.Header.Get(testbackend.HeaderBackend) != "b1" {
		t.Fatalf("before reload: %q", resp.Header.Get(testbackend.HeaderBackend))
	}
	idle := h.app.Upstream().Lookup(b1)
	if idle == nil || idle.Idle() != 1 {
		t.Fatal("no idle upstream connection to b1")
	}

	if err := h.reload(plainConfig("127.0.0.1:0", []string{b2}, "")); err != nil {
		t.Fatal(err)
	}
	if got := h.app.Addrs(); !slices.Equal(got, h.l.Addrs) {
		t.Fatalf("listener changed from %v to %v", h.l.Addrs, got)
	}
	// The same client connection's next request uses the new config.
	if resp := c.do(t, keepAliveGet); resp.Header.Get(testbackend.HeaderBackend) != "b2" {
		t.Fatalf("after reload: %q", resp.Header.Get(testbackend.HeaderBackend))
	}
	if _, be := get(t, addr, "/"); be != "b2" {
		t.Fatalf("new connection after reload: %q", be)
	}
	if got := addrsOf(h.app.Health().Targets()); !slices.Equal(got, []string{b2}) {
		t.Fatalf("health targets %v, want [%s]", got, b2)
	}
	if h.app.Upstream().Lookup(b1) != nil || idle.Idle() != 0 {
		t.Fatal("removed backend's upstream pool kept")
	}
	if m := h.app.Metrics(); m.Reloads(true) != 1 || m.Reloads(false) != 0 {
		t.Fatalf("reloads: %d applied, %d failed", m.Reloads(true), m.Reloads(false))
	}
	if len(h.logs.records("config reloaded")) != 1 {
		t.Fatal("reload not logged")
	}
}

// A backend in both configs keeps its health state, its idle connections
// and its in-flight count.
func TestReloadCarriesBackendState(t *testing.T) {
	b1, b2 := startBackend(t, "b1"), startBackend(t, "b2")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	c := dialHTTP(t, h.l.Addrs[0])
	c.do(t, keepAliveGet)
	pool, target := h.app.Upstream().Lookup(b1), h.app.Health().Target(b1)
	stats := h.app.Store().Load().Pool("p").Backends[0].Stats
	stats.InFlight.Add(3) // as if three requests were in flight

	if err := h.reload(plainConfig("127.0.0.1:0", []string{b1, b2}, "")); err != nil {
		t.Fatal(err)
	}
	nb := h.app.Store().Load().Pool("p").Backends[0]
	if nb.Addr != b1 || nb.Conns != pool || nb.Health != target || nb.Stats != stats || nb.InFlight() != 3 {
		t.Fatal("b1's connections, health or counters were not carried across")
	}
	if pool.Idle() != 1 {
		t.Fatalf("b1 has %d idle connections after reload, want 1", pool.Idle())
	}
	stats.InFlight.Add(-3)
}

// A request in flight during a reload finishes on the old config.
func TestReloadDuringRequest(t *testing.T) {
	b1, b2 := startBackend(t, "b1"), startBackend(t, "b2")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	type result struct {
		status  int
		backend string
	}
	done := make(chan result, 1)
	c := dialHTTP(t, h.l.Addrs[0])
	go func() {
		resp, err := c.try("GET /slow?ms=500 HTTP/1.1\r\nHost: a.test\r\n\r\n")
		if err != nil {
			done <- result{}
			return
		}
		done <- result{resp.StatusCode, resp.Header.Get(testbackend.HeaderBackend)}
	}()
	waitFor(t, "request in flight", func() bool { return h.app.Store().Load().Pool("p").Backends[0].InFlight() == 1 })
	if err := h.reload(plainConfig("127.0.0.1:0", []string{b2}, "")); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.status != 200 || r.backend != "b1" {
		t.Fatalf("in-flight request: %d from %q, want 200 from b1", r.status, r.backend)
	}
	if _, be := get(t, h.l.Addrs[0], "/"); be != "b2" {
		t.Fatalf("after reload: %q", be)
	}
}

// A config that does not load, or cannot be applied, leaves the old one
// serving, is logged, counted, and leaves no prober or pool behind for the
// backends it named.
func TestReloadBrokenConfig(t *testing.T) {
	b1, b3 := startBackend(t, "b1"), startBackend(t, "b3")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	good := plainConfig("127.0.0.1:0", []string{b1}, "")
	h := start(t, good)
	for i, tc := range []struct {
		name, src, err string
	}{
		{"toml syntax", good + "\n[pool.p\n", "proxy.toml"},
		{"validation", plainConfig("127.0.0.1:0", []string{b3}, "[[route]]\nhost = \"a.test\"\npool = \"nope\"\n"), `no pool named "nope"`},
		{"missing certificate", plainConfig("127.0.0.1:0", []string{b3}, "[tls]\ndefault_cert = \"missing.crt\"\ndefault_key = \"missing.key\"\n"), "tls.default_cert"},
		{"busy port", plainConfig("127.0.0.1:0", []string{b3}, fmt.Sprintf("[[listener]]\naddress = %q\nmode = \"http\"\n", busy.Addr())), "listen " + busy.Addr().String()},
		{"missing file", "", "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.src == "" {
				os.Remove(h.path)
				defer h.write(good)
				err = h.app.Reload(h.path)
			} else {
				err = h.reload(tc.src)
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Reload = %v, want an error containing %q", err, tc.err)
			}
			if st, be := get(t, h.l.Addrs[0], "/"); st != 200 || be != "b1" {
				t.Fatalf("after a failed reload: %d from %q, want 200 from b1", st, be)
			}
			m := h.app.Metrics()
			if m.Reloads(false) != uint64(i+1) || m.Reloads(true) != 0 {
				t.Fatalf("reloads: %d failed, %d applied", m.Reloads(false), m.Reloads(true))
			}
			if got := addrsOf(h.app.Health().Targets()); !slices.Equal(got, []string{b1}) {
				t.Fatalf("health targets %v after a failed reload", got)
			}
			if got := addrsOf(h.app.Upstream().Pools()); !slices.Equal(got, []string{b1}) {
				t.Fatalf("upstream pools %v after a failed reload", got)
			}
			if got := h.app.Addrs(); !slices.Equal(got, h.l.Addrs) {
				t.Fatalf("listeners %v after a failed reload", got)
			}
			if n := len(h.logs.records("config reload failed")); n != i+1 {
				t.Fatalf("%d failures logged, want %d", n, i+1)
			}
		})
	}
	// The failure gauge clears on the next good reload.
	if err := h.reload(good); err != nil {
		t.Fatal(err)
	}
	if h.app.Metrics().Reloads(true) != 1 {
		t.Fatal("good reload after failures not counted")
	}
}

// Listeners: a reload opens added ports, closes removed ones (their open
// connections carry on), and switches a kept port's mode for new
// connections only.
func TestReloadListeners(t *testing.T) {
	b1 := startBackend(t, "b1")
	fixed := freeAddr(t)
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	first := h.l.Addrs[0]
	onFirst := dialHTTP(t, first)
	onFirst.do(t, keepAliveGet)

	// Add a listener on a fixed port, keeping the first.
	both := plainConfig("127.0.0.1:0", []string{b1}, fmt.Sprintf("[[listener]]\naddress = %q\nmode = \"http\"\n", fixed))
	if err := h.reload(both); err != nil {
		t.Fatal(err)
	}
	if got := h.app.Addrs(); !slices.Equal(got, []string{first, fixed}) {
		t.Fatalf("listeners %v, want [%s %s]", got, first, fixed)
	}
	if st, _ := get(t, fixed, "/"); st != 200 {
		t.Fatalf("added listener: %d", st)
	}
	onFixed := dialHTTP(t, fixed)
	onFixed.do(t, keepAliveGet)

	// Remove the first, and turn the fixed one into a redirect listener.
	redirect := plainConfig(fixed, []string{b1}, "")
	redirect = strings.Replace(redirect, `mode = "http"`, "mode = \"redirect\"\nredirect_port = 8443", 1)
	if err := h.reload(redirect); err != nil {
		t.Fatal(err)
	}
	if got := h.app.Addrs(); !slices.Equal(got, []string{fixed}) {
		t.Fatalf("listeners %v, want [%s]", got, fixed)
	}
	if c, err := net.DialTimeout("tcp", first, time.Second); err == nil {
		c.Close()
		t.Fatal("removed listener still accepts")
	}
	if resp := onFirst.do(t, keepAliveGet); resp.StatusCode != 200 {
		t.Fatalf("connection on the removed listener: %d", resp.StatusCode)
	}
	if resp := onFixed.do(t, keepAliveGet); resp.StatusCode != 200 {
		t.Fatalf("connection accepted before the mode change: %d", resp.StatusCode)
	}
	if st, _ := get(t, fixed, "/"); st != http.StatusPermanentRedirect {
		t.Fatalf("new connection after the mode change: %d, want 308", st)
	}
}

// Changed startup-only settings are logged and keep their running values;
// the rest of the reload applies.
func TestReloadStartupOnly(t *testing.T) {
	b1 := startBackend(t, "b1")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	src := plainConfig("127.0.0.1:0", []string{b1}, "[limits]\nmax_conns = 7\n[upstream]\nmax_idle_per_backend = 3\nmax_retries = 0\n")
	src = strings.Replace(src, "interval_ms = 0", "interval_ms = 5000", 1)
	if err := h.reload(src); err != nil {
		t.Fatal(err)
	}
	recs := h.logs.records("config reload: settings that apply only after a restart")
	if len(recs) != 1 || fmt.Sprint(recs[0]["settings"]) != "[health upstream.max_idle_per_backend]" {
		t.Fatalf("warning %v", recs)
	}
	snap := h.app.Store().Load()
	if snap.Health.Interval != 0 || snap.Upstream.MaxIdlePerBackend != config.DefaultUpstream().MaxIdlePerBackend {
		t.Fatal("a startup-only setting took its new value")
	}
	if snap.Upstream.MaxRetries != 0 || h.app.Server().Tracker().Limits().MaxConns != 7 {
		t.Fatal("a reloadable setting kept its old value")
	}
}

// Shutdown lets a request in flight finish with Connection: close, closes
// idle connections at once, refuses new ones, and stops every goroutine
// (goleak checks in TestMain); Reload afterwards is refused.
func TestShutdownDrains(t *testing.T) {
	b1 := startBackend(t, "b1")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	addr := h.l.Addrs[0]
	idle := dialHTTP(t, addr)
	idle.do(t, keepAliveGet)
	busy := dialHTTP(t, addr)
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := busy.try("GET /slow?ms=500 HTTP/1.1\r\nHost: a.test\r\n\r\n")
		done <- result{resp, err}
	}()
	waitFor(t, "request in flight", func() bool { return h.app.Store().Load().Pool("p").Backends[0].InFlight() == 1 })

	shut := make(chan error, 1)
	go func() { shut <- h.app.Shutdown() }()
	waitFor(t, "draining", h.app.Server().Tracker().Draining)
	idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.br.ReadByte(); err == nil || isTimeout(err) {
		t.Fatalf("idle connection not closed at once: %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("new connection accepted while draining")
	}
	r := <-done
	if r.err != nil || r.resp.StatusCode != 200 || !r.resp.Close {
		t.Fatalf("in-flight request: %v %+v", r.err, r.resp)
	}
	if err := <-shut; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := h.app.Reload(h.path); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reload after Shutdown: %v", err)
	}
	if len(h.logs.records("proxy stopped")) != 1 {
		t.Fatal("stop not logged")
	}
}

// With a shutdown timeout shorter than a response, Shutdown cuts it off at
// the timeout.
func TestShutdownTimeout(t *testing.T) {
	b1 := startBackend(t, "b1")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, "[timeouts]\nshutdown_ms = 300\n"))
	c := dialHTTP(t, h.l.Addrs[0])
	done := make(chan error, 1)
	go func() {
		_, err := c.try("GET /slow?ms=5000 HTTP/1.1\r\nHost: a.test\r\n\r\n")
		done <- err
	}()
	waitFor(t, "request in flight", func() bool { return h.app.Store().Load().Pool("p").Backends[0].InFlight() == 1 })
	start := time.Now()
	err := h.app.Shutdown()
	el := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}
	if el < 300*time.Millisecond || el > time.Second {
		t.Fatalf("Shutdown took %v, want about 300ms", el)
	}
	if err := <-done; err == nil {
		t.Fatal("request cut off at the deadline got a whole response")
	}
}

// The shutdown timeout in force is the one from the current config.
func TestShutdownTimeoutReloaded(t *testing.T) {
	b1 := startBackend(t, "b1")
	h := start(t, plainConfig("127.0.0.1:0", []string{b1}, ""))
	if err := h.reload(plainConfig("127.0.0.1:0", []string{b1}, "[timeouts]\nshutdown_ms = 0\n")); err != nil {
		t.Fatal(err)
	}
	c := dialHTTP(t, h.l.Addrs[0])
	go c.try("GET /slow?ms=5000 HTTP/1.1\r\nHost: a.test\r\n\r\n")
	waitFor(t, "request in flight", func() bool { return h.app.Store().Load().Pool("p").Backends[0].InFlight() == 1 })
	start := time.Now()
	if err := h.app.Shutdown(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("Shutdown with shutdown_ms = 0 took %v", el)
	}
}

// A failure to start leaves nothing running (goleak checks).
func TestStartFails(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg, err := config.Parse("t.toml", []byte(plainConfig(busy.Addr().String(), []string{"127.0.0.1:1"}, "")))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Start = %v, want a listen error", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

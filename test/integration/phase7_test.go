package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/app"
	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
)

// appProxy is the proxy run by internal/app, as cmd/proxy runs it, over
// HTTPS with a route for a.test whose certificate is a.crt in Dir, and a
// default pool. Reload rewrites the config file and reloads it.
type appProxy struct {
	*Proxy
	App *app.App
	t   testing.TB
}

// appConfig is the config appProxy serves: one HTTPS listener on addr, the
// route a.test to pool p of backends, and extra.
func appConfig(addr string, backends []string, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
default_pool = "p"

[[listener]]
address = %q

[[route]]
host = "a.test"
pool = "p"
cert = "a.crt"
key  = "a.key"

[tls]
default_cert = "default.crt"
default_key  = "default.key"

[pool.p]
backends = [`, addr)
	for i, a := range backends {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "{ address = %q }", a)
	}
	b.WriteString("]\n")
	b.WriteString(extra)
	return b.String()
}

// startApp writes the config for backends and runs the proxy on it until
// the test ends.
func startApp(t testing.TB, backends []string, extra string, logger *slog.Logger) *appProxy {
	t.Helper()
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	ca.WriteFiles(t, dir, "default", tlstest.LeafOptions{}, "*.test")
	path := filepath.Join(dir, "proxy.toml")
	writeConfig(t, path, appConfig("127.0.0.1:0", backends, extra))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	a, err := app.New(cfg, app.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Shutdown() })
	addr := l.Addrs[0]
	return &appProxy{
		Proxy: &Proxy{ConfigPath: path, Dir: dir, Scheme: config.ModeHTTPS, CA: ca, Addr: addr,
			URL: "https://a.test:" + port(addr), Metrics: a.Metrics(), Health: a.Health()},
		App: a,
		t:   t,
	}
}

// writeConfig replaces the file at path by renaming a new one into place,
// so a reload never reads half a file.
func writeConfig(t testing.TB, path, src string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// Reload writes the config for backends and reloads it. The listener's
// configured address stays 127.0.0.1:0, which is what keeps it: listeners
// are matched by the address the file gives, not the port bound.
func (p *appProxy) Reload(backends []string, extra string) {
	p.t.Helper()
	writeConfig(p.t, p.ConfigPath, appConfig("127.0.0.1:0", backends, extra))
	if err := p.App.Reload(p.ConfigPath); err != nil {
		p.t.Fatalf("reload: %v", err)
	}
}

// reloadHealth turns on active checks every 50 ms, so reloads start and stop
// probers that are actually probing.
const reloadHealth = "\n[health]\ninterval_ms = 50\ntimeout_ms = 40\n"

// Done when (in-process, short; bench/phase7/reload-wrk.sh is the 60-second
// wrk run): reloads alternating pool membership under load fail no request.
func TestReloadUnderLoad(t *testing.T) {
	bs := StartBackends(t, 3, BackendOptions{Quiet: true})
	sets := [][]string{{bs[0].Addr, bs[1].Addr}, {bs[1].Addr, bs[2].Addr}}
	p := startApp(t, sets[0], reloadHealth, nil)
	load := StartLoad(t, p.Proxy, 8, "/")
	reloads := 60
	if testing.Short() {
		reloads = 10
	}
	for i := range reloads {
		time.Sleep(50 * time.Millisecond)
		p.Reload(sets[(i+1)%2], reloadHealth)
	}
	time.Sleep(100 * time.Millisecond)
	load.Stop()
	if f := load.Failures(); len(f) > 0 {
		t.Fatalf("%d of %d requests failed across %d reloads; first: %+v", len(f), int64(len(f))+load.OK(), reloads, f[0])
	}
	if m := p.App.Metrics(); m.Reloads(true) != uint64(reloads) || m.Reloads(false) != 0 {
		t.Fatalf("reloads: %d applied, %d failed", m.Reloads(true), m.Reloads(false))
	}
	got := load.Backends()
	for _, b := range []string{"b1", "b2", "b3"} {
		if got[b] == 0 {
			t.Fatalf("no requests reached %s: %v", b, got)
		}
	}
	t.Logf("%d requests, %d reloads, by backend %v", load.OK(), reloads, got)
}

// serial returns the leaf serial number the server presented on c.
func serial(c *tls.Conn) string {
	return c.ConnectionState().PeerCertificates[0].SerialNumber.String()
}

// Done when: after SIGHUP, new handshakes present the new certificate and
// existing connections keep working.
func TestCertificateRotation(t *testing.T) {
	bs := StartBackends(t, 1, BackendOptions{Quiet: true})
	p := startApp(t, []string{bs[0].Addr}, "", nil)
	dial := func() (*tls.Conn, *bufio.Reader) {
		t.Helper()
		// No session cache, so every handshake is a full one and shows the
		// certificate.
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", p.Addr, p.CA.ClientConfig("a.test"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c, bufio.NewReader(c)
	}
	request := func(c *tls.Conn, br *bufio.Reader) {
		t.Helper()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Close {
			t.Fatalf("status %d close %v", resp.StatusCode, resp.Close)
		}
	}
	old, oldBR := dial()
	request(old, oldBR)
	before := serial(old)

	// Renew: a new leaf for a.test in the same files, then reload.
	certPEM, keyPEM := p.CA.Issue(t, tlstest.LeafOptions{}, "a.test")
	writeConfig(t, filepath.Join(p.Dir, "a.crt"), string(certPEM))
	writeConfig(t, filepath.Join(p.Dir, "a.key"), string(keyPEM))
	p.Reload([]string{bs[0].Addr}, "")

	fresh, freshBR := dial()
	request(fresh, freshBR)
	if after := serial(fresh); after == before {
		t.Fatalf("new handshake still presents serial %s", before)
	}
	// The connection made before the reload carries on, on its old
	// session.
	request(old, oldBR)
	if serial(old) != before {
		t.Fatal("existing connection's certificate changed")
	}
}

// heapAfterGC returns the live heap once garbage has been collected.
func heapAfterGC() uint64 {
	var ms runtime.MemStats
	for range 3 {
		runtime.GC()
	}
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// settledGoroutines waits for the goroutine count to stop changing (client
// connections closing, probers of removed backends returning) and returns
// it.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m == n {
			return n
		}
		n = m
	}
	return n
}

// Done when: after 1,000 reloads, heap size and goroutine count are flat.
// Each reload alternates pool membership and rereads the certificates;
// probers run and requests flow between reloads.
func TestReloadsFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000 reloads")
	}
	// The backends run in this process, so they must not keep a log of
	// every request and probe.
	bs := StartBackends(t, 3, BackendOptions{Quiet: true, NoRecord: true})
	sets := [][]string{{bs[0].Addr, bs[1].Addr}, {bs[1].Addr, bs[2].Addr}}
	p := startApp(t, sets[0], reloadHealth, nil)
	c := p.Client(t)
	tr := c.Transport.(*http.Transport)
	reload := func(i int) {
		p.Reload(sets[i%2], reloadHealth)
		if i%10 == 0 {
			if _, err := getOnce(c, p.URL+"/"); err != nil {
				t.Fatalf("reload %d: %v", i, err)
			}
		}
	}
	// Warm up: pools, metrics, buffers and the rate-limit route table
	// reach their working size.
	for i := range 100 {
		reload(i)
	}
	tr.CloseIdleConnections()
	heap0, g0 := heapAfterGC(), settledGoroutines()
	for i := range 1000 {
		reload(i)
	}
	tr.CloseIdleConnections()
	heap1, g1 := heapAfterGC(), settledGoroutines()
	t.Logf("heap %d -> %d bytes (%+d), goroutines %d -> %d", heap0, heap1, int64(heap1)-int64(heap0), g0, g1)
	// A leak of even a few hundred bytes per reload would show as
	// hundreds of KiB here.
	if heap1 > heap0+256<<10 {
		t.Errorf("heap grew from %d to %d bytes over 1,000 reloads", heap0, heap1)
	}
	if g1 > g0 {
		t.Errorf("goroutines grew from %d to %d over 1,000 reloads", g0, g1)
	}
	if n := len(p.App.Health().Targets()); n != 2 {
		t.Errorf("%d health targets, want 2", n)
	}
	if n := len(p.App.Upstream().Pools()); n != 2 {
		t.Errorf("%d upstream pools, want 2", n)
	}
	if m := p.App.Metrics(); m.Reloads(true) != 1100 {
		t.Errorf("%d reloads applied, want 1100", m.Reloads(true))
	}
}

// Done when: goleak finds no goroutines left after shutdown, with idle
// keep-alive connections, a request in flight, probers and reloads behind
// it.
func TestShutdownLeavesNoGoroutines(t *testing.T) {
	bs := StartBackends(t, 3, BackendOptions{Quiet: true})
	base := goleak.IgnoreCurrent()
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	ca.WriteFiles(t, dir, "default", tlstest.LeafOptions{}, "*.test")
	path := filepath.Join(dir, "proxy.toml")
	writeConfig(t, path, appConfig("127.0.0.1:0", []string{bs[0].Addr, bs[1].Addr}, reloadHealth+"[admin]\naddress = \"127.0.0.1:0\"\n"))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Addr: l.Addrs[0], CA: ca}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, p.Addr)
		},
		TLSClientConfig: ca.ClientConfig(""),
	}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for range 10 {
		if _, err := getOnce(c, "https://a.test/"); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		writeConfig(t, path, appConfig("127.0.0.1:0", []string{bs[i%3].Addr, bs[(i+1)%3].Addr}, reloadHealth+"[admin]\naddress = \"127.0.0.1:0\"\n"))
		if err := a.Reload(path); err != nil {
			t.Fatal(err)
		}
	}
	slow := make(chan error, 1)
	go func() {
		_, err := getOnce(c, "https://a.test/slow?ms=300")
		slow <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-slow; err != nil {
		t.Fatalf("request in flight at shutdown: %v", err)
	}
	tr.CloseIdleConnections()
	goleak.VerifyNone(t, base)
}

package integration

// Phase 5 "Done when" checks that need the proxy running. The state
// machine's flapping test and the prober leak check are unit tests in
// internal/health; the retry rules are unit-tested in internal/conn;
// Prometheus scraping is lab/prometheus-check.sh.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/admin"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
)

// fastHealth checks every 100 ms. With rise 2 and fall 3 an active check
// verdict takes 200 to 400 ms.
func fastHealth() *health.Options {
	return &health.Options{
		Path:            "/healthz",
		Interval:        100 * time.Millisecond,
		Timeout:         80 * time.Millisecond,
		Rise:            2,
		Fall:            3,
		PassiveFailures: 3,
		Eject:           time.Second,
	}
}

func procBackends(t *testing.T, n int) []*ProcBackend {
	t.Helper()
	out := make([]*ProcBackend, n)
	for i := range out {
		out[i] = StartProcBackend(t, fmt.Sprintf("b%d", i+1))
	}
	return out
}

func procAddrs(bs []*ProcBackend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Addr
	}
	return out
}

// waitState waits up to d for addr's health to satisfy ok and returns
// when it was first seen to.
func waitState(t *testing.T, p *Proxy, addr string, d time.Duration, what string, ok func(health.State) bool) time.Time {
	t.Helper()
	tg := p.Health.Target(addr)
	deadline := time.Now().Add(d)
	for {
		if ok(tg.State()) {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not %s after %v: %+v", addr, what, d, tg.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

func outOfRotation(s health.State) bool { return s != health.Healthy }
func inRotation(s health.State) bool    { return s == health.Healthy }

func poolStats(p *Proxy) *metrics.PoolStats { return p.Snapshot.Pool("default").Stats }

// TestKillBackendUnderLoad: kill -9 on one of three backends under load.
// Requests it was serving are retried on the others, new requests stop
// going to it once passive checks eject it, and no request fails after
// that. The failures during detection are recorded; the target is zero,
// because every failure there is a GET that can be retried.
func TestKillBackendUnderLoad(t *testing.T) {
	bs := procBackends(t, 3)
	p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(procAddrs(bs)...),
		ProxyOptions{Health: fastHealth(), Admin: true, Logger: discard()})
	load := StartLoad(t, p, 16, "/")
	time.Sleep(500 * time.Millisecond)
	if load.Backends()["b2"] == 0 {
		t.Fatalf("b2 served nothing before the kill: %v", load.Backends())
	}

	killed := time.Now()
	bs[1].Kill()
	ejected := waitState(t, p, bs[1].Addr, 2*time.Second, "out of rotation", outOfRotation)
	time.Sleep(time.Second)
	load.Stop()

	during, after := load.Split(ejected)
	st := BackendStatus(t, p.Status(t), bs[1].Addr)
	t.Logf("kill -9: out of rotation after %v (state %s), %d requests ok, failed: %d during detection, %d after ejection; retries: %d dial, %d error",
		ejected.Sub(killed).Round(time.Microsecond), st.State, load.OK(), during, after,
		poolStats(p).Retries(metrics.RetryDial), poolStats(p).Retries(metrics.RetryError))
	for _, f := range load.Failures() {
		t.Logf("failure at %v after the kill: %s", f.Start.Sub(killed), f.Err)
	}
	if after != 0 {
		t.Errorf("%d requests failed after ejection, want 0", after)
	}
	if during != 0 {
		t.Errorf("%d GETs failed during detection, want 0: each could have been retried", during)
	}
	if st.State == "healthy" {
		t.Errorf("killed backend healthy in /status: %+v", st)
	}
}

// TestStoppedBackendEjectedByTimeouts: SIGSTOP leaves a backend's sockets
// open and silent. Requests to it time out and are retried elsewhere, the
// timeouts eject it, it stays out while it is stopped, and it returns
// once resumed.
func TestStoppedBackendEjectedByTimeouts(t *testing.T) {
	bs := procBackends(t, 3)
	opts := ProxyOptions{Health: fastHealth(), Logger: discard()}
	opts.Timeouts.UpstreamResponse = 300 * time.Millisecond
	p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(procAddrs(bs)...), opts)
	load := StartLoad(t, p, 8, "/")
	time.Sleep(300 * time.Millisecond)

	stopped := time.Now()
	bs[1].Stop()
	out := waitState(t, p, bs[1].Addr, 3*time.Second, "out of rotation", outOfRotation)
	// Detection is bounded by the faster of passive checks (a run of
	// response timeouts) and active checks (fall checks, each up to the
	// check timeout, an interval apart).
	h := fastHealth()
	bound := min(opts.Timeouts.UpstreamResponse, time.Duration(h.Fall)*(h.Interval+h.Timeout)) + 500*time.Millisecond
	t.Logf("SIGSTOP: out of rotation after %v (bound %v), state %v", out.Sub(stopped), bound, p.Health.Target(bs[1].Addr).State())
	if out.Sub(stopped) > bound {
		t.Errorf("out of rotation after %v, want within %v", out.Sub(stopped), bound)
	}

	// Past the ejection backoff it stays out: its checks still time out.
	time.Sleep(h.Eject + 300*time.Millisecond)
	if st := p.Health.Target(bs[1].Addr).Status(); st.State == "healthy" {
		t.Fatalf("stopped backend back in rotation: %+v", st)
	}
	if st := p.Health.Target(bs[1].Addr).Status(); st.LastCheck == nil || !strings.Contains(st.LastCheck.Error, "deadline exceeded") {
		t.Errorf("last check %+v, want a timeout", st.LastCheck)
	}
	load.Stop()
	if fs := load.Failures(); len(fs) != 0 {
		t.Errorf("%d GETs failed, want 0 (each timed out request is retried); first: %s", len(fs), fs[0].Err)
	}
	if n := poolStats(p).Retries(metrics.RetryTimeout); n == 0 {
		t.Error("no request was retried after a timeout")
	}

	bs[1].Cont()
	back := waitState(t, p, bs[1].Addr, 3*time.Second, "back in rotation", inRotation)
	t.Logf("SIGCONT: back in rotation after %v", back.Sub(stopped))
}

// TestRestartedBackendReadmitted: a killed backend that comes back is put
// back in rotation after rise successful checks, not before, and /status
// shows each step.
func TestRestartedBackendReadmitted(t *testing.T) {
	b, other := StartProcBackend(t, "b1"), StartProcBackend(t, "b2")
	h := fastHealth()
	h.Rise, h.Fall, h.PassiveFailures = 3, 2, 0 // active checks alone
	p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(b.Addr, other.Addr),
		ProxyOptions{Health: h, Admin: true, Logger: discard()})

	b.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for BackendStatus(t, p.Status(t), b.Addr).State != "unhealthy" {
		if time.Now().After(deadline) {
			t.Fatal("killed backend never unhealthy in /status")
		}
		time.Sleep(10 * time.Millisecond)
	}

	b.Restart()
	restarted := time.Now()
	maxWhileDown := 0
	var up admin.BackendStatus
	for deadline := time.Now().Add(5 * time.Second); ; {
		st := BackendStatus(t, p.Status(t), b.Addr)
		if st.State == "healthy" {
			up = st
			break
		}
		maxWhileDown = max(maxWhileDown, st.ConsecutiveSuccesses)
		if time.Now().After(deadline) {
			t.Fatalf("restarted backend not healthy after 5s: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("restart: healthy after %v; while unhealthy /status showed up to %d consecutive successes", time.Since(restarted), maxWhileDown)
	if maxWhileDown >= h.Rise {
		t.Errorf("unhealthy with %d consecutive successes, rise is %d", maxWhileDown, h.Rise)
	}
	if maxWhileDown == 0 {
		t.Error("never saw a success before re-admission; the check is not exercised")
	}
	if up.ConsecutiveSuccesses < h.Rise || up.Transitions["healthy"] != 1 || up.Transitions["unhealthy"] != 1 {
		t.Errorf("healthy status %+v", up)
	}

	// Traffic reaches it again.
	c := p.Client(t)
	seen := map[string]bool{}
	for range 10 {
		resp, err := c.Get(p.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		seen[resp.Header.Get("X-Backend")] = true
	}
	if !seen["b1"] {
		t.Errorf("no traffic reached the re-admitted backend: %v", seen)
	}
}

// TestHealthzFailureEjects: a backend that accepts TCP and serves traffic
// but answers 500 on /healthz is taken out by active checks alone
// (passive checks are off), and gets no traffic until its checks pass.
func TestHealthzFailureEjects(t *testing.T) {
	b1 := StartBackend(t, "b1", BackendOptions{Quiet: true})
	b2 := StartBackend(t, "b2", BackendOptions{Quiet: true, HealthzFail: true})
	b3 := StartBackend(t, "b3", BackendOptions{Quiet: true})
	h := fastHealth()
	h.PassiveFailures = 0
	p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(b1.Addr, b2.Addr, b3.Addr),
		ProxyOptions{Health: h, Logger: discard()})
	waitState(t, p, b2.Addr, 2*time.Second, "out of rotation", outOfRotation)
	if st := p.Health.Target(b2.Addr).Status(); st.LastCheck == nil || st.LastCheck.Status != 500 {
		t.Fatalf("last check %+v, want a 500", st.LastCheck)
	}

	b2.ResetRequests()
	c := p.Client(t)
	for range 30 {
		resp, err := c.Get(p.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	for _, r := range b2.Requests() {
		if r.Path != "/healthz" {
			t.Fatalf("unhealthy backend got %s %s", r.Method, r.Path)
		}
	}

	b2.SetHealthy(true)
	waitState(t, p, b2.Addr, 2*time.Second, "back in rotation", inRotation)
}

// TestFailedResponseRetryRules: a backend that dies partway through its
// answer. Before the response headers are complete, nothing has reached
// the client: a GET is retried on another backend, a POST is not, because
// the backend may have acted on it. Once the headers are out, neither is.
func TestFailedResponseRetryRules(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		headersOut  bool
	}{
		{"before headers", "HTTP/1.1 200 OK\r\nContent-Le", false},
		{"mid-body", "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"GET", "POST"} {
				t.Run(method, func(t *testing.T) {
					good := StartBackend(t, "good", BackendOptions{Quiet: true})
					bad := StartRawBackend(t, func(c net.Conn) {
						br := bufio.NewReader(c)
						c.SetDeadline(time.Now().Add(5 * time.Second))
						req, err := http.ReadRequest(br)
						if err != nil {
							return
						}
						io.Copy(io.Discard, req.Body)
						io.WriteString(c, tc.reply)
					})
					// Round-robin tries the failing backend first.
					p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(bad.Addr, good.Addr),
						ProxyOptions{Logger: discard()})
					c := p.Client(t)
					var body io.Reader
					if method == "POST" {
						body = strings.NewReader("order=42")
					}
					req, _ := http.NewRequest(method, p.URL+"/echo", body)
					resp, err := c.Do(req)
					var status int
					var readErr error
					if err == nil {
						_, readErr = io.ReadAll(resp.Body)
						resp.Body.Close()
						status = resp.StatusCode
					}
					retried := len(good.Requests()) > 0
					wantRetry := method == "GET" && !tc.headersOut
					t.Logf("status %d, err %v, body err %v, retried %v", status, err, readErr, retried)
					if retried != wantRetry {
						t.Fatalf("retried on the other backend: %v, want %v", retried, wantRetry)
					}
					if bad.Accepted.Load() != 1 {
						t.Fatalf("failing backend accepted %d connections, want 1", bad.Accepted.Load())
					}
					switch {
					case wantRetry:
						if err != nil || status != 200 || readErr != nil {
							t.Fatalf("retried request failed: %d %v %v", status, err, readErr)
						}
					case tc.headersOut:
						// The client got the headers and a truncated body.
						if err != nil || status != 200 || readErr == nil {
							t.Fatalf("status %d err %v body err %v, want a truncated 200", status, err, readErr)
						}
					default:
						if err != nil || status != http.StatusBadGateway {
							t.Fatalf("status %d err %v, want 502", status, err)
						}
					}
				})
			}
		})
	}
}

// TestMetricsEndpoint: /metrics on the admin listener reports requests,
// health and connection reuse for live traffic.
func TestMetricsEndpoint(t *testing.T) {
	bs := StartBackends(t, 2, BackendOptions{Quiet: true})
	p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(Addrs(bs)...),
		ProxyOptions{Health: fastHealth(), Admin: true, Logger: discard()})
	c := p.Client(t)
	for range 10 {
		resp, err := c.Get(p.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	mc := &http.Client{Timeout: 5 * time.Second}
	defer mc.CloseIdleConnections()
	resp, err := mc.Get(p.AdminURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		fmt.Sprintf(`proxy_requests_total{backend=%q,code="200",pool="default"} 5`, bs[0].Addr),
		fmt.Sprintf(`proxy_backend_healthy{backend=%q} 1`, bs[1].Addr),
		`proxy_request_duration_seconds_count{pool="default"} 10`,
		fmt.Sprintf(`proxy_upstream_connections_total{backend=%q,kind="reuse"}`, bs[0].Addr),
		`proxy_retries_total{pool="default",reason="dial"} 0`,
		`proxy_client_connections `,
		`go_goroutines `,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}

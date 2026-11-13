package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// value returns the value of the metric name whose labels include every
// pair in labels, read from the text exposition the handler serves, and
// whether it was found. For a histogram, name_count is the name to ask for.
func value(t *testing.T, m *Metrics, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
next:
	for line := range strings.Lines(rec.Body.String()) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		series, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		metric, rest, _ := strings.Cut(series, "{")
		if metric != name {
			continue
		}
		got := map[string]string{}
		for pair := range strings.SplitSeq(strings.TrimSuffix(rest, "}"), ",") {
			if k, v, ok := strings.Cut(pair, "="); ok {
				got[k] = strings.Trim(v, `"`)
			}
		}
		for k, v := range labels {
			if got[k] != v {
				continue next
			}
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		return f, true
	}
	return 0, false
}

func want(t *testing.T, m *Metrics, name string, labels map[string]string, v float64) {
	t.Helper()
	got, ok := value(t, m, name, labels)
	if !ok {
		t.Fatalf("%s%v missing", name, labels)
	}
	if got != v {
		t.Fatalf("%s%v = %v, want %v", name, labels, got, v)
	}
}

func TestCodes(t *testing.T) {
	var c Codes
	c.Add(200)
	c.Add(200)
	c.Add(599)
	c.Add(100)
	c.Add(42)
	c.Add(700)
	if c.Get(200) != 2 || c.Get(599) != 1 || c.Get(100) != 1 || c.Get(0) != 2 || c.Get(999) != 2 {
		t.Fatalf("counts 200:%d 599:%d 100:%d other:%d", c.Get(200), c.Get(599), c.Get(100), c.Get(0))
	}
	seen := map[string]uint64{}
	c.each(func(code string, n uint64) { seen[code] = n })
	if len(seen) != 4 || seen["200"] != 2 || seen["other"] != 2 {
		t.Fatalf("each: %v", seen)
	}
}

func TestCodesAddAllocatesNothing(t *testing.T) {
	var c Codes
	if n := testing.AllocsPerRun(100, func() { c.Add(200) }); n != 0 {
		t.Fatalf("Add allocates %v", n)
	}
}

// TestCollect: every metric family the proxy promises is exposed with the
// values put into the stats and sources.
func TestCollect(t *testing.T) {
	reg := upstream.NewRegistry(upstream.Options{MaxIdle: 4, IdleTimeout: time.Minute})
	defer reg.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	up := reg.Pool(addr)
	c, err := up.Get(t.Context(), time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	up.Put(c)
	if c, err = up.Get(t.Context(), time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	up.Put(c)

	chk := health.NewChecker(health.Config{Options: health.Options{PassiveFailures: 1, Eject: time.Hour}})
	defer chk.Close()
	tg := chk.Target(addr)
	chk.Target("10.0.0.99:80")
	tg.ReportFailure() // ejected

	m := New(Sources{Upstream: reg, Health: chk, ClientConns: func() int { return 7 }})
	ps := m.Pool("web")
	if ps != m.Pool("web") {
		t.Fatal("Pool made new stats for the same name")
	}
	bs := m.Backend("web", addr)
	if bs != m.Backend("web", addr) || bs == m.Backend("api", addr) {
		t.Fatal("Backend not keyed by pool and address")
	}
	bs.Codes.Add(200)
	bs.Codes.Add(200)
	bs.Codes.Add(502)
	bs.InFlight.Add(3)
	ps.NoBackend.Add(503)
	ps.Retry(RetryDial)
	ps.Retry(RetryStale)
	ps.Retry(RetryStale)
	ps.Latency.Observe(0.003)
	m.Unrouted().Add(404)

	want(t, m, "proxy_requests_total", map[string]string{"pool": "web", "backend": addr, "code": "200"}, 2)
	want(t, m, "proxy_requests_total", map[string]string{"pool": "web", "backend": addr, "code": "502"}, 1)
	want(t, m, "proxy_requests_total", map[string]string{"pool": "web", "backend": "", "code": "503"}, 1)
	want(t, m, "proxy_requests_total", map[string]string{"pool": "", "backend": "", "code": "404"}, 1)
	if _, ok := value(t, m, "proxy_requests_total", map[string]string{"code": "201"}); ok {
		t.Fatal("a code never counted is exposed")
	}
	want(t, m, "proxy_requests_in_flight", map[string]string{"pool": "web", "backend": addr}, 3)
	want(t, m, "proxy_retries_total", map[string]string{"pool": "web", "reason": "stale_conn"}, 2)
	want(t, m, "proxy_retries_total", map[string]string{"pool": "web", "reason": "dial"}, 1)
	want(t, m, "proxy_retries_total", map[string]string{"pool": "web", "reason": "timeout"}, 0)
	want(t, m, "proxy_request_duration_seconds_count", map[string]string{"pool": "web"}, 1)
	want(t, m, "proxy_upstream_connections_total", map[string]string{"backend": addr, "kind": "dial"}, 1)
	want(t, m, "proxy_upstream_connections_total", map[string]string{"backend": addr, "kind": "reuse"}, 1)
	want(t, m, "proxy_upstream_idle_connections", map[string]string{"backend": addr}, 1)
	want(t, m, "proxy_backend_healthy", map[string]string{"backend": addr}, 0)
	want(t, m, "proxy_backend_healthy", map[string]string{"backend": "10.0.0.99:80"}, 1)
	want(t, m, "proxy_backend_state", map[string]string{"backend": addr, "state": "ejected"}, 1)
	want(t, m, "proxy_backend_state", map[string]string{"backend": addr, "state": "healthy"}, 0)
	want(t, m, "proxy_backend_transitions_total", map[string]string{"backend": addr, "to": "ejected"}, 1)
	want(t, m, "proxy_client_connections", nil, 7)
	if _, ok := value(t, m, "go_goroutines", nil); !ok {
		t.Fatal("go_goroutines missing")
	}

	// The handler serves the text format.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), `proxy_requests_total{backend="`+addr+`",code="200",pool="web"} 2`) {
		t.Fatalf("status %d body:\n%s", rec.Code, body)
	}
}

// TestCollectWithoutSources: a Metrics with no sources still serves its own
// stats, and a nil one hands out no unrouted counter.
func TestCollectWithoutSources(t *testing.T) {
	m := New(Sources{})
	m.Backend("p", "a:1").Codes.Add(200)
	want(t, m, "proxy_requests_total", map[string]string{"pool": "p", "backend": "a:1", "code": "200"}, 1)
	if _, ok := value(t, m, "proxy_backend_healthy", nil); ok {
		t.Fatal("health exposed without a checker")
	}
	var nilM *Metrics
	if nilM.Unrouted() != nil {
		t.Fatal("nil Metrics returned an unrouted counter")
	}
}

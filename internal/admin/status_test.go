package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

const statusConfig = `
default_pool = "web"

[[listener]]
address = "127.0.0.1:0"
mode    = "http"

[pool.web]
algorithm = "least_conn"
backends  = [{ address = "127.0.0.1:1" }, { address = "127.0.0.1:2" }]

[pool.api]
backends  = [{ address = "127.0.0.1:2" }]
`

// runtimeFor builds the statusConfig snapshot with a checker that ejects on
// one passive failure and runs no active checks.
func runtimeFor(t *testing.T) (StatusSource, *metrics.Metrics) {
	t.Helper()
	c, err := config.Parse("status.toml", []byte(statusConfig))
	if err != nil {
		t.Fatal(err)
	}
	reg := upstream.NewRegistry(c.Upstream.Options())
	t.Cleanup(reg.Close)
	chk := health.NewChecker(health.Config{Options: health.Options{PassiveFailures: 1, Eject: time.Hour}})
	t.Cleanup(chk.Close)
	m := metrics.New(metrics.Sources{Upstream: reg, Health: chk})
	snap, err := config.NewSnapshot(c, config.Runtime{Conns: reg, Health: chk, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	return StatusSource{Config: config.NewStore(snap), Health: chk, Conns: reg}, m
}

func TestStatus(t *testing.T) {
	src, m := runtimeFor(t)
	src.Health.Target("127.0.0.1:2").ReportFailure()
	src.Config.Load().Pool("web").Backends[0].Acquire()

	s := start(t, Options{Address: "127.0.0.1:0", Status: src, Metrics: m.Handler()})
	code, body := fetch(t, "http://"+s.Addr()+"/status")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(st.Pools) != 2 || st.Pools[0].Name != "api" || st.Pools[1].Name != "web" {
		t.Fatalf("pools %+v", st.Pools)
	}
	api, web := st.Pools[0], st.Pools[1]
	if api.Healthy != 0 || api.Algorithm != "round_robin" || api.Backends[0].State != "ejected" {
		t.Fatalf("api %+v", api)
	}
	if web.Healthy != 1 || web.Algorithm != "least_conn" || web.Backends[0].InFlight != 1 || web.Backends[1].State != "ejected" {
		t.Fatalf("web %+v", web)
	}
	if len(st.Backends) != 2 || st.Backends[0].Address != "127.0.0.1:1" || st.Backends[1].Address != "127.0.0.1:2" {
		t.Fatalf("backends %+v", st.Backends)
	}
	b2 := st.Backends[1]
	if b2.State != "ejected" || b2.Transitions["ejected"] != 1 {
		t.Fatalf("backend 2 %+v", b2)
	}
	for _, key := range []string{`"consecutive_successes"`, `"passive_failures"`, `"idle_conns"`, `"dials"`, `"transitions"`} {
		if !strings.Contains(body, key) {
			t.Errorf("no %s in\n%s", key, body)
		}
	}

	code, body = fetch(t, "http://"+s.Addr()+"/metrics")
	if code != 200 || !strings.Contains(body, `proxy_backend_healthy{backend="127.0.0.1:2"} 0`) {
		t.Fatalf("metrics %d:\n%s", code, body)
	}
	resp, err := http.Post("http://"+s.Addr()+"/status", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status: %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
}

// Without a checker every backend reports healthy.
func TestStatusWithoutHealth(t *testing.T) {
	src, _ := runtimeFor(t)
	src.Health = nil
	st := src.Snapshot()
	if len(st.Backends) != 2 || st.Backends[0].State != "healthy" {
		t.Fatalf("backends %+v", st.Backends)
	}
}

// Without handlers for them, /metrics and /status are not served.
func TestNoMetricsOrStatus(t *testing.T) {
	s := start(t, Options{Address: "127.0.0.1:0"})
	for _, p := range []string{"/metrics", "/status"} {
		if code, _ := fetch(t, "http://"+s.Addr()+p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

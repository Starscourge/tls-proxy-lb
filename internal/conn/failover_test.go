package conn

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// noHealth turns active and passive checks off: backends stay healthy
// whatever happens, so a test sees only retries.
var noHealth = health.Options{Rise: 1, Fall: 1, Eject: time.Hour}

// failover is a proxy whose round-robin default pool holds addrs, in
// order, with a health checker and metrics.
type failover struct {
	addr  string
	logs  *logBuf
	snap  *config.Snapshot
	chk   *health.Checker
	m     *metrics.Metrics
	stats *metrics.PoolStats
}

func startFailover(t *testing.T, addrs []string, ho health.Options, mut func(*config.Config)) *failover {
	t.Helper()
	var list []string
	for _, a := range addrs {
		list = append(list, fmt.Sprintf("{ address = %q }", a))
	}
	c := parseConfig(t, "default_pool = \"p\"\n[[route]]\nhost = \"known\"\npool = \"p\"\n[pool.p]\nbackends = ["+strings.Join(list, ", ")+"]\n")
	if mut != nil {
		mut(c)
	}
	reg := upstream.NewRegistry(c.Upstream.Options())
	t.Cleanup(reg.Close)
	chk := health.NewChecker(health.Config{Options: ho})
	t.Cleanup(chk.Close)
	m := metrics.New(metrics.Sources{Upstream: reg, Health: chk})
	snap, err := config.NewSnapshot(c, config.Runtime{Conns: reg, Health: chk, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	addr, _, logs := startProxyWith(t, snap, func(p *Proxy) { p.Metrics = m })
	return &failover{addr: addr, logs: logs, snap: snap, chk: chk, m: m, stats: snap.Pool("p").Stats}
}

// countingOK is a keep-alive backend that answers every request with 200
// "ok" and counts them.
type countingOK struct {
	*kaBackend
	requests atomic.Int64
}

func newCountingOK(t *testing.T) *countingOK {
	b := &countingOK{}
	b.kaBackend = newKABackend(t, func(_ int, c net.Conn, br *bufio.Reader) {
		serveAll(c, br, func(*http.Request) string {
			b.requests.Add(1)
			return okReply
		})
	})
	return b
}

// failingBackend reads each request whole and then does reply: writes
// what it says (possibly nothing) and closes the connection.
func failingBackend(t *testing.T, reply string, seen *atomic.Int64) string {
	return rawBackend(t, func(c net.Conn) {
		if _, _, err := readRequestFrom(c, bufio.NewReader(c)); err != nil {
			return
		}
		seen.Add(1)
		if reply != "" {
			io.WriteString(c, reply)
		}
	})
}

const postBody = "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello"
const postEmpty = "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\n\r\n"

// A failed dial is retried on another backend for any method, body
// included: nothing of the request has been read or sent.
func TestRetryAfterDialFailure(t *testing.T) {
	for _, tc := range []struct {
		name, raw, method string
	}{
		{"GET", get("a"), "GET"},
		{"POST with body", postBody, "POST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			good := newCountingOK(t)
			f := startFailover(t, []string{refusedAddr(t), good.addr}, noHealth, nil)
			resp, body := request(t, f.addr, tc.raw, tc.method)
			if resp.StatusCode != 200 || string(body) != "ok" || good.requests.Load() != 1 {
				t.Fatalf("status %d body %q, good backend saw %d", resp.StatusCode, body, good.requests.Load())
			}
			rec := f.logs.waitAccess(t, 1)[0]
			if rec["retries"] != 1.0 || rec["backend"] != good.addr || rec["status"] != 200.0 {
				t.Fatalf("access %v", rec)
			}
			if f.stats.Retries(metrics.RetryDial) != 1 {
				t.Fatalf("dial retries %d", f.stats.Retries(metrics.RetryDial))
			}
		})
	}
}

// TestRetryAfterUpstreamFailure: a backend that fails after the request
// went out, before any response reached the client, is retried elsewhere
// for an idempotent method without a body, and never for a POST, with or
// without a body: the backend may have acted on it.
func TestRetryAfterUpstreamFailure(t *testing.T) {
	for _, reply := range []struct{ name, text string }{
		{"close without response", ""},
		{"partial status line", "HTTP/1.1 200 OK\r\nContent-Le"},
		{"garbage", "SSH-2.0-OpenSSH\r\n\r\n"},
	} {
		t.Run(reply.name, func(t *testing.T) {
			for _, tc := range []struct {
				raw, method string
				retried     bool
			}{
				{get("a"), "GET", true},
				{"HEAD / HTTP/1.1\r\nHost: a\r\n\r\n", "HEAD", true},
				{postEmpty, "POST", false},
				{postBody, "POST", false},
				{"GET / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\n\r\nabc", "GET", false},
			} {
				var bad atomic.Int64
				good := newCountingOK(t)
				f := startFailover(t, []string{failingBackend(t, reply.text, &bad), good.addr}, noHealth, nil)
				resp, _ := request(t, f.addr, tc.raw, tc.method)
				want, wantGood := http.StatusBadGateway, int64(0)
				if tc.retried {
					want, wantGood = 200, 1
				}
				if resp.StatusCode != want || bad.Load() != 1 || good.requests.Load() != wantGood {
					t.Fatalf("%q: status %d (want %d), failing backend saw %d, good saw %d (want %d)",
						tc.raw, resp.StatusCode, want, bad.Load(), good.requests.Load(), wantGood)
				}
				if tc.retried && f.stats.Retries(metrics.RetryError) != 1 {
					t.Fatalf("%q: error retries %d", tc.raw, f.stats.Retries(metrics.RetryError))
				}
			}
		})
	}
}

// A backend that takes the request and never answers is retried elsewhere
// after the response timeout.
func TestRetryAfterTimeout(t *testing.T) {
	var bad atomic.Int64
	hang := rawBackend(t, func(c net.Conn) {
		if _, _, err := readRequestFrom(c, bufio.NewReader(c)); err != nil {
			return
		}
		bad.Add(1)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 1)) // until the proxy gives up and closes
	})
	good := newCountingOK(t)
	f := startFailover(t, []string{hang, good.addr}, noHealth, func(c *config.Config) {
		c.Timeouts.UpstreamResponse = 150 * time.Millisecond
	})
	resp, _ := request(t, f.addr, get("a"), "GET")
	if resp.StatusCode != 200 || bad.Load() != 1 || good.requests.Load() != 1 {
		t.Fatalf("status %d, hanging backend saw %d, good %d", resp.StatusCode, bad.Load(), good.requests.Load())
	}
	if f.stats.Retries(metrics.RetryTimeout) != 1 {
		t.Fatalf("timeout retries %d", f.stats.Retries(metrics.RetryTimeout))
	}
}

// Once a 1xx has been relayed, the client has started reading this
// backend's answer, so a failure after it is not retried.
func TestNoRetryAfterInterimResponse(t *testing.T) {
	var bad atomic.Int64
	good := newCountingOK(t)
	f := startFailover(t, []string{failingBackend(t, "HTTP/1.1 103 Early Hints\r\nLink: </a.css>\r\n\r\n", &bad), good.addr}, noHealth, nil)
	cl := dialClient(t, f.addr)
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 103 {
		t.Fatalf("first response %d, want the relayed 103", resp.StatusCode)
	}
	if resp, _ := cl.read("GET"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("final response %d, want 502", resp.StatusCode)
	}
	if good.requests.Load() != 0 {
		t.Fatal("retried after a 1xx reached the client")
	}
}

// A response is an answer, whatever its status: a 5xx is relayed, not
// retried.
func TestNoRetryOnServerError(t *testing.T) {
	var bad atomic.Int64
	good := newCountingOK(t)
	f := startFailover(t, []string{failingBackend(t, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n", &bad), good.addr}, noHealth, nil)
	if resp, _ := request(t, f.addr, get("a"), "GET"); resp.StatusCode != 503 || good.requests.Load() != 0 {
		t.Fatalf("status %d, good backend saw %d", resp.StatusCode, good.requests.Load())
	}
}

func TestRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		maxRetries, backends, wantRetries int
	}{
		{0, 2, 0},
		{1, 3, 1},
		{2, 4, 2},
		{2, 2, 1}, // no backend left to try
	} {
		t.Run(fmt.Sprintf("max %d of %d", tc.maxRetries, tc.backends), func(t *testing.T) {
			var addrs []string
			for range tc.backends {
				addrs = append(addrs, refusedAddr(t))
			}
			f := startFailover(t, addrs, noHealth, func(c *config.Config) { c.Upstream.MaxRetries = tc.maxRetries })
			if resp, _ := request(t, f.addr, get("a"), "GET"); resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status %d", resp.StatusCode)
			}
			rec := f.logs.waitAccess(t, 1)[0]
			got, _ := rec["retries"].(float64)
			if int(got) != tc.wantRetries || f.stats.Retries(metrics.RetryDial) != uint64(tc.wantRetries) {
				t.Fatalf("retries %v in the log, %d counted; want %d", rec["retries"], f.stats.Retries(metrics.RetryDial), tc.wantRetries)
			}
		})
	}
}

// With every backend out of rotation the proxy answers 503 at once and
// dials nothing.
func TestAllUnhealthyIs503WithoutDial(t *testing.T) {
	a, b := newCountingOK(t), newCountingOK(t)
	ho := health.Options{Rise: 1, Fall: 1, PassiveFailures: 1, Eject: time.Hour}
	f := startFailover(t, []string{a.addr, b.addr}, ho, nil)
	f.chk.Target(a.addr).ReportFailure()
	f.chk.Target(b.addr).ReportFailure()
	resp, _ := request(t, f.addr, get("a"), "GET")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if a.conns.Load() != 0 || b.conns.Load() != 0 {
		t.Fatalf("dialled %d and %d", a.conns.Load(), b.conns.Load())
	}
	rec := f.logs.waitAccess(t, 1)[0]
	if rec["error"] != "no healthy backend" || rec["backend"] != "" {
		t.Fatalf("access %v", rec)
	}
	if n := f.stats.NoBackend.Get(503); n != 1 {
		t.Fatalf("503s without a backend counted %d", n)
	}
}

// TestPassiveEjection: a run of failures in live traffic takes a backend
// out of rotation at once; a success breaks the run.
//
// The proxy relays a response to the client before it reports the outcome
// to health, and writes the access-log line after that. So a test that
// reads health state, or depends on the order of reports, waits for each
// request's access-log line first: receiving the response is not enough.
func TestPassiveEjection(t *testing.T) {
	ho := health.Options{Rise: 1, Fall: 1, PassiveFailures: 3, Eject: time.Hour}
	t.Run("5xx responses", func(t *testing.T) {
		var n atomic.Int64
		// 500, 500, 200, then only 500s.
		b := newKABackend(t, func(_ int, c net.Conn, br *bufio.Reader) {
			serveAll(c, br, func(*http.Request) string {
				if n.Add(1) == 3 {
					return okReply
				}
				return "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n"
			})
		})
		f := startFailover(t, []string{b.addr}, ho, nil)
		tg := f.chk.Target(b.addr)
		sent := 0
		// send makes one request and waits until its outcome has been
		// reported to health.
		send := func(want int, what string) {
			t.Helper()
			if resp, _ := request(t, f.addr, get("a"), "GET"); resp.StatusCode != want {
				t.Fatalf("%s: status %d, want %d", what, resp.StatusCode, want)
			}
			sent++
			f.logs.waitAccess(t, sent)
		}
		for i, want := range []int{500, 500, 200, 500, 500} {
			send(want, fmt.Sprintf("request %d", i))
		}
		if !tg.Healthy() {
			t.Fatal("ejected although a success broke the run")
		}
		send(500, "third 500 in a row")
		if tg.State() != health.Ejected {
			t.Fatalf("state %v after three 500s in a row", tg.State())
		}
		send(503, "after ejection")
	})
	t.Run("dial failures", func(t *testing.T) {
		// A dead backend is ejected by the failures its requests saw, which
		// were each retried; afterwards no request is sent to it at all.
		good := newCountingOK(t)
		dead := refusedAddr(t)
		f := startFailover(t, []string{dead, good.addr}, ho, nil)
		for range 10 {
			if resp, _ := request(t, f.addr, get("a"), "GET"); resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
		}
		if f.chk.Target(dead).State() != health.Ejected {
			t.Fatal("dead backend not ejected")
		}
		if n := f.stats.Retries(metrics.RetryDial); n != 3 {
			t.Fatalf("%d dial retries, want 3: one per failure until ejection", n)
		}
	})
	t.Run("client failures do not count", func(t *testing.T) {
		b := newCountingOK(t)
		f := startFailover(t, []string{b.addr}, ho, func(c *config.Config) { c.Timeouts.ClientIO = 50 * time.Millisecond })
		for i := range 4 {
			// A body that never arrives: 408, the client's fault.
			cl := dialClient(t, f.addr)
			cl.send("POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\nab")
			if resp, _ := cl.read("POST"); resp.StatusCode != http.StatusRequestTimeout {
				t.Fatalf("status %d", resp.StatusCode)
			}
			// Any report to health happens before the access-log line.
			f.logs.waitAccess(t, i+1)
		}
		if !f.chk.Target(b.addr).Healthy() {
			t.Fatal("client stalls ejected the backend")
		}
	})
}

// Requests are counted by pool, backend and status, or as unrouted.
func TestRequestMetrics(t *testing.T) {
	good := newCountingOK(t)
	f := startFailover(t, []string{good.addr}, noHealth, func(c *config.Config) { c.DefaultPool = "" })
	if resp, _ := request(t, f.addr, get("known"), "GET"); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp, _ := request(t, f.addr, get("unknown"), "GET"); resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	f.logs.waitAccess(t, 2)
	if n := f.m.Backend("p", good.addr).Codes.Get(200); n != 1 {
		t.Fatalf("200s counted %d", n)
	}
	if n := f.m.Unrouted().Get(404); n != 1 {
		t.Fatalf("unrouted 404s counted %d", n)
	}
}

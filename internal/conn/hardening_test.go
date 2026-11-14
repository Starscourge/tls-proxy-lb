package conn

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// Requests with ambiguous framing are refused before anything reaches the
// backend, the connection is closed, and each refusal is counted by
// reason.
func TestProxyRefusesAmbiguousFraming(t *testing.T) {
	baddr, b := startBackend(t, testbackend.Config{})
	m := metrics.New(metrics.Sources{})
	addr, _, logs := startProxyWith(t, newSnapshot(t, parseConfig(t, fmt.Sprintf("default_pool = \"p\"\n[pool.p]\nbackends = [{ address = %q }]\n[limits]\nmax_header_count = 5\nmax_request_line = 64\n", baddr))),
		func(p *Proxy) { p.Metrics = m })
	cases := []struct {
		name, raw, reason string
		want              int
	}{
		{"CL and TE", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "cl_and_te", 400},
		{"duplicate CL", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\nx", "duplicate_cl", 400},
		{"TE in HTTP/1.0", "POST / HTTP/1.0\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "te_in_http10", 400},
		{"space before colon", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding : chunked\r\n\r\n0\r\n\r\n", "space_before_colon", 400},
		{"bare LF", "GET / HTTP/1.1\nHost: a\n\n", "bare_lf", 400},
		{"folded", "GET / HTTP/1.1\r\nHost: a\r\nX-A: 1\r\n Transfer-Encoding: chunked\r\n\r\n", "folded_header", 400},
		{"invalid Host", "GET / HTTP/1.1\r\nHost: a b\r\n\r\n", "invalid_host", 400},
		{"too many headers", "GET / HTTP/1.1\r\nHost: a\r\nA: 1\r\nB: 1\r\nC: 1\r\nD: 1\r\nE: 1\r\n\r\n", "too_many_headers", 431},
		{"request line too long", "GET /" + strings.Repeat("a", 60) + " HTTP/1.1\r\nHost: a\r\n\r\n", "request_line_too_long", 414},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := dialClient(t, addr)
			cl.send(tc.raw)
			resp, _ := cl.read("GET")
			if resp.StatusCode != tc.want || !resp.Close {
				t.Fatalf("status %d close %v, want %d and close", resp.StatusCode, resp.Close, tc.want)
			}
			cl.expectClosed(2 * time.Second)
		})
	}
	if n := len(b.Requests()); n != 0 {
		t.Fatalf("backend saw %d refused requests", n)
	}
	recs := logs.waitAccess(t, len(cases))
	for i, tc := range cases {
		if got := recs[i]["error"]; got != tc.reason {
			t.Errorf("%s: access log error %v, want %s", tc.name, got, tc.reason)
		}
	}
	rec := httptestGather(t, m)
	for _, tc := range cases {
		if !strings.Contains(rec, `proxy_bad_requests_total{reason="`+tc.reason+`"} 1`) {
			t.Errorf("%s not counted once in\n%s", tc.reason, rec)
		}
	}
}

// httptestGather returns m's metrics in the text format.
func httptestGather(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	w := &strings.Builder{}
	rec := &recorder{h: http.Header{}, w: w}
	m.Handler().ServeHTTP(rec, &http.Request{Method: "GET", Header: http.Header{}})
	return w.String()
}

type recorder struct {
	h http.Header
	w io.Writer
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(p []byte) (int, error) { return r.w.Write(p) }
func (r *recorder) WriteHeader(int)             {}

// A Content-Length over max_body_bytes is refused with 413 before the
// backend sees anything; a chunked body is cut off at the cap with 413.
func TestProxyBodyTooLarge(t *testing.T) {
	baddr, b := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.Limits.MaxBodyBytes = 10 })

	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 11\r\n\r\n01234567890")
	if resp, _ := cl.read("POST"); resp.StatusCode != 413 || !resp.Close {
		t.Fatalf("CL over the cap: status %d close %v", resp.StatusCode, resp.Close)
	}
	if n := len(b.Requests()); n != 0 {
		t.Fatalf("backend saw %d requests for a refused body", n)
	}

	cl = dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\n0123456789")
	if resp, body := cl.read("POST"); resp.StatusCode != 200 || string(body) != "0123456789" {
		t.Fatalf("body at the cap: status %d body %q", resp.StatusCode, body)
	}

	// /slow answers after the body is read, so the cap is hit while the
	// backend has not answered yet.
	cl = dialClient(t, addr)
	cl.send("POST /slow?ms=500 HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n8\r\n01234567\r\n8\r\n01234567\r\n0\r\n\r\n")
	if resp, _ := cl.read("POST"); resp.StatusCode != 413 || !resp.Close {
		t.Fatalf("chunked over the cap: status %d close %v", resp.StatusCode, resp.Close)
	}
}

// slowBody sets a minimum body rate of 1,000 bytes/s after 300 ms.
func slowBody(c *config.Config) {
	c.Limits.MinBodyRate = 1000
	c.Timeouts.BodyGrace = 300 * time.Millisecond
}

// A body trickling below min_body_rate is cut off once the grace has
// passed, however often it sends a byte, long before the stall timeout.
func TestProxyBodyTooSlow(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, slowBody)
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 100000\r\n\r\n")
	start := time.Now()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := cl.c.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	// /echo answers 200 at once and streams the body back, so the cut-off
	// shows as the connection closing mid-response.
	cl.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, cl.br); isTimeout(err) {
		t.Fatal("connection still open")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("slow body ran %v; the stall timeout is 5 s", el)
	}
}

// Before the backend answers, a body below the minimum rate gets 408.
func TestProxyBodyTooSlowBeforeResponse(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, slowBody)
	cl := dialClient(t, addr)
	cl.send("POST /slow?ms=10 HTTP/1.1\r\nHost: a\r\nContent-Length: 100000\r\n\r\nxx")
	start := time.Now()
	resp, _ := cl.read("POST")
	if resp.StatusCode != 408 || !resp.Close {
		t.Fatalf("status %d close %v, want 408 and close", resp.StatusCode, resp.Close)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("408 after %v", el)
	}
}

// The max_requests_per_conn-th response says Connection: close.
func TestProxyMaxRequestsPerConn(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.Limits.MaxRequestsPerConn = 3 })
	cl := dialClient(t, addr)
	for i := 1; i <= 3; i++ {
		cl.send(get("a"))
		resp, _ := cl.read("GET")
		if resp.StatusCode != 200 || resp.Close != (i == 3) {
			t.Fatalf("request %d: status %d close %v", i, resp.StatusCode, resp.Close)
		}
	}
	cl.expectClosed(2 * time.Second)
}

// X-Forwarded-For from an untrusted client is replaced, even when the
// client names it in Connection to have it stripped; from a trusted proxy
// it is appended to, and Forwarded is kept.
func TestProxyForwardedFor(t *testing.T) {
	got := make(chan *http.Request, 1)
	baddr := rawBackend(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		for {
			req, _, err := readRequestFrom(c, br)
			if err != nil {
				return
			}
			got <- req
			io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
		}
	})
	for _, tc := range []struct {
		name, trusted string
		wantForwarded bool
	}{
		{"untrusted", "", false},
		{"trusted", "127.0.0.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, _ := startProxy(t, baddr, func(c *config.Config) {
				if tc.trusted != "" {
					tr, err := ratelimit.ParseTrusted([]string{tc.trusted})
					if err != nil {
						t.Fatal(err)
					}
					c.TrustedProxies = tr
				}
			})
			cl := dialClient(t, addr)
			cl.send("GET / HTTP/1.1\r\nHost: a\r\nConnection: X-Forwarded-For\r\n" +
				"X-Forwarded-For: 203.0.113.9\r\nForwarded: for=203.0.113.9\r\n\r\n")
			if resp, _ := cl.read("GET"); resp.StatusCode != 204 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			req := <-got
			// Connection named X-Forwarded-For, so the client's copy is
			// stripped as hop-by-hop before the proxy sets its own: the
			// trick cannot remove the proxy's entry.
			if v := req.Header["X-Forwarded-For"]; len(v) != 1 || v[0] != "127.0.0.1" {
				t.Errorf("X-Forwarded-For %q, want [127.0.0.1]", v)
			}
			if _, ok := req.Header["Forwarded"]; ok != tc.wantForwarded {
				t.Errorf("Forwarded kept %v, want %v", ok, tc.wantForwarded)
			}
		})
	}
	// Without the Connection trick, a trusted proxy's X-Forwarded-For is
	// appended to.
	tr, _ := ratelimit.ParseTrusted([]string{"127.0.0.1"})
	addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.TrustedProxies = tr })
	cl := dialClient(t, addr)
	cl.send("GET / HTTP/1.1\r\nHost: a\r\nX-Forwarded-For: 203.0.113.9\r\n\r\n")
	cl.read("GET")
	if v := (<-got).Header.Get("X-Forwarded-For"); v != "203.0.113.9, 127.0.0.1" {
		t.Errorf("trusted proxy: X-Forwarded-For %q", v)
	}
}

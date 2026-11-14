package conn

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// limitedSnapshot builds a snapshot of src with a rate-limit table.
func limitedSnapshot(t *testing.T, src string) *config.Snapshot {
	t.Helper()
	c := parseConfig(t, src)
	reg := upstream.NewRegistry(c.Upstream.Options())
	t.Cleanup(reg.Close)
	tab := ratelimit.New(ratelimit.Options{MaxEntries: c.RateLimit.MaxEntries})
	t.Cleanup(tab.Close)
	snap, err := config.NewSnapshot(c, config.Runtime{Conns: reg, RateLimit: tab})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// limitConfig: a.test and the default pool are held to 2 requests and then
// one per 100 seconds, which no test lasts; b.test is not limited.
func limitConfig(backend, extraTop string) string {
	return fmt.Sprintf(`%s
default_pool = "p"

[[route]]
host = "a.test"
pool = "p"

[[route]]
host = "b.test"
pool = "p"
rate_limit = { rate = 0 }

[pool.p]
backends = [{ address = %q }]

[rate_limit]
rate  = 0.01
burst = 2
`, extraTop, backend)
}

// dialClientFrom dials addr from the local IP from, such as 127.0.0.2.
func dialClientFrom(t *testing.T, addr, from string) *client {
	t.Helper()
	d := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return newClient(t, c)
}

func TestRateLimitBurstThen429KeepAlive(t *testing.T) {
	baddr, b := startBackend(t, testbackend.Config{})
	snap := limitedSnapshot(t, limitConfig(baddr, ""))
	addr, _, logs := startProxySnapshot(t, snap)

	cl := dialClient(t, addr)
	for i := range 2 {
		cl.send(get("a.test"))
		if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
			t.Fatalf("request %d of the burst: status %d", i+1, resp.StatusCode)
		}
	}
	// Over the limit: 429 with Retry-After, on a connection that stays open.
	for i := range 2 {
		cl.send(get("a.test"))
		resp, body := cl.read("GET")
		if resp.StatusCode != http.StatusTooManyRequests || resp.Close {
			t.Fatalf("over the limit %d: status %d close %v", i+1, resp.StatusCode, resp.Close)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "100" {
			t.Fatalf("Retry-After %q, want 100 (one request per 100s)", ra)
		}
		if string(body) != "429 Too Many Requests\n" {
			t.Fatalf("body %q", body)
		}
	}
	// HEAD gets the headers only, and the connection still works after.
	cl.send("HEAD / HTTP/1.1\r\nHost: a.test\r\n\r\n")
	if resp, body := cl.read("HEAD"); resp.StatusCode != 429 || len(body) != 0 {
		t.Fatalf("HEAD: status %d, %d body bytes", resp.StatusCode, len(body))
	}
	// A route with rate = 0 is not limited, even for the same client.
	for range 5 {
		cl.send(get("b.test"))
		if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
			t.Fatalf("unlimited route: status %d", resp.StatusCode)
		}
	}
	if n := len(b.Requests()); n != 7 {
		t.Fatalf("backend saw %d requests, want 2 + 5: refused ones must not reach it", n)
	}
	recs := logs.waitAccess(t, 10)
	r := recs[2]
	if r["status"] != float64(429) || r["pool"] != "p" || r["backend"] != "" || r["error"] != "rate limited: client 127.0.0.1" {
		t.Fatalf("access line %v", r)
	}
	if n := snap.Lookup("a.test").RateLimited.Load(); n != 3 {
		t.Fatalf("a.test rate-limited counter = %d, want 3", n)
	}
	if n := snap.Lookup("b.test").RateLimited.Load(); n != 0 {
		t.Fatalf("b.test rate-limited counter = %d", n)
	}
}

func TestRateLimitOtherClientUnaffected(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxySnapshot(t, limitedSnapshot(t, limitConfig(baddr, "")))
	one := dialClient(t, addr)
	for range 3 {
		one.send(get("a.test"))
		one.read("GET")
	}
	one.send(get("a.test"))
	if resp, _ := one.read("GET"); resp.StatusCode != 429 {
		t.Fatalf("first client: status %d, want 429", resp.StatusCode)
	}
	two := dialClientFrom(t, addr, "127.0.0.2")
	for i := range 2 {
		two.send(get("a.test"))
		if resp, _ := two.read("GET"); resp.StatusCode != 200 {
			t.Fatalf("second client request %d: status %d", i+1, resp.StatusCode)
		}
	}
}

func TestRateLimitDefaultPool(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	snap := limitedSnapshot(t, limitConfig(baddr, ""))
	addr, _, _ := startProxySnapshot(t, snap)
	cl := dialClient(t, addr)
	codes := make([]int, 0, 3)
	for range 3 {
		cl.send(get("unknown.test"))
		resp, _ := cl.read("GET")
		codes = append(codes, resp.StatusCode)
	}
	if fmt.Sprint(codes) != "[200 200 429]" {
		t.Fatalf("default pool statuses %v, want [200 200 429]", codes)
	}
	// The default pool's bucket is its own: a.test still has its burst.
	cl.send(get("a.test"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("a.test after the default pool's limit: %d", resp.StatusCode)
	}
	if n := snap.Lookup("unknown.test").RateLimited.Load(); n != 1 {
		t.Fatalf("default pool counter %d", n)
	}
}

func TestRateLimitClosesWhenBodyOrClose(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxySnapshot(t, limitedSnapshot(t, limitConfig(baddr, "")))
	use := dialClient(t, addr)
	for range 2 {
		use.send(get("a.test"))
		use.read("GET")
	}
	for _, tc := range []struct {
		name, req string
	}{
		{"body", "POST /echo HTTP/1.1\r\nHost: a.test\r\nContent-Length: 5\r\n\r\nhello"},
		{"Connection: close", "GET / HTTP/1.1\r\nHost: a.test\r\nConnection: close\r\n\r\n"},
		{"HTTP/1.0", "GET / HTTP/1.0\r\nHost: a.test\r\n\r\n"},
	} {
		cl := dialClient(t, addr)
		cl.send(tc.req)
		resp, _ := cl.read("GET")
		if resp.StatusCode != 429 || !resp.Close || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: status %d close %v Retry-After %q", tc.name, resp.StatusCode, resp.Close, resp.Header.Get("Retry-After"))
		}
		cl.expectClosed(2 * time.Second)
	}
}

func TestRateLimitIgnoresUntrustedForwardedFor(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxySnapshot(t, limitedSnapshot(t, limitConfig(baddr, "")))
	cl := dialClient(t, addr)
	codes := make([]int, 0, 4)
	for i := range 4 {
		// A client rotating X-Forwarded-For gains nothing.
		cl.send(fmt.Sprintf("GET / HTTP/1.1\r\nHost: a.test\r\nX-Forwarded-For: 203.0.113.%d\r\n\r\n", i))
		resp, _ := cl.read("GET")
		codes = append(codes, resp.StatusCode)
	}
	if fmt.Sprint(codes) != "[200 200 429 429]" {
		t.Fatalf("statuses %v, want [200 200 429 429]", codes)
	}
}

func TestRateLimitTrustedForwardedFor(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, logs := startProxySnapshot(t, limitedSnapshot(t, limitConfig(baddr, `trusted_proxies = ["127.0.0.1"]`)))
	cl := dialClient(t, addr)
	req := func(xff string) int {
		cl.send("GET / HTTP/1.1\r\nHost: a.test\r\nX-Forwarded-For: " + xff + "\r\n\r\n")
		resp, _ := cl.read("GET")
		return resp.StatusCode
	}
	// From a trusted proxy, each forwarded client has its own bucket; the
	// client-written part left of the real client is not used.
	var got []int
	for _, xff := range []string{
		"203.0.113.1", "1.1.1.1, 203.0.113.1", "203.0.113.1",
		"203.0.113.2", "203.0.113.2",
	} {
		got = append(got, req(xff))
	}
	if fmt.Sprint(got) != "[200 200 429 200 200]" {
		t.Fatalf("statuses %v, want [200 200 429 200 200]", got)
	}
	recs := logs.waitAccess(t, 5)
	if !strings.Contains(fmt.Sprint(recs[2]["error"]), "client 203.0.113.1") {
		t.Fatalf("access line %v", recs[2])
	}
}

func TestPerIPConnectionCap(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	snap := newSnapshot(t, parseConfig(t, fmt.Sprintf("default_pool = \"p\"\n[[listener]]\naddress = \"127.0.0.1:0\"\nmode = \"http\"\n[pool.p]\nbackends = [{ address = %q }]\n", baddr)))
	p := &Proxy{Config: config.NewStore(snap)}
	s := NewServer(Limits{MaxPerIP: 2}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, p) }()
	t.Cleanup(func() { s.Close(); <-errc })
	addr := ln.Addr().String()

	a := dialClient(t, addr)
	b := dialClient(t, addr)
	waitFor(t, "two connections tracked", func() bool { return s.Tracker().Len() == 2 })
	over := dialClient(t, addr)
	over.expectClosed(2 * time.Second)
	if n := s.Tracker().RejectedFor(RejectPerIP); n != 1 {
		t.Fatalf("per-IP rejections %d, want 1", n)
	}
	// Another address is admitted, and the capped ones still work.
	other := dialClientFrom(t, addr, "127.0.0.2")
	for _, cl := range []*client{a, b, other} {
		cl.send(get("x"))
		if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// Closing one frees a slot.
	a.c.Close()
	waitFor(t, "slot freed", func() bool { return s.Tracker().ConnsFrom(a.c.LocalAddr().(*net.TCPAddr).AddrPort().Addr()) == 1 })
	again := dialClient(t, addr)
	again.send(get("x"))
	if resp, _ := again.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("after a slot freed: status %d", resp.StatusCode)
	}
}

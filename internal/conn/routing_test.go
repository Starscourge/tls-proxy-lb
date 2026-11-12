package conn

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// twoPools is a config with a.test on pool pa (backend one) and b.test on
// pool pb (backend two), plus extra.
func twoPools(one, two, extra string) string {
	return fmt.Sprintf(`%s
[[route]]
host = "a.test"
pool = "pa"

[[route]]
host = "b.test"
pool = "pb"

[pool.pa]
backends = [{ address = %q }]

[pool.pb]
backends = [{ address = %q }]
`, extra, one, two)
}

func TestRouteByHost(t *testing.T) {
	one, _ := startBackend(t, testbackend.Config{Name: "one"})
	two, _ := startBackend(t, testbackend.Config{Name: "two"})
	addr, _, logs := startProxySnapshot(t, newSnapshot(t, parseConfig(t, twoPools(one, two, ""))))

	// Hosts are matched without port and case; one keep-alive connection
	// may carry requests for different hosts.
	cl := dialClient(t, addr)
	for _, tc := range []struct{ host, want string }{
		{"a.test", "one"},
		{"B.Test:8080", "two"},
		{"a.test.", "one"},
	} {
		cl.send(get(tc.host))
		resp, _ := cl.read("GET")
		if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != tc.want {
			t.Fatalf("Host %s: status %d X-Backend %q, want %s", tc.host, resp.StatusCode, resp.Header.Get("X-Backend"), tc.want)
		}
	}
	recs := logs.waitAccess(t, 3)
	if recs[1]["pool"] != "pb" || recs[1]["backend"] != two || recs[1]["host"] != "B.Test:8080" {
		t.Fatalf("access line %v", recs[1])
	}
}

func TestUnknownHostGets404(t *testing.T) {
	one, b1 := startBackend(t, testbackend.Config{Name: "one"})
	two, b2 := startBackend(t, testbackend.Config{Name: "two"})
	addr, _, logs := startProxySnapshot(t, newSnapshot(t, parseConfig(t, twoPools(one, two, ""))))
	for _, req := range []string{
		get("c.test"),
		// HTTP/1.0 may omit Host; with no default pool it has no route.
		"GET / HTTP/1.0\r\n\r\n",
	} {
		cl := dialClient(t, addr)
		cl.send(req)
		resp, _ := cl.read("GET")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q: status %d, want 404", req, resp.StatusCode)
		}
		cl.expectClosed(2 * time.Second)
	}
	r := logs.waitAccess(t, 2)[0]
	if r["status"] != float64(404) || r["pool"] != "" || r["backend"] != "" || r["host"] != "c.test" {
		t.Fatalf("access line %v", r)
	}
	if n := len(b1.Requests()) + len(b2.Requests()); n != 0 {
		t.Fatalf("backends saw %d requests for an unknown host", n)
	}
}

func TestUnknownHostGoesToDefaultPool(t *testing.T) {
	one, _ := startBackend(t, testbackend.Config{Name: "one"})
	two, _ := startBackend(t, testbackend.Config{Name: "two"})
	addr, _, _ := startProxySnapshot(t, newSnapshot(t, parseConfig(t, twoPools(one, two, `default_pool = "pb"`))))
	cl := dialClient(t, addr)
	cl.send(get("c.test"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "two" {
		t.Fatalf("status %d X-Backend %q, want two", resp.StatusCode, resp.Header.Get("X-Backend"))
	}
}

// TestInFlightCountsTheExchange: a backend's in-flight count is 1 while its
// response is outstanding and returns to 0 after success and after errors.
func TestInFlightCountsTheExchange(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	refused := refusedAddr(t)
	src := fmt.Sprintf(`
default_pool = "p"

[[route]]
host = "down.test"
pool = "down"

[pool.p]
backends = [{ address = %q }]

[pool.down]
backends = [{ address = %q }]
`, baddr, refused)
	snap := newSnapshot(t, parseConfig(t, src))
	addr, _, _ := startProxySnapshot(t, snap)
	b := snap.Pool("p").Backends[0]

	cl := dialClient(t, addr)
	cl.send("GET /slow?ms=300 HTTP/1.1\r\nHost: a\r\n\r\n")
	waitFor(t, "in-flight count 1", func() bool { return b.InFlight() == 1 })
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	waitFor(t, "in-flight count 0", func() bool { return b.InFlight() == 0 })

	down := snap.Pool("down").Backends[0]
	cl = dialClient(t, addr)
	cl.send(get("down.test"))
	if resp, _ := cl.read("GET"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	waitFor(t, "in-flight count 0 after 502", func() bool { return down.InFlight() == 0 })

	// A client that leaves mid-response.
	cl = dialClient(t, addr)
	cl.send("GET /slow?ms=300 HTTP/1.1\r\nHost: a\r\n\r\n")
	waitFor(t, "in-flight count 1", func() bool { return b.InFlight() == 1 })
	cl.c.Close()
	waitFor(t, "in-flight count 0 after client left", func() bool { return b.InFlight() == 0 })
}

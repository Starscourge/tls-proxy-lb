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
)

// kaBackend is a raw backend that counts its connections and hands each
// one, with its index from 0, to handle.
type kaBackend struct {
	addr  string
	conns atomic.Int32
}

func newKABackend(t *testing.T, handle func(i int, c net.Conn, br *bufio.Reader)) *kaBackend {
	t.Helper()
	b := &kaBackend{}
	b.addr = rawBackend(t, func(c net.Conn) {
		i := int(b.conns.Add(1)) - 1
		handle(i, c, bufio.NewReader(c))
	})
	return b
}

// serveAll answers every request on a connection with respond's reply,
// reading each request body first, until the client closes or respond
// returns "".
func serveAll(c net.Conn, br *bufio.Reader, respond func(req *http.Request) string) {
	for {
		req, _, err := readRequestFrom(c, br)
		if err != nil {
			return
		}
		reply := respond(req)
		if reply == "" {
			return
		}
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
	}
}

const okReply = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"

func okBackend(t *testing.T) *kaBackend {
	return newKABackend(t, func(_ int, c net.Conn, br *bufio.Reader) {
		serveAll(c, br, func(*http.Request) string { return okReply })
	})
}

// request sends raw on a new client connection and returns the response.
func request(t *testing.T, addr, raw, method string) (*http.Response, []byte) {
	t.Helper()
	cl := dialClient(t, addr)
	cl.send(raw)
	return cl.read(method)
}

func expectOK(t *testing.T, addr string) {
	t.Helper()
	if resp, body := request(t, addr, get("a"), "GET"); resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

// Requests from different client connections share one upstream
// connection, and the access log says so.
func TestReuseAcrossClients(t *testing.T) {
	b := okBackend(t)
	snap := newSnapshot(t, parseConfig(t, fmt.Sprintf("default_pool = \"p\"\n[pool.p]\nbackends = [{ address = %q }]\n", b.addr)))
	addr, _, logs := startProxySnapshot(t, snap)
	for range 3 {
		expectOK(t, addr)
	}
	if n := b.conns.Load(); n != 1 {
		t.Fatalf("backend saw %d connections, want 1", n)
	}
	waitFor(t, "the connection parked", func() bool { return idleConns(snap) == 1 })
	recs := logs.waitAccess(t, 3)
	for i, r := range recs {
		if r["reused"] != (i > 0) {
			t.Errorf("request %d: reused %v", i, r["reused"])
		}
	}
}

// Requests on one client connection use the upstream connection one after
// another, chunked and empty responses included.
func TestReuseFraming(t *testing.T) {
	replies := []string{
		okReply,
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n",
		"HTTP/1.1 204 No Content\r\n\r\n",
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 99\r\n\r\n",
		okReply,
	}
	var n atomic.Int32
	b := newKABackend(t, func(_ int, c net.Conn, br *bufio.Reader) {
		serveAll(c, br, func(*http.Request) string { return replies[int(n.Add(1)-1)%len(replies)] })
	})
	addr, _, _ := startProxy(t, b.addr, nil)
	cl := dialClient(t, addr)
	for i := range replies {
		cl.send(get("a"))
		resp, _ := cl.read("GET")
		if resp.StatusCode/100 > 3 {
			t.Fatalf("reply %d: status %d", i, resp.StatusCode)
		}
	}
	// A HEAD response has no body whatever its Content-Length.
	cl.send("HEAD / HTTP/1.1\r\nHost: a\r\n\r\n")
	cl.read("HEAD")
	if c := b.conns.Load(); c != 1 {
		t.Fatalf("backend saw %d connections, want 1", c)
	}
}

// A POST whose body the backend read whole leaves the connection reusable.
func TestReuseAfterRequestBody(t *testing.T) {
	b := okBackend(t)
	addr, _, _ := startProxy(t, b.addr, nil)
	for _, raw := range []string{
		"POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello",
		"POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
	} {
		if resp, _ := request(t, addr, raw, "POST"); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	expectOK(t, addr)
	if c := b.conns.Load(); c != 1 {
		t.Fatalf("backend saw %d connections, want 1", c)
	}
}

// With max_idle_per_backend = 0 every request asks the backend to close
// and gets a connection of its own.
func TestReuseOff(t *testing.T) {
	var closes atomic.Int32
	b := newKABackend(t, func(_ int, c net.Conn, br *bufio.Reader) {
		serveAll(c, br, func(req *http.Request) string {
			if req.Close {
				closes.Add(1)
			}
			return okReply
		})
	})
	addr, _, logs := startProxy(t, b.addr, func(c *config.Config) { c.Upstream.MaxIdlePerBackend = 0 })
	for range 3 {
		expectOK(t, addr)
	}
	waitFor(t, "three connections", func() bool { return b.conns.Load() == 3 })
	if closes.Load() != 3 {
		t.Fatalf("%d of 3 requests asked to close", closes.Load())
	}
	for _, r := range logs.waitAccess(t, 3) {
		if r["reused"] != false {
			t.Fatalf("reused with reuse off: %v", r)
		}
	}
}

// Backends with the same address in two pools share their connections.
func TestReuseAcrossPools(t *testing.T) {
	b := okBackend(t)
	c := parseConfig(t, fmt.Sprintf(`
[[route]]
host = "a.test"
pool = "p"
[[route]]
host = "b.test"
pool = "q"
[pool.p]
backends = [{ address = %q }]
[pool.q]
algorithm = "least_conn"
backends = [{ address = %q }]
`, b.addr, b.addr))
	addr, _, _ := startProxySnapshot(t, newSnapshot(t, c))
	for _, host := range []string{"a.test", "b.test", "a.test"} {
		if resp, _ := request(t, addr, get(host), "GET"); resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", host, resp.StatusCode)
		}
	}
	if n := b.conns.Load(); n != 1 {
		t.Fatalf("backend saw %d connections, want 1", n)
	}
}

// An idle connection the backend has closed is found by the liveness check
// and skipped: the next request dials, with no failed attempt first.
func TestReuseSkipsConnectionClosedWhileIdle(t *testing.T) {
	b := newKABackend(t, func(i int, c net.Conn, br *bufio.Reader) {
		if i == 0 {
			// As a backend's own idle timeout would, right after replying.
			readRequestFrom(c, br)
			io.WriteString(c, okReply)
			return
		}
		serveAll(c, br, func(*http.Request) string { return okReply })
	})
	addr, _, logs := startProxy(t, b.addr, nil)
	cl := dialClient(t, addr)
	cl.send(get("a"))
	cl.read("GET")
	time.Sleep(50 * time.Millisecond) // let the FIN arrive
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	recs := logs.waitAccess(t, 2)
	if recs[1]["reused"] != false || recs[1]["retried"] != nil {
		t.Fatalf("second request: %v", recs[1])
	}
}

// After each of these exchanges the upstream connection must be closed,
// not reused: the next request opens a second connection.
func TestNoReuse(t *testing.T) {
	cases := []struct {
		name string
		// first is the backend's handling of the first request on the
		// first connection; it may leave the connection open.
		first func(c net.Conn, br *bufio.Reader)
		// client sends the first request and reads what it wants of it.
		client func(t *testing.T, addr string)
	}{
		{"Connection: close", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok")
			time.Sleep(time.Second) // and keep it open, ignoring its own word
		}, getOK},
		{"close-delimited body", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\nok")
		}, getOK},
		{"HTTP/1.0 response", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok")
			time.Sleep(time.Second)
		}, getOK},
		{"client leaves mid-response", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 10000000\r\n\r\n")
			chunk := strings.Repeat("x", 64<<10)
			for {
				c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.WriteString(c, chunk); err != nil {
					return
				}
			}
		}, func(t *testing.T, addr string) {
			cl := dialClient(t, addr)
			cl.send(get("a"))
			cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := http.ReadResponse(cl.br, nil); err != nil {
				t.Fatal(err)
			}
			cl.c.Close()
		}},
		{"answer before the request body", func(c net.Conn, br *bufio.Reader) {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			http.ReadRequest(br) // headers only
			io.WriteString(c, "HTTP/1.1 413 Content Too Large\r\nContent-Length: 0\r\n\r\n")
			time.Sleep(time.Second)
		}, func(t *testing.T, addr string) {
			cl := dialClient(t, addr)
			cl.send("POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 10000000\r\n\r\n" + strings.Repeat("x", 1000))
			if resp, _ := cl.read("POST"); resp.StatusCode != 413 {
				t.Fatalf("status %d, want 413", resp.StatusCode)
			}
		}},
		{"client body cut short", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
		}, func(t *testing.T, addr string) {
			cl := dialClient(t, addr)
			cl.send("POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 10\r\n\r\nhello")
			time.Sleep(50 * time.Millisecond)
			cl.c.Close()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newKABackend(t, func(i int, c net.Conn, br *bufio.Reader) {
				if i == 0 {
					tc.first(c, br)
					return
				}
				serveAll(c, br, func(*http.Request) string { return okReply })
			})
			snap := newSnapshot(t, parseConfig(t, fmt.Sprintf("default_pool = \"p\"\n[pool.p]\nbackends = [{ address = %q }]\n", b.addr)))
			addr, _, logs := startProxySnapshot(t, snap)
			tc.client(t, addr)
			logs.waitAccess(t, 1)
			// The liveness check and the retry would hide a connection
			// parked by mistake, so look at the pool itself.
			time.Sleep(20 * time.Millisecond)
			if n := idleConns(snap); n != 0 {
				t.Fatalf("%d connections parked after the exchange", n)
			}
			expectOK(t, addr)
			if n := b.conns.Load(); n != 2 {
				t.Fatalf("backend saw %d connections, want 2", n)
			}
			if r := logs.waitAccess(t, 2)[1]; r["reused"] != false || r["retried"] != nil {
				t.Fatalf("second request reused: %v", r)
			}
		})
	}
}

func getOK(t *testing.T, addr string) { expectOK(t, addr) }

// idleConns returns how many connections the default pool's first backend
// has parked.
func idleConns(snap *config.Snapshot) int {
	return snap.Route("any").Backends[0].Conns.Idle()
}

// The backend reads the second request on a kept-alive connection and
// closes without a reply: its idle timeout fired just as the request
// arrived. The liveness check on Get cannot see that coming; the retry
// must cover it.
func TestRetryOnStaleConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(c net.Conn)
	}{
		{"FIN", func(c net.Conn) { c.Close() }},
		{"reset", func(c net.Conn) { c.(*net.TCPConn).SetLinger(0); c.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newKABackend(t, func(i int, c net.Conn, br *bufio.Reader) {
				if i > 0 {
					serveAll(c, br, func(*http.Request) string { return okReply })
					return
				}
				n := 0
				serveAll(c, br, func(*http.Request) string {
					n++
					if n == 2 {
						tc.close(c)
						return ""
					}
					return okReply
				})
			})
			addr, _, logs := startProxy(t, b.addr, nil)
			expectOK(t, addr)
			expectOK(t, addr)
			recs := logs.waitAccess(t, 2)
			if r := recs[1]; r["retried"] != true || r["reused"] != false || r["status"] != float64(200) {
				t.Fatalf("second request: %v", r)
			}
			if n := b.conns.Load(); n != 2 {
				t.Fatalf("backend saw %d connections, want 2", n)
			}
		})
	}
}

// No retry when the failure might not be a stale connection, or when
// sending the request again might repeat its effect.
func TestNoRetry(t *testing.T) {
	cases := []struct {
		name string
		// second handles the second request on the first connection.
		second func(c net.Conn)
		raw    string
		method string
		want   int
	}{
		{"POST already sent", func(c net.Conn) { c.Close() },
			"POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\n\r\n", "POST", 502},
		{"request with a body", func(c net.Conn) { c.Close() },
			"PUT / HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\nhi", "PUT", 502},
		{"response bytes arrived", func(c net.Conn) { io.WriteString(c, "HTTP/1.1 2"); c.Close() },
			get("a"), "GET", 502},
		{"timeout", func(c net.Conn) { time.Sleep(time.Second) },
			get("a"), "GET", 504},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newKABackend(t, func(i int, c net.Conn, br *bufio.Reader) {
				if i > 0 {
					serveAll(c, br, func(*http.Request) string { return okReply })
					return
				}
				n := 0
				serveAll(c, br, func(*http.Request) string {
					n++
					if n == 2 {
						tc.second(c)
						return ""
					}
					return okReply
				})
			})
			addr, _, logs := startProxy(t, b.addr, func(c *config.Config) { c.Timeouts.UpstreamResponse = 300 * time.Millisecond })
			expectOK(t, addr)
			if resp, _ := request(t, addr, tc.raw, tc.method); resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if r := logs.waitAccess(t, 2)[1]; r["retried"] != nil || r["reused"] != true {
				t.Fatalf("second request: %v", r)
			}
			if n := b.conns.Load(); n != 1 {
				t.Fatalf("backend saw %d connections, want 1", n)
			}
		})
	}
}

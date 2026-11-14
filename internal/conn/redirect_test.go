package conn

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
)

// startRedirect runs a redirect handler to port over a config parsed from
// src (routes and pools; parseConfig adds a listener).
func startRedirect(t *testing.T, src string, port int) (string, *logBuf) {
	t.Helper()
	logs := &logBuf{}
	r := NewRedirect(config.NewStore(newSnapshot(t, parseConfig(t, src))), slog.New(slog.NewJSONHandler(logs, nil)), port)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Limits{}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, r) }()
	t.Cleanup(func() { s.Close(); <-errc })
	return ln.Addr().String(), logs
}

const redirectRoutes = `
[[route]]
host = "a.test"
pool = "p"

[[route]]
host = "[::1]"
pool = "p"

[pool.p]
backends = [{ address = "127.0.0.1:1" }]
`

func TestRedirect(t *testing.T) {
	addr, logs := startRedirect(t, redirectRoutes, 8443)
	for _, tc := range []struct {
		req, method string
		status      int
		location    string
	}{
		{"GET /x/y?q=1 HTTP/1.1\r\nHost: a.test\r\n\r\n", "GET", 308, "https://a.test:8443/x/y?q=1"},
		{"GET / HTTP/1.1\r\nHost: A.TEST:8080\r\n\r\n", "GET", 308, "https://a.test:8443/"},
		{"HEAD /h HTTP/1.1\r\nHost: a.test\r\n\r\n", "HEAD", 308, "https://a.test:8443/h"},
		{"POST /p HTTP/1.1\r\nHost: a.test\r\nContent-Length: 5\r\n\r\nhello", "POST", 308, "https://a.test:8443/p"},
		{"GET http://a.test/abs HTTP/1.1\r\nHost: a.test\r\n\r\n", "GET", 308, "https://a.test:8443/abs"},
		{"GET / HTTP/1.1\r\nHost: [::1]:80\r\n\r\n", "GET", 308, "https://[::1]:8443/"},
		{"GET / HTTP/1.1\r\nHost: evil.example\r\n\r\n", "GET", 404, ""},
		{"GET / HTTP/1.1\r\n\r\n", "GET", 400, ""},
	} {
		cl := dialClient(t, addr)
		cl.send(tc.req)
		resp, body := cl.read(tc.method)
		if resp.StatusCode != tc.status || resp.Header.Get("Location") != tc.location || !resp.Close {
			t.Errorf("%q: status %d Location %q close %v, want %d %q close", tc.req, resp.StatusCode, resp.Header.Get("Location"), resp.Close, tc.status, tc.location)
		}
		if tc.method == "HEAD" && len(body) != 0 {
			t.Errorf("HEAD got a body: %q", body)
		}
		if tc.status == 308 && tc.method != "HEAD" && !strings.Contains(string(body), tc.location) {
			t.Errorf("body %q does not name the location", body)
		}
		cl.expectClosed(time.Second)
	}
	logs.waitAccess(t, 8)
}

// TestRedirectDefaultPortAndPool: port 443 is left out of the URL, and with
// a default pool every host is redirected.
func TestRedirectDefaultPortAndPool(t *testing.T) {
	addr, _ := startRedirect(t, "default_pool = \"p\"\n"+redirectRoutes, 443)
	cl := dialClient(t, addr)
	cl.send("GET /z HTTP/1.1\r\nHost: other.test\r\n\r\n")
	resp, _ := cl.read("GET")
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://other.test/z" {
		t.Fatalf("status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestRedirectHeaderTimeout: the redirect listener keeps the proxy's header
// deadline.
func TestRedirectHeaderTimeout(t *testing.T) {
	logs := &logBuf{}
	c := parseConfig(t, redirectRoutes)
	c.Timeouts.HeaderRead = 300 * time.Millisecond
	r := NewRedirect(config.NewStore(newSnapshot(t, c)), slog.New(slog.NewJSONHandler(logs, nil)), 443)
	s := NewServer(Limits{}, nil)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, r) }()
	t.Cleanup(func() { s.Close(); <-errc })
	cl := dialClient(t, ln.Addr().String())
	t0 := time.Now()
	cl.send("GET / HTTP/1.1\r\nHost: a.t")
	cl.expectClosed(3 * time.Second)
	if d := time.Since(t0); d > 300*time.Millisecond+time.Second {
		t.Fatalf("closed after %v", d)
	}
}

func TestRedirectValidate(t *testing.T) {
	if err := NewRedirect(config.NewStore(nil), nil, 443).Validate(); err == nil {
		t.Error("nil snapshot accepted")
	}
	if err := NewRedirect(config.NewStore(newSnapshot(t, parseConfig(t, redirectRoutes))), nil, 0).Validate(); err == nil {
		t.Error("port 0 accepted")
	}
}

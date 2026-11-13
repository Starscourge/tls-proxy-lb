package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// --- helpers ---------------------------------------------------------------

type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) access() []map[string]any { return l.records("access") }

// records returns every logged line with message msg, decoded.
func (l *logBuf) records(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(l.buf.Bytes()))
	for {
		var m map[string]any
		if dec.Decode(&m) != nil {
			return out
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
}

// waitAccess waits until n access lines exist and returns them.
func (l *logBuf) waitAccess(t *testing.T, n int) []map[string]any {
	t.Helper()
	var recs []map[string]any
	waitFor(t, "access log lines", func() bool { recs = l.access(); return len(recs) >= n })
	return recs
}

func testTimeouts() config.Timeouts {
	return config.Timeouts{
		Handshake:        2 * time.Second,
		HeaderRead:       2 * time.Second,
		Idle:             5 * time.Second,
		ClientIO:         5 * time.Second,
		UpstreamConnect:  time.Second,
		UpstreamResponse: 5 * time.Second,
	}
}

// parseConfig parses src, a config file without a listener, and sets the
// test timeouts.
func parseConfig(t *testing.T, src string) *config.Config {
	t.Helper()
	c, err := config.Parse("test.toml", []byte(src+"\n[[listener]]\naddress = \"127.0.0.1:0\"\nmode = \"http\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Timeouts = testTimeouts()
	return c
}

// startProxy runs a Proxy whose default pool is backend alone; mut may
// adjust the config before it starts.
func startProxy(t *testing.T, backend string, mut func(*config.Config)) (string, *Server, *logBuf) {
	t.Helper()
	c := parseConfig(t, fmt.Sprintf("default_pool = \"p\"\n[pool.p]\nbackends = [{ address = %q }]\n", backend))
	if mut != nil {
		mut(c)
	}
	return startProxySnapshot(t, newSnapshot(t, c))
}

func newSnapshot(t *testing.T, c *config.Config) *config.Snapshot {
	t.Helper()
	reg := upstream.NewRegistry(c.Upstream.Options())
	t.Cleanup(reg.Close)
	snap, err := config.NewSnapshot(c, reg)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// startProxySnapshot runs a Proxy serving snap.
func startProxySnapshot(t *testing.T, snap *config.Snapshot) (string, *Server, *logBuf) {
	t.Helper()
	logs := &logBuf{}
	p := &Proxy{Config: config.NewStore(snap), Logger: slog.New(slog.NewJSONHandler(logs, nil))}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, p) }()
	t.Cleanup(func() { s.Close(); <-errc })
	return ln.Addr().String(), s, logs
}

// startBackend runs a test backend and returns its address.
func startBackend(t *testing.T, cfg testbackend.Config) (string, *testbackend.Server) {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "b1"
	}
	cfg.Record = true
	b, err := testbackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- b.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Shutdown(ctx)
		<-errc
	})
	return ln.Addr().String(), b
}

// rawBackend runs handle for each connection; cleanup closes everything.
func rawBackend(t *testing.T, handle func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
			wg.Go(func() { defer c.Close(); handle(c) })
		}
	})
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// readRequestFrom reads one request from a raw backend connection.
func readRequestFrom(c net.Conn, br *bufio.Reader) (*http.Request, []byte, error) {
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(req.Body)
	return req, body, err
}

type client struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func dialClient(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{t: t, c: c, br: bufio.NewReader(c)}
}

func (cl *client) send(raw string) {
	cl.t.Helper()
	cl.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(cl.c, raw); err != nil {
		cl.t.Fatalf("send: %v", err)
	}
}

func (cl *client) read(method string) (*http.Response, []byte) {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(cl.br, &http.Request{Method: method})
	if err != nil {
		cl.t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		cl.t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// expectClosed checks that the proxy closes the connection within d.
func (cl *client) expectClosed(d time.Duration) {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(d))
	_, err := cl.br.ReadByte()
	if err == nil {
		cl.t.Fatal("connection still open: read a byte")
	}
	if isTimeout(err) {
		cl.t.Fatalf("connection still open after %v", d)
	}
}

func get(host string) string { return "GET / HTTP/1.1\r\nHost: " + host + "\r\n\r\n" }

// --- forwarding ------------------------------------------------------------

func TestProxyGET(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{Name: "alpha"})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send(get("a.test"))
	resp, body := cl.read("GET")
	if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "alpha" || string(body) != "hello from alpha\n" {
		t.Fatalf("status %d backend %q body %q", resp.StatusCode, resp.Header.Get("X-Backend"), body)
	}
	if resp.Close {
		t.Fatal("keep-alive client was told to close")
	}
}

// TestProxyRequestHeaders checks exactly what the backend receives.
func TestProxyRequestHeaders(t *testing.T) {
	got := make(chan *http.Request, 1)
	baddr := rawBackend(t, func(c net.Conn) {
		req, _, err := readRequestFrom(c, bufio.NewReader(c))
		if err != nil {
			return
		}
		got <- req
		io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
	})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET /path?q=1 HTTP/1.1\r\n" +
		"Host: a.test\r\n" +
		"Connection: keep-alive, X-Hop\r\n" +
		"X-Hop: secret\r\n" +
		"Keep-Alive: timeout=5\r\n" +
		"Proxy-Authorization: Basic Zm9v\r\n" +
		"Upgrade: h2c\r\n" +
		"X-Forwarded-For: 203.0.113.9\r\n" +
		"X-End-To-End: kept\r\n\r\n")
	if resp, _ := cl.read("GET"); resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	req := <-got
	if req.Host != "a.test" || req.URL.RequestURI() != "/path?q=1" {
		t.Errorf("host %q uri %q", req.Host, req.URL.RequestURI())
	}
	for _, h := range []string{"X-Hop", "Keep-Alive", "Proxy-Authorization", "Upgrade", "User-Agent"} {
		if v, ok := req.Header[h]; ok {
			t.Errorf("%s forwarded: %q", h, v)
		}
	}
	if v := req.Header.Get("X-Forwarded-For"); v != "203.0.113.9, 127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q", v)
	}
	if v := req.Header.Get("X-Forwarded-Proto"); v != "http" {
		t.Errorf("X-Forwarded-Proto = %q", v)
	}
	if !req.Close {
		t.Error("backend not asked to close the upstream connection")
	}
	if v := req.Header.Get("X-End-To-End"); v != "kept" {
		t.Errorf("end-to-end header lost: %q", v)
	}
}

func TestProxyResponseHopByHop(t *testing.T) {
	baddr := rawBackend(t, func(c net.Conn) {
		if _, _, err := readRequestFrom(c, bufio.NewReader(c)); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\nConnection: keep-alive, X-Hop\r\nX-Hop: 1\r\n"+
			"Keep-Alive: timeout=5\r\nX-Kept: 2\r\nContent-Length: 2\r\n\r\nok")
	})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send(get("a.test"))
	resp, body := cl.read("GET")
	if _, ok := resp.Header["X-Hop"]; ok {
		t.Error("X-Hop reached the client")
	}
	if _, ok := resp.Header["Keep-Alive"]; ok {
		t.Error("Keep-Alive reached the client")
	}
	if resp.Header.Get("X-Kept") != "2" || string(body) != "ok" {
		t.Errorf("header %q body %q", resp.Header.Get("X-Kept"), body)
	}
}

// TestProxyCloseDelimitedResponse: a body the backend ends by closing is
// re-framed as chunked, and the client connection stays usable.
func TestProxyCloseDelimitedResponse(t *testing.T) {
	baddr := rawBackend(t, func(c net.Conn) {
		if _, _, err := readRequestFrom(c, bufio.NewReader(c)); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nuntil close")
	})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	for range 2 {
		cl.send(get("a.test"))
		resp, body := cl.read("GET")
		if string(body) != "until close" || len(resp.TransferEncoding) != 1 || resp.Close {
			t.Fatalf("body %q TE %v close %v", body, resp.TransferEncoding, resp.Close)
		}
	}
}

func TestProxyHTTP10Client(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET /bytes?n=100&chunked=1 HTTP/1.0\r\n\r\n")
	resp, body := cl.read("GET")
	want, _ := io.ReadAll(testbackend.Pattern(100))
	if !bytes.Equal(body, want) || len(resp.TransferEncoding) != 0 {
		t.Fatalf("HTTP/1.0 client got TE %v, %d bytes", resp.TransferEncoding, len(body))
	}
	cl.expectClosed(2 * time.Second)
}

func TestProxyClientConnectionClose(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET / HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n")
	if resp, _ := cl.read("GET"); !resp.Close {
		t.Fatal("response did not announce Connection: close")
	}
	cl.expectClosed(2 * time.Second)
}

func TestProxyHEAD(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("HEAD /bytes?n=5000 HTTP/1.1\r\nHost: a\r\n\r\n")
	resp, body := cl.read("HEAD")
	if resp.StatusCode != 200 || resp.ContentLength != 5000 || len(body) != 0 {
		t.Fatalf("status %d CL %d body %d", resp.StatusCode, resp.ContentLength, len(body))
	}
	// The connection is still in sync: a GET follows.
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("follow-up status %d", resp.StatusCode)
	}
}

func TestProxyPipelining(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("GET /bytes?n=10 HTTP/1.1\r\nHost: a\r\n\r\n" +
		"POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello" +
		"GET /healthz HTTP/1.1\r\nHost: a\r\n\r\n")
	want := [][]byte{nil, []byte("hello"), []byte("ok\n")}
	want[0], _ = io.ReadAll(testbackend.Pattern(10))
	for i, w := range want {
		if _, body := cl.read("GET"); !bytes.Equal(body, w) {
			t.Fatalf("response %d = %q, want %q", i, body, w)
		}
	}
}

func TestProxyExpectContinue(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n")
	interim, _ := cl.read("POST")
	if interim.StatusCode != 100 {
		t.Fatalf("got %d before sending the body, want 100", interim.StatusCode)
	}
	cl.send("hello")
	resp, body := cl.read("POST")
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

// TestProxyStreamsBothWays: a trickled chunked upload to /echo comes back
// piece by piece while the upload is still going, so neither direction
// waits for a buffer to fill.
func TestProxyStreamsBothWays(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n")
	cl.send("5\r\nfirst\r\n")
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(cl.br, nil)
	if err != nil {
		t.Fatalf("response headers did not arrive mid-upload: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != "first" {
		t.Fatalf("first piece %q, %v", got, err)
	}
	cl.send("6\r\nsecond\r\n")
	got = make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != "second" {
		t.Fatalf("second piece %q, %v", got, err)
	}
	cl.send("0\r\n\r\n")
	if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
		t.Fatalf("tail %q, %v", rest, err)
	}
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 {
		t.Fatalf("follow-up status %d", resp.StatusCode)
	}
}

// TestProxyEarlyResponse: the backend answers 413 without reading a large
// upload and closes. The client still gets the 413, then a close.
func TestProxyEarlyResponse(t *testing.T) {
	baddr := rawBackend(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 413 Payload Too Large\r\nContent-Length: 4\r\nConnection: close\r\n\r\nbig!")
		// Lingering close, as a careful server does, so the reply survives.
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, c)
	})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	const size = 20 << 20
	cl.send("POST /upload HTTP/1.1\r\nHost: a\r\nContent-Length: 20971520\r\n\r\n")
	sendErr := make(chan error, 1)
	go func() {
		cl.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := io.Copy(cl.c, io.LimitReader(testbackend.Pattern(size), size))
		sendErr <- err
	}()
	resp, body := cl.read("POST")
	if resp.StatusCode != 413 || string(body) != "big!" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	cl.expectClosed(5 * time.Second)
	<-sendErr // fails or finishes once the proxy closes; either is fine
}

// --- errors ----------------------------------------------------------------

func TestProxyRejects(t *testing.T) {
	baddr, b := startBackend(t, testbackend.Config{})
	addr, _, logs := startProxy(t, baddr, func(c *config.Config) { c.Limits.MaxHeaderBytes = 8 << 10 })
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"garbage", "NOT HTTP AT ALL\r\n\r\n", 400},
		{"bad header line", "GET / HTTP/1.1\r\nHost: a\r\nno colon here\r\n\r\n", 400},
		{"missing host", "GET / HTTP/1.1\r\n\r\n", 400},
		{"connect", "CONNECT a:443 HTTP/1.1\r\nHost: a:443\r\n\r\n", 405},
		{"http2 preface", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", 505},
		{"huge header", "GET / HTTP/1.1\r\nHost: a\r\nX-Big: " + strings.Repeat("x", 20<<10) + "\r\n\r\n", 431},
		{"many headers", "GET / HTTP/1.1\r\nHost: a\r\n" + strings.Repeat("X-H: 0123456789\r\n", 1000) + "\r\n", 431},
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
		t.Fatalf("backend saw %d rejected requests", n)
	}
	recs := logs.waitAccess(t, len(cases))
	for i, r := range recs[:len(cases)] {
		if int(r["status"].(float64)) != cases[i].want {
			t.Errorf("access log %d status %v, want %d", i, r["status"], cases[i].want)
		}
	}
}

func TestProxyBackendRefused(t *testing.T) {
	addr, _, _ := startProxy(t, refusedAddr(t), nil)
	cl := dialClient(t, addr)
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 502 {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

func TestProxyDialTimeout(t *testing.T) {
	addr, _, _ := startProxy(t, hangingAddr(t), func(c *config.Config) { c.Timeouts.UpstreamConnect = 200 * time.Millisecond })
	cl := dialClient(t, addr)
	t0 := time.Now()
	cl.send(get("a"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 504 {
		t.Fatalf("status %d, want 504", resp.StatusCode)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("504 after %v", d)
	}
}

func TestProxyUpstreamErrors(t *testing.T) {
	cases := []struct {
		name   string
		handle func(c net.Conn, br *bufio.Reader)
		want   int
	}{
		{"silent backend", func(c net.Conn, br *bufio.Reader) {
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			io.Copy(io.Discard, br) // read forever, never answer
		}, 504},
		{"closes without reply", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
		}, 502},
		{"garbage reply", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "SPDY/9 banana\r\n\r\n")
		}, 502},
		{"unexpected 101", func(c net.Conn, br *bufio.Reader) {
			readRequestFrom(c, br)
			io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\nConnection: Upgrade\r\n\r\n")
		}, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baddr := rawBackend(t, func(c net.Conn) { tc.handle(c, bufio.NewReader(c)) })
			addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.UpstreamResponse = 300 * time.Millisecond })
			cl := dialClient(t, addr)
			cl.send(get("a"))
			if resp, _ := cl.read("GET"); resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestProxyTruncatedUpstreamBody: once the status line is out, a backend
// failure can only be signalled by closing the client connection.
func TestProxyTruncatedUpstreamBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(c net.Conn)
	}{
		{"backend closes", func(c net.Conn) {}},
		{"backend stalls", func(c net.Conn) { time.Sleep(3 * time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baddr := rawBackend(t, func(c net.Conn) {
				if _, _, err := readRequestFrom(c, bufio.NewReader(c)); err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nonly-part")
				tc.after(c)
			})
			addr, _, logs := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.UpstreamResponse = 300 * time.Millisecond })
			cl := dialClient(t, addr)
			cl.send(get("a"))
			cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(cl.br, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("body read error %v, want unexpected EOF", err)
			}
			rec := logs.waitAccess(t, 1)[0]
			if !strings.Contains(rec["error"].(string), "upstream body") {
				t.Fatalf("access error %q", rec["error"])
			}
		})
	}
}

// --- timeouts --------------------------------------------------------------

func TestProxyHeaderTimeout(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, logs := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.HeaderRead = 300 * time.Millisecond })
	cl := dialClient(t, addr)
	t0 := time.Now()
	cl.send("GET / HTTP/1.1\r\nHost: a\r\n") // no terminating blank line
	cl.expectClosed(3 * time.Second)
	if d := time.Since(t0); d < 250*time.Millisecond {
		t.Fatalf("closed after %v, before the header timeout", d)
	}
	if s := logs.waitAccess(t, 1)[0]["status"]; s != float64(408) {
		t.Fatalf("access status %v, want 408", s)
	}
}

// TestProxyHeaderTimeoutCoversWholeBlock: trickling header bytes does not
// extend the header deadline (slowloris).
func TestProxyHeaderTimeoutCoversWholeBlock(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.HeaderRead = 400 * time.Millisecond })
	cl := dialClient(t, addr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			if _, err := cl.c.Write([]byte("X")); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	t0 := time.Now()
	cl.expectClosed(3 * time.Second)
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Fatalf("trickling client kept the connection for %v", d)
	}
	<-done
}

func TestProxyIdleTimeout(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.Idle = 300 * time.Millisecond })
	cl := dialClient(t, addr)
	cl.send(get("a"))
	cl.read("GET")
	waitFor(t, "connection to go idle", func() bool {
		l := s.Tracker().List()
		return len(l) == 1 && l[0].State == StateIdle
	})
	t0 := time.Now()
	cl.expectClosed(3 * time.Second)
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Fatalf("idle connection closed after %v", d)
	}
}

// TestProxyClientBodyStall: a client that stops sending its body is
// answered 408 after ClientIO (the backend waits for the whole body), and
// the upstream connection is closed.
func TestProxyClientBodyStall(t *testing.T) {
	upstreamClosed := make(chan struct{})
	baddr := rawBackend(t, func(c net.Conn) {
		_, _, err := readRequestFrom(c, bufio.NewReader(c)) // reads the whole body
		if err != nil && !isTimeout(err) {
			close(upstreamClosed)
		}
	})
	addr, _, _ := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.ClientIO = 300 * time.Millisecond })
	cl := dialClient(t, addr)
	cl.send("POST /upload HTTP/1.1\r\nHost: a\r\nContent-Length: 100\r\n\r\nonly ten b")
	resp, _ := cl.read("POST")
	if resp.StatusCode != 408 {
		t.Fatalf("status %d, want 408", resp.StatusCode)
	}
	cl.expectClosed(3 * time.Second)
	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection left open")
	}
}

// TestProxyClientStallMidStream: once the response is streaming, a client
// that stops sending its body can only be cut off.
func TestProxyClientStallMidStream(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, logs := startProxy(t, baddr, func(c *config.Config) { c.Timeouts.ClientIO = 300 * time.Millisecond })
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 100\r\n\r\nonly ten b")
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(cl.br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("response %v, %v", resp, err)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body error %v, want unexpected EOF", err)
	}
	rec := logs.waitAccess(t, 1)[0]
	if rec["status"] != float64(200) || rec["bytes_in"] != float64(10) {
		t.Fatalf("access %v", rec)
	}
}

func TestProxyMalformedChunk(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\nZZ\r\n")
	resp, _ := cl.read("POST")
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	cl.expectClosed(3 * time.Second)
}

// --- access log ------------------------------------------------------------

func TestProxyAccessLog(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, _, logs := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("POST /echo?x=1 HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nhello")
	cl.read("POST")
	r := logs.waitAccess(t, 1)[0]
	want := map[string]any{
		"client": "127.0.0.1", "method": "POST", "host": "a", "path": "/echo", "status": float64(200),
		"pool": "p", "backend": baddr, "bytes_in": float64(5), "bytes_out": float64(5),
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	up, total := r["upstream_ms"].(float64), r["total_ms"].(float64)
	if up <= 0 || total < up {
		t.Errorf("upstream_ms %v total_ms %v", up, total)
	}
	if _, ok := r["error"]; ok {
		t.Errorf("unexpected error field %v", r["error"])
	}
}

// --- shutdown --------------------------------------------------------------

// TestServerCloseMidRequest: Close cuts off an in-flight request and leaves
// no goroutines (goleak in TestMain checks).
func TestServerCloseMidRequest(t *testing.T) {
	baddr, _ := startBackend(t, testbackend.Config{})
	addr, s, _ := startProxy(t, baddr, nil)
	cl := dialClient(t, addr)
	cl.send("POST /echo HTTP/1.1\r\nHost: a\r\nContent-Length: 1000\r\n\r\npartial")
	waitFor(t, "request in flight", func() bool {
		l := s.Tracker().List()
		return len(l) == 1 && l[0].State == StateActive
	})
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with a request in flight")
	}
	// The echo may already have streamed part of a response; whatever
	// arrived, the connection must end in a close, not a timeout.
	cl.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, cl.br); isTimeout(err) {
		t.Fatal("connection still open after Close")
	}
}

func TestProxyValidate(t *testing.T) {
	if err := (&Proxy{}).Validate(); err == nil {
		t.Error("accepted a proxy without a config store")
	}
	if err := (&Proxy{Config: config.NewStore(nil)}).Validate(); err == nil {
		t.Error("accepted an empty config store")
	}
}

func TestClientIP(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:5000":        "127.0.0.1",
		"[::1]:5000":            "::1",
		"[::ffff:10.0.0.1]:80":  "10.0.0.1",
		"[fe80::1%eth0]:80":     "fe80::1%eth0",
		"not-an-address-at-all": "not-an-address-at-all",
	} {
		if got := clientIP(fakeAddr(in)); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// refusedAddr returns an address that refuses connections: a socket bound
// but not listening, held for the test so no one else can take the port.
func refusedAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, _ := syscall.Getsockname(fd)
	return (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sa.(*syscall.SockaddrInet4).Port}).String()
}

// hangingAddr returns an address where connects hang: a backlog-0 listener
// that never accepts, with its queue already full.
func hangingAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, _ := syscall.Getsockname(fd)
	addr := (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sa.(*syscall.SockaddrInet4).Port}).String()
	for range 4 {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		t.Cleanup(func() { c.Close() })
	}
	return addr
}

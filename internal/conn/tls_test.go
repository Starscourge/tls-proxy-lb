package conn

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
)

// tlsFixture is an HTTPS proxy with routes a.test -> backend "a" and
// b.test -> backend "b", each with its own certificate, and a default pool
// on backend "d".
type tlsFixture struct {
	ca         *tlstest.CA
	addr       string
	logs       *logBuf
	a, b, d    *testbackend.Server
	defaultSNI bool // a default certificate (*.test) is configured
}

// startTLSProxy starts the fixture. withDefault adds a *.test default
// certificate; mut may adjust the config before it starts.
func startTLSProxy(t *testing.T, withDefault bool, mut func(*config.Config)) *tlsFixture {
	t.Helper()
	f := &tlsFixture{ca: tlstest.NewCA(t), defaultSNI: withDefault}
	var aAddr, bAddr, dAddr string
	aAddr, f.a = startBackend(t, testbackend.Config{Name: "a"})
	bAddr, f.b = startBackend(t, testbackend.Config{Name: "b"})
	dAddr, f.d = startBackend(t, testbackend.Config{Name: "d"})
	dir := t.TempDir()
	f.ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	f.ca.WriteFiles(t, dir, "b", tlstest.LeafOptions{}, "b.test")
	f.ca.WriteFiles(t, dir, "default", tlstest.LeafOptions{}, "*.test")
	src := fmt.Sprintf(`
default_pool = "pd"

[[listener]]
address = "127.0.0.1:0"

[[route]]
host = "a.test"
pool = "pa"
cert = "a.crt"
key  = "a.key"

[[route]]
host = "b.test"
pool = "pb"
cert = "b.crt"
key  = "b.key"

[pool.pa]
backends = [{ address = %q }]

[pool.pb]
backends = [{ address = %q }]

[pool.pd]
backends = [{ address = %q }]
`, aAddr, bAddr, dAddr)
	if withDefault {
		src += "\n[tls]\ndefault_cert = \"default.crt\"\ndefault_key = \"default.key\"\n"
	}
	c, err := config.Parse(filepath.Join(dir, "proxy.toml"), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	c.Timeouts = testTimeouts()
	if mut != nil {
		mut(c)
	}
	store := config.NewStore(newSnapshot(t, c))
	f.logs = &logBuf{}
	p := &Proxy{
		Config:    store,
		Logger:    slog.New(slog.NewJSONHandler(f.logs, nil)),
		AccessLog: f.logs,
		TLS:       tlsx.ServerConfig(func() *tlsx.CertSet { return store.Load().Certs }),
	}
	s := NewServer(Limits{}, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, p) }()
	t.Cleanup(func() { s.Close(); <-errc })
	f.addr = ln.Addr().String()
	return f
}

// dial opens a TLS connection with sni (none if empty) and returns a
// client over it. Without SNI there is no name to verify the certificate
// against, so verification is skipped.
func (f *tlsFixture) dial(t *testing.T, sni string) (*client, *tls.Conn) {
	t.Helper()
	cfg := f.ca.ClientConfig(sni)
	cfg.InsecureSkipVerify = sni == ""
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.addr, cfg)
	if err != nil {
		t.Fatalf("dial with SNI %q: %v", sni, err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{t: t, c: c, br: bufio.NewReader(c)}, c
}

// TestTLSRoutesBySNI: each SNI gets its own certificate and pool, on one
// listener, with X-Forwarded-Proto https and keep-alive.
func TestTLSRoutesBySNI(t *testing.T) {
	f := startTLSProxy(t, false, nil)
	for _, host := range []string{"a.test", "b.test"} {
		cl, tc := f.dial(t, host)
		if got := tc.ConnectionState().PeerCertificates[0].DNSNames; len(got) != 1 || got[0] != host {
			t.Fatalf("SNI %s: certificate for %v", host, got)
		}
		for range 3 {
			cl.send(get(host))
			resp, _ := cl.read("GET")
			if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != host[:1] || resp.Close {
				t.Fatalf("SNI %s: status %d X-Backend %q close %v", host, resp.StatusCode, resp.Header.Get("X-Backend"), resp.Close)
			}
		}
	}
	recs := f.a.Requests()
	if len(recs) != 3 || recs[0].Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("backend a saw %+v", recs)
	}
	logs := f.logs.waitAccess(t, 6)
	if l := logs[0]; l["scheme"] != "https" || l["sni"] != "a.test" || l["tls_version"] != "TLS 1.3" || l["tls_resumed"] != false {
		t.Fatalf("access line %v", l)
	}
}

// TestTLSMisdirected: a request whose Host differs from its SNI gets 421
// and reaches no backend, whatever the case or port of the Host.
func TestTLSMisdirected(t *testing.T) {
	f := startTLSProxy(t, true, nil)
	for _, tc := range []struct {
		sni, host string
		want      int
	}{
		{"a.test", "b.test", http.StatusMisdirectedRequest},
		{"a.test", "c.test", http.StatusMisdirectedRequest},
		{"c.test", "a.test", http.StatusMisdirectedRequest},
		{"a.test", "A.Test:8443", http.StatusOK},
		{"c.test", "c.test", http.StatusOK}, // default certificate, default pool
	} {
		cl, _ := f.dial(t, tc.sni)
		cl.send(get(tc.host))
		resp, _ := cl.read("GET")
		if resp.StatusCode != tc.want {
			t.Errorf("SNI %s Host %s: status %d, want %d", tc.sni, tc.host, resp.StatusCode, tc.want)
		}
		if tc.want != 200 {
			cl.expectClosed(time.Second)
		}
	}
	if n := len(f.b.Requests()); n != 0 {
		t.Fatalf("backend b saw %d requests", n)
	}
	if n := len(f.a.Requests()); n != 1 {
		t.Fatalf("backend a saw %d requests, want 1", n)
	}
}

// TestTLSNoSNI: without SNI the client gets the default certificate and
// the default pool; asking for a host with its own route is misdirected.
// Without a default certificate the handshake fails.
func TestTLSNoSNI(t *testing.T) {
	f := startTLSProxy(t, true, nil)
	cl, tc := f.dial(t, "")
	if got := tc.ConnectionState().PeerCertificates[0].DNSNames[0]; got != "*.test" {
		t.Fatalf("no SNI got certificate %q", got)
	}
	cl.send(get("127.0.0.1"))
	if resp, _ := cl.read("GET"); resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "d" {
		t.Fatalf("no SNI, unrouted host: status %d X-Backend %q", resp.StatusCode, resp.Header.Get("X-Backend"))
	}
	cl.send(get("a.test"))
	if resp, _ := cl.read("GET"); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("no SNI, Host a.test: status %d, want 421", resp.StatusCode)
	}

	strict := startTLSProxy(t, false, nil)
	cfg := strict.ca.ClientConfig("")
	cfg.InsecureSkipVerify = true
	for _, sni := range []string{"", "c.test"} {
		cfg.ServerName = sni
		_, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", strict.addr, cfg)
		if err == nil || !strings.Contains(err.Error(), "unrecognized name") {
			t.Errorf("SNI %q without a default certificate: %v, want unrecognized_name", sni, err)
		}
	}
	waitFor(t, "handshake failure log lines", func() bool { return len(strict.logs.records("tls handshake failed")) == 2 })
}

// TestTLSHandshakeTimeout: a client that connects and sends nothing, or
// half a ClientHello, is closed at the handshake timeout.
func TestTLSHandshakeTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	f := startTLSProxy(t, false, func(c *config.Config) { c.Timeouts.Handshake = timeout })
	for name, prefix := range map[string]string{"silent": "", "partial hello": "\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03"} {
		cl := dialClient(t, f.addr)
		t0 := time.Now()
		if prefix != "" {
			cl.send(prefix)
		}
		cl.expectClosed(5 * time.Second)
		if d := time.Since(t0); d < timeout-20*time.Millisecond || d > timeout+time.Second {
			t.Errorf("%s: closed after %v, want about %v", name, d, timeout)
		}
	}
	waitFor(t, "two timeout log lines", func() bool {
		n := 0
		for _, r := range f.logs.records("tls handshake failed") {
			if r["timeout"] == true {
				n++
			}
		}
		return n == 2
	})
}

// TestTLSPlainHTTPToHTTPSPort: a plain HTTP request on the HTTPS port fails
// the handshake and is closed, with a log line and no backend request.
func TestTLSPlainHTTPToHTTPSPort(t *testing.T) {
	f := startTLSProxy(t, false, nil)
	cl := dialClient(t, f.addr)
	cl.send(get("a.test"))
	cl.expectClosed(2 * time.Second)
	waitFor(t, "log line", func() bool { return len(f.logs.records("tls handshake failed")) == 1 })
	if n := len(f.a.Requests()); n != 0 {
		t.Fatalf("backend saw %d requests", n)
	}
}

// TestTLSErrorWithUnreadBody: a rejected request with a body still reaches
// the client intact over TLS (lingering close through the TLS layer), and
// the connection then ends with close_notify, which the client reads as a
// clean EOF.
func TestTLSErrorWithUnreadBody(t *testing.T) {
	f := startTLSProxy(t, false, nil)
	cl, _ := f.dial(t, "a.test")
	body := strings.Repeat("x", 100<<10)
	cl.send(fmt.Sprintf("POST / HTTP/1.1\r\nHost: b.test\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	resp, _ := cl.read("POST")
	if resp.StatusCode != http.StatusMisdirectedRequest || !resp.Close {
		t.Fatalf("status %d close %v, want 421 and close", resp.StatusCode, resp.Close)
	}
	cl.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := cl.br.ReadByte(); err != io.EOF {
		t.Fatalf("after the response: %v, want io.EOF from close_notify", err)
	}
}

// TestTLSSessionResumed: the access log reports a resumed session on a
// second connection.
func TestTLSSessionResumed(t *testing.T) {
	f := startTLSProxy(t, false, nil)
	cfg := f.ca.ClientConfig("a.test")
	cfg.ClientSessionCache = tls.NewLRUClientSessionCache(1)
	for i := range 2 {
		c, err := tls.Dial("tcp", f.addr, cfg)
		if err != nil {
			t.Fatal(err)
		}
		cl := &client{t: t, c: c, br: bufio.NewReader(c)}
		cl.send(get("a.test"))
		cl.read("GET") // also takes in the TLS 1.3 session ticket
		if got := c.ConnectionState().DidResume; got != (i == 1) {
			t.Fatalf("connection %d: DidResume %v", i, got)
		}
		c.Close()
	}
	logs := f.logs.waitAccess(t, 2)
	if logs[0]["tls_resumed"] != false || logs[1]["tls_resumed"] != true {
		t.Fatalf("tls_resumed %v, %v", logs[0]["tls_resumed"], logs[1]["tls_resumed"])
	}
}

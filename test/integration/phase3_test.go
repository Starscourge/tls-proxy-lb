package integration

// Phase 3 "Done when" checks. The Phase 1 and 2 tests run over HTTPS as
// their "https" subtests; the testssl.sh report is bench/phase3/testssl.sh.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
)

// sniFixture is an HTTPS proxy on one port with a.test -> pool_a (backend
// a1) and b.test -> pool_b (backend b1), each with its own certificate,
// and no default pool.
type sniFixture struct {
	*Proxy
	a, b *Backend
}

func startSNIProxy(t *testing.T, opts ProxyOptions) *sniFixture {
	t.Helper()
	a := StartBackend(t, "a1", BackendOptions{})
	b := StartBackend(t, "b1", BackendOptions{})
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	for _, h := range []string{"a.test", "b.test"} {
		ca.WriteFiles(t, dir, h, tlstest.LeafOptions{}, h)
	}
	body := `
[[route]]
host = "a.test"
pool = "pool_a"
cert = "a.test.crt"
key  = "a.test.key"

[[route]]
host = "b.test"
pool = "pool_b"
cert = "b.test.crt"
key  = "b.test.key"
` + pool("pool_a", "round_robin", "", a.Addr) + pool("pool_b", "round_robin", "", b.Addr)
	opts.Scheme = config.ModeHTTPS
	return &sniFixture{Proxy: StartProxyDir(t, dir, ca, body, opts), a: a, b: b}
}

// TestTwoHostsOnePort: on one port, each SNI gets its own certificate and
// its own pool.
func TestTwoHostsOnePort(t *testing.T) {
	f := startSNIProxy(t, ProxyOptions{})
	c := f.Client(t)
	for host, want := range map[string]string{"a.test": "a1", "b.test": "b1"} {
		for range 3 {
			resp, err := c.Get(f.URLFor(host) + "/")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			leaf := resp.TLS.PeerCertificates[0]
			if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != host {
				t.Errorf("%s: certificate for %v", host, leaf.DNSNames)
			}
			if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != want {
				t.Errorf("%s: status %d X-Backend %q, want %s", host, resp.StatusCode, resp.Header.Get("X-Backend"), want)
			}
		}
	}
	if len(f.a.Requests()) != 3 || len(f.b.Requests()) != 3 {
		t.Fatalf("backends saw %d and %d requests, want 3 each", len(f.a.Requests()), len(f.b.Requests()))
	}
	if got := f.a.Requests()[0].Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto %q", got)
	}

	// A client offering h2 and http/1.1, as browsers do, gets http/1.1.
	// (net/http's client offers no ALPN unless HTTP/2 is enabled.)
	cfg := f.CA.ClientConfig("a.test")
	cfg.NextProtos = []string{"h2", "http/1.1"}
	tc, err := tls.Dial("tcp", f.Addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if got := tc.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Errorf("ALPN %q, want http/1.1", got)
	}
}

// handshake dials the proxy with sni (none if empty) and returns the
// certificate's names or the handshake error.
func handshake(p *Proxy, sni string) ([]string, error) {
	cfg := p.CA.ClientConfig(sni)
	cfg.InsecureSkipVerify = sni == "" // nothing to verify a name against
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", p.Addr, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].DNSNames, nil
}

// TestUnknownSNI: with a default certificate, an unknown or missing SNI
// gets it; without one, the handshake fails with unrecognized_name.
func TestUnknownSNI(t *testing.T) {
	t.Run("default certificate", func(t *testing.T) {
		f := startSNIProxy(t, ProxyOptions{})
		for _, sni := range []string{"c.test", ""} {
			names, err := handshake(f.Proxy, sni)
			if err != nil || len(names) != 1 || names[0] != "*.test" {
				t.Errorf("SNI %q: names %v err %v, want the *.test default", sni, names, err)
			}
		}
		// The handshake succeeds; the request then has no route.
		if status, _ := get(t, f.Client(t), f.Proxy, "c.test", nil); status != http.StatusNotFound {
			t.Errorf("c.test: status %d, want 404", status)
		}
	})
	t.Run("no default certificate", func(t *testing.T) {
		f := startSNIProxy(t, ProxyOptions{NoDefaultCert: true})
		for _, sni := range []string{"c.test", ""} {
			_, err := handshake(f.Proxy, sni)
			if err == nil || !strings.Contains(err.Error(), "remote error: tls: unrecognized name") {
				t.Errorf("SNI %q: err %v, want unrecognized_name", sni, err)
			}
		}
		if names, err := handshake(f.Proxy, "a.test"); err != nil || names[0] != "a.test" {
			t.Errorf("a.test still works: %v %v", names, err)
		}
	})
}

// TestTLSVersionsGoClient: TLS 1.0 and 1.1 are refused with
// protocol_version; TLS 1.2 and 1.3 succeed.
func TestTLSVersionsGoClient(t *testing.T) {
	f := startSNIProxy(t, ProxyOptions{})
	for _, v := range []uint16{tls.VersionTLS10, tls.VersionTLS11, tls.VersionTLS12, tls.VersionTLS13} {
		cfg := f.CA.ClientConfig("a.test")
		cfg.MinVersion, cfg.MaxVersion = v, v
		c, err := tls.Dial("tcp", f.Addr, cfg)
		if v < tls.VersionTLS12 {
			if err == nil || !strings.Contains(err.Error(), "protocol version") {
				t.Errorf("%s: err %v, want a protocol_version alert", tls.VersionName(v), err)
			}
			if c != nil {
				c.Close()
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tls.VersionName(v), err)
			continue
		}
		if got := c.ConnectionState().Version; got != v {
			t.Errorf("negotiated %s, want %s", tls.VersionName(got), tls.VersionName(v))
		}
		c.Close()
	}
}

// opensslFixture is the SNI proxy plus the CA file s_client verifies
// against. It skips the test if openssl is not installed.
func opensslFixture(t *testing.T) (*sniFixture, string) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	f := startSNIProxy(t, ProxyOptions{})
	return f, f.CA.WriteCA(t, t.TempDir())
}

// sClient runs openssl s_client against the proxy with SNI a.test and the
// CA, writing stdin, and returns its combined output and exit error.
func sClient(t *testing.T, f *sniFixture, caFile, stdin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := []string{"s_client", "-connect", f.Addr, "-servername", "a.test", "-CAfile", caFile, "-verify_return_error"}
	cmd := exec.CommandContext(ctx, "openssl", append(base, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

const sClientRequest = "GET / HTTP/1.1\r\nHost: a.test\r\nConnection: close\r\n\r\n"

// TestOpenSSLVersions: openssl s_client -tls1_1 fails and -tls1_2 and
// -tls1_3 succeed, verified and reaching the backend.
//
// OpenSSL 3 will not even offer TLS 1.0 or 1.1 at its default security
// level ("no protocols available"), which would pass the test without the
// proxy refusing anything. SECLEVEL=0 makes it send the old ClientHello,
// and the test checks for the proxy's protocol_version alert (70).
func TestOpenSSLVersions(t *testing.T) {
	f, ca := opensslFixture(t)
	for _, v := range []string{"-tls1", "-tls1_1"} {
		out, err := sClient(t, f, ca, "", v, "-cipher", "DEFAULT@SECLEVEL=0")
		if err == nil || !strings.Contains(out, "alert protocol version") {
			t.Errorf("s_client %s: err %v, want a protocol_version alert; output:\n%s", v, err, out)
		}
	}
	for v, proto := range map[string]string{"-tls1_2": "TLSv1.2", "-tls1_3": "TLSv1.3"} {
		out, err := sClient(t, f, ca, sClientRequest, v, "-ign_eof")
		if err != nil || !strings.Contains(out, "New, "+proto+", Cipher is ") ||
			!strings.Contains(out, "Verify return code: 0 (ok)") || !strings.Contains(out, "X-Backend: a1") {
			t.Errorf("s_client %s: err %v; output:\n%s", v, err, out)
		}
	}
}

// TestOpenSSLWeakSuitesRefused: TLS 1.2 without ECDHE, or with CBC, is
// refused.
func TestOpenSSLWeakSuitesRefused(t *testing.T) {
	f, ca := opensslFixture(t)
	for _, suite := range []string{"AES128-GCM-SHA256", "ECDHE-ECDSA-AES128-SHA", "ECDHE-ECDSA-AES256-SHA384"} {
		out, err := sClient(t, f, ca, "", "-tls1_2", "-cipher", suite)
		if err == nil || !strings.Contains(out, "alert handshake failure") {
			t.Errorf("TLS 1.2 with %s: err %v, want a handshake_failure alert; output:\n%s", suite, err, out)
		}
	}
}

// TestOpenSSLSessionReused: openssl s_client -reconnect resumes the TLS 1.2
// session on all five reconnects. For TLS 1.3 the ticket arrives after the
// handshake, which -reconnect does not wait for in OpenSSL 3.0, so the
// session is saved after a request with -sess_out and resumed with
// -sess_in.
func TestOpenSSLSessionReused(t *testing.T) {
	f, ca := opensslFixture(t)
	out, err := sClient(t, f, ca, "", "-tls1_2", "-reconnect")
	if n := strings.Count(out, "Reused, TLSv1.2"); err != nil || n != 5 || !strings.Contains(out, "New, TLSv1.2") {
		t.Errorf("TLS 1.2 -reconnect: err %v, %d reused of 5; output:\n%s", err, n, out)
	}

	sess := filepath.Join(t.TempDir(), "sess.pem")
	if out, err := sClient(t, f, ca, sClientRequest, "-tls1_3", "-ign_eof", "-sess_out", sess); err != nil || !strings.Contains(out, "New, TLSv1.3") {
		t.Fatalf("TLS 1.3 first connection: err %v; output:\n%s", err, out)
	}
	out, err = sClient(t, f, ca, sClientRequest, "-tls1_3", "-ign_eof", "-sess_in", sess)
	if err != nil || !strings.Contains(out, "Reused, TLSv1.3") || !strings.Contains(out, "X-Backend: a1") {
		t.Errorf("TLS 1.3 -sess_in: err %v; output:\n%s", err, out)
	}
}

// TestSNIHostMismatch421: SNI a.test with Host b.test gets 421 and never
// reaches either backend; SNI and Host agreeing still work on the same
// proxy.
func TestSNIHostMismatch421(t *testing.T) {
	f := startSNIProxy(t, ProxyOptions{})
	c := f.Dial(t, "a.test", nil)
	br := bufio.NewReader(c)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: b.test\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusMisdirectedRequest || !resp.Close {
		t.Fatalf("status %d close %v, want 421 and close", resp.StatusCode, resp.Close)
	}

	// The same through an HTTP client: URL (and so SNI) a.test, Host b.test.
	req, _ := http.NewRequest("GET", f.URLFor("a.test")+"/", nil)
	req.Host = "b.test"
	resp, err = f.Client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("client: status %d, want 421", resp.StatusCode)
	}
	if n, m := len(f.a.Requests()), len(f.b.Requests()); n != 0 || m != 0 {
		t.Fatalf("backends saw %d and %d requests, want none", n, m)
	}
	if status, name := get(t, f.Client(t), f.Proxy, "b.test", nil); status != 200 || name != "b1" {
		t.Fatalf("b.test with matching SNI: %d %q", status, name)
	}
}

// TestSilentClientClosedAtHandshakeTimeout: a client that connects and
// sends nothing is closed at the handshake timeout, and its connection is
// no longer tracked.
func TestSilentClientClosedAtHandshakeTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	f := startSNIProxy(t, ProxyOptions{Timeouts: config.Timeouts{Handshake: timeout}})
	c, err := net.Dial("tcp", f.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t0 := time.Now()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(make([]byte, 1))
	d := time.Since(t0)
	if n != 0 || err == nil {
		t.Fatalf("read %d bytes, err %v; want a close", n, err)
	}
	if d < timeout-20*time.Millisecond || d > timeout+time.Second {
		t.Fatalf("closed after %v, want about %v", d, timeout)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.Tracker().Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.Tracker().Len(); n != 0 {
		t.Fatalf("%d connections still tracked", n)
	}
}

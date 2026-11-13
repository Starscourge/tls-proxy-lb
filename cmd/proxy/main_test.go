package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx/tlstest"
)

// runMainEnv, when set, makes the test binary run main() with the arguments
// after "--", so tests can check real exit codes.
const runMainEnv = "PROXY_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		for i, a := range os.Args {
			if a == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				break
			}
		}
		main()
		os.Exit(exitOK)
	}
	goleak.VerifyTestMain(m)
}

// runMain runs the command as a subprocess and returns its exit code and
// output.
func runMain(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodConfig = `
[[listener]]
address = "127.0.0.1:0"
mode = "http"

[[route]]
host = "a.test"
pool = "p"

[pool.p]
backends = [{ address = "127.0.0.1:8000" }]
`

const badConfig = `
[[listener]]
address = "127.0.0.1"
mode = "http"

[[route]]
host = "a.test"
pool = "q"

[pool.p]
algorithm = "fastest"
backends = [{ address = "127.0.0.1:8000" }]
`

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-config", "p.toml", "-check-config"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := (options{configPath: "p.toml", checkConfig: true}); o != want {
		t.Fatalf("got %+v, want %+v", o, want)
	}
	bad := map[string][]string{
		"-config is required": {},
		"unexpected":          {"-config", "p.toml", "stray"},
		"not defined":         {"-backend", "x:1"},
	}
	for want, args := range bad {
		if _, err := parseFlags(args, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseFlags(%v) = %v, want error mentioning %q", args, err, want)
		}
	}
}

func TestCheckConfig(t *testing.T) {
	good := writeFile(t, goodConfig)
	var out, errOut bytes.Buffer
	if code := checkConfig(good, &out, &errOut); code != exitOK || !strings.Contains(out.String(), ": ok (1 listeners, 1 routes, 1 pools, 0 certificates)") {
		t.Fatalf("good config: code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}

	bad := writeFile(t, badConfig)
	out.Reset()
	errOut.Reset()
	if code := checkConfig(bad, &out, &errOut); code != exitFailed || out.Len() != 0 {
		t.Fatalf("bad config: code %d, stdout %q", code, out.String())
	}
	// Every problem, each naming its field, one per line.
	for _, want := range []string{
		bad + `: listener[0].address: "127.0.0.1" is not host:port`,
		bad + `: pool.p.algorithm: "fastest" is not one of`,
		bad + `: route[0].pool: no pool named "q"`,
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr\n%s\nmissing %q", errOut.String(), want)
		}
	}
	if n := strings.Count(strings.TrimSpace(errOut.String()), "\n") + 1; n != 3 {
		t.Errorf("%d lines on stderr, want 3", n)
	}
}

// TestCheckConfigCertificates: certificates are loaded by the check, a bad
// one fails it with its field named, and one about to expire passes with a
// warning on stderr.
func TestCheckConfigCertificates(t *testing.T) {
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{NotAfter: time.Now().Add(72 * time.Hour)}, "a.test")
	ca.WriteFiles(t, dir, "default", tlstest.LeafOptions{NotAfter: time.Now().Add(365 * 24 * time.Hour)}, "*.test")
	src := `
[[listener]]
address = "127.0.0.1:0"

[[route]]
host = "%s"
pool = "p"
cert = "a.crt"
key  = "a.key"

[tls]
default_cert = "default.crt"
default_key  = "default.key"

[pool.p]
backends = [{ address = "127.0.0.1:8000" }]
`
	path := filepath.Join(dir, "proxy.toml")
	for _, tc := range []struct {
		host, stdout, stderr string
		code                 int
	}{
		{"a.test", ": ok (1 listeners, 1 routes, 1 pools, 2 certificates)", "warning: a.test: certificate expires at", exitOK},
		{"b.test", "", `route[0].cert: certificate is not valid for "b.test"`, exitFailed},
	} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf(src, tc.host)), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		code := checkConfig(path, &out, &errOut)
		if code != tc.code || !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
			t.Errorf("host %s: code %d stdout %q stderr %q", tc.host, code, out.String(), errOut.String())
		}
	}
}

// TestCheckConfigExitCodes runs the command itself, as an operator or a
// deploy script would.
func TestCheckConfigExitCodes(t *testing.T) {
	good, bad := writeFile(t, goodConfig), writeFile(t, badConfig)
	missing := filepath.Join(t.TempDir(), "missing.toml")
	for _, tc := range []struct {
		name       string
		args       []string
		code       int
		inStdout   string
		inStderr   string
		quietOther bool
	}{
		{"valid", []string{"-config", good, "-check-config"}, exitOK, ": ok (", "", true},
		{"invalid", []string{"-config", bad, "-check-config"}, exitFailed, "", "pool.p.algorithm", true},
		{"missing file", []string{"-config", missing, "-check-config"}, exitFailed, "", "no such file", true},
		{"no -config", []string{"-check-config"}, exitUsage, "", "-config is required", false},
		{"invalid at startup", []string{"-config", bad}, exitFailed, "", "route[0].pool", true},
		// The error is logged through the buffered log, which must be
		// written out before the process exits.
		{"run fails", []string{"-config", writeFile(t, busyConfig(t))}, exitFailed, `"msg":"proxy failed"`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runMain(t, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.inStdout) || !strings.Contains(stderr, tc.inStderr) {
				t.Fatalf("stdout %q (want %q), stderr %q (want %q)", stdout, tc.inStdout, stderr, tc.inStderr)
			}
			if tc.quietOther && ((tc.inStdout == "" && stdout != "") || (tc.inStderr == "" && stderr != "")) {
				t.Fatalf("unexpected output: stdout %q stderr %q", stdout, stderr)
			}
		})
	}
}

func startBackend(t *testing.T, name string) string {
	t.Helper()
	b, err := testbackend.New(testbackend.Config{Name: name})
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
	return ln.Addr().String()
}

func parse(t *testing.T, src string) *config.Config {
	t.Helper()
	return parseFile(t, "test.toml", src)
}

// parseFile parses src as if read from path, so relative certificate paths
// resolve against path's directory.
func parseFile(t *testing.T, path, src string) *config.Config {
	t.Helper()
	c, err := config.Parse(path, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRunServesEveryListener: an HTTPS, a plain HTTP and a redirect
// listener, each doing its job and routing by host, then a clean stop on
// cancel.
func TestRunServesEveryListener(t *testing.T) {
	one, two := startBackend(t, "one"), startBackend(t, "two")
	dir := t.TempDir()
	ca := tlstest.NewCA(t)
	ca.WriteFiles(t, dir, "a", tlstest.LeafOptions{}, "a.test")
	ca.WriteFiles(t, dir, "default", tlstest.LeafOptions{}, "*.test")
	cfg := parseFile(t, filepath.Join(dir, "proxy.toml"), fmt.Sprintf(`
default_pool = "pb"

[[listener]]
address = "127.0.0.1:0"

[[listener]]
address = "127.0.0.1:0"
mode = "http"

[[listener]]
address = "127.0.0.1:0"
mode = "redirect"
redirect_port = 8443

[[route]]
host = "a.test"
pool = "pa"
cert = "a.crt"
key  = "a.key"

[tls]
default_cert = "default.crt"
default_key  = "default.key"

[pool.pa]
backends = [{ address = %q }]

[pool.pb]
backends = [{ address = %q }]
`, one, two))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan listening, 1)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, cfg, slog.New(slog.DiscardHandler), ready) }()
	var addrs []string
	select {
	case l := <-ready:
		addrs = l.addrs
	case err := <-errc:
		t.Fatalf("run exited early: %v", err)
	}
	if len(addrs) != 3 {
		t.Fatalf("listening on %v, want 3 addresses", addrs)
	}

	// Every host name resolves to the listener being tested, so URLs can
	// carry the host and the client sends it as SNI.
	var target string
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
		},
		TLSClientConfig: ca.ClientConfig(""),
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{
		Transport:     tr,
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	do := func(url string) *http.Response {
		t.Helper()
		resp, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	// The transport pools connections by URL host, so idle ones are
	// dropped whenever the target listener changes.
	for i, scheme := range []string{"https", "http"} {
		tr.CloseIdleConnections()
		target = addrs[i]
		for host, want := range map[string]string{"a.test": "one", "other.test": "two"} {
			resp := do(scheme + "://" + host + "/")
			if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != want {
				t.Fatalf("%s://%s: status %d X-Backend %q, want %s", scheme, host, resp.StatusCode, resp.Header.Get("X-Backend"), want)
			}
			if scheme == "https" && resp.TLS == nil {
				t.Fatalf("%s: no TLS", host)
			}
		}
	}
	tr.CloseIdleConnections()
	target = addrs[2]
	if resp := do("http://a.test/x"); resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://a.test:8443/x" {
		t.Fatalf("redirect listener: status %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	tr.CloseIdleConnections()

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// TestRunFailsOnBusyListener: if any listener cannot open, run fails at
// startup and closes the ones it opened.
func TestRunFailsOnBusyListener(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAddr := free.Addr().String()
	free.Close()

	cfg := parse(t, fmt.Sprintf(`
default_pool = "p"

[[listener]]
address = %q
mode = "http"

[[listener]]
address = %q
mode = "http"

[pool.p]
backends = [{ address = "127.0.0.1:1" }]
`, freeAddr, busy.Addr().String()))
	err = run(context.Background(), cfg, slog.New(slog.DiscardHandler), nil)
	if err == nil || !strings.Contains(err.Error(), "listen "+busy.Addr().String()) {
		t.Fatalf("run = %v, want a listen error for the busy address", err)
	}
	// The first listener was closed again: its port can be bound.
	ln, err := net.Listen("tcp", freeAddr)
	if err != nil {
		t.Fatalf("first listener left open: %v", err)
	}
	ln.Close()
}

// TestRunAdmin: the admin listener serves pprof on its loopback address
// and stops with the proxy.
func TestRunAdmin(t *testing.T) {
	cfg := parse(t, `
default_pool = "p"

[[listener]]
address = "127.0.0.1:0"
mode = "http"

[pool.p]
backends = [{ address = "127.0.0.1:1" }]

[admin]
address = "127.0.0.1:0"
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan listening, 1)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, cfg, slog.New(slog.DiscardHandler), ready) }()
	var l listening
	select {
	case l = <-ready:
	case err := <-errc:
		t.Fatalf("run exited early: %v", err)
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + l.admin + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	c.CloseIdleConnections()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "goroutine profile") {
		t.Fatalf("pprof: status %d body %.80q", resp.StatusCode, body)
	}
	for path, want := range map[string]string{
		"/metrics": `proxy_backend_healthy{backend="127.0.0.1:1"}`,
		"/status":  `"address": "127.0.0.1:1"`,
	} {
		resp, err := c.Get("http://" + l.admin + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Fatalf("%s: status %d, %q not in\n%s", path, resp.StatusCode, want, body)
		}
	}
	c.CloseIdleConnections()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if _, err := net.DialTimeout("tcp", l.admin, time.Second); err == nil {
		t.Fatal("admin listener still open after run returned")
	}
}

// busyConfig returns a config whose listener address is already taken, so
// run fails at startup.
func busyConfig(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return fmt.Sprintf(`
default_pool = "p"

[[listener]]
address = %q
mode = "http"

[pool.p]
backends = [{ address = "127.0.0.1:1" }]
`, ln.Addr().String())
}

// The buffered log is written out by its close function; with buffering
// off, lines go straight to the output.
func TestLogOutput(t *testing.T) {
	var buf bytes.Buffer
	w, closeLog := logOutput(&buf, config.Log{BufferBytes: 1 << 10, FlushInterval: time.Hour})
	fmt.Fprintln(w, "line")
	if buf.Len() != 0 {
		t.Fatal("buffered line written early")
	}
	if err := closeLog(); err != nil || buf.String() != "line\n" {
		t.Fatalf("after close: %q, %v", buf.String(), err)
	}
	buf.Reset()
	w, closeLog = logOutput(&buf, config.Log{})
	fmt.Fprintln(w, "line")
	if buf.String() != "line\n" || closeLog() != nil {
		t.Fatalf("unbuffered: %q", buf.String())
	}
}

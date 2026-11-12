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
	if code := checkConfig(good, &out, &errOut); code != exitOK || !strings.Contains(out.String(), ": ok (1 listeners, 1 routes, 1 pools)") {
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
	c, err := config.Parse("test.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRunServesEveryListener: two listeners, each routing by host, then a
// clean stop on cancel.
func TestRunServesEveryListener(t *testing.T) {
	one, two := startBackend(t, "one"), startBackend(t, "two")
	cfg := parse(t, fmt.Sprintf(`
default_pool = "pb"

[[listener]]
address = "127.0.0.1:0"
mode = "http"

[[listener]]
address = "127.0.0.1:0"
mode = "http"

[[route]]
host = "a.test"
pool = "pa"

[pool.pa]
backends = [{ address = %q }]

[pool.pb]
backends = [{ address = %q }]
`, one, two))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan []string, 1)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, cfg, slog.New(slog.DiscardHandler), ready) }()
	var addrs []string
	select {
	case addrs = <-ready:
	case err := <-errc:
		t.Fatalf("run exited early: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("listening on %v, want 2 addresses", addrs)
	}

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for _, addr := range addrs {
		for host, want := range map[string]string{"a.test": "one", "other.test": "two"} {
			req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
			req.Host = host
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != want {
				t.Fatalf("%s Host %s: status %d X-Backend %q, want %s", addr, host, resp.StatusCode, resp.Header.Get("X-Backend"), want)
			}
		}
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

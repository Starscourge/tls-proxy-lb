package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// proc is the proxy running as a subprocess (main() in the test binary),
// so tests can send it real signals and see its real exit code. Its output
// goes to a file, so no goroutine copies it.
type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  string
	done chan struct{}
	err  error
}

// startProc runs the proxy on the config file at path and waits until addr
// accepts connections.
func startProc(t *testing.T, path, addr string) *proc {
	t.Helper()
	p := &proc{t: t, out: filepath.Join(t.TempDir(), "proxy.log"), done: make(chan struct{})}
	f, err := os.Create(p.out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p.cmd = exec.Command(os.Args[0], "-test.run=^$", "--", "-config", path)
	// A -race binary sleeps a second at exit unless told not to, which
	// would hide when the proxy itself finished.
	p.cmd.Env = append(os.Environ(), runMainEnv+"=1", "GORACE=atexit_sleep_ms=0")
	p.cmd.Stdout, p.cmd.Stderr = f, f
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			out, _ := os.ReadFile(p.out)
			t.Logf("proxy output:\n%s", out)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return p
		}
		select {
		case <-p.done:
			out, _ := os.ReadFile(p.out)
			t.Fatalf("proxy exited at startup: %v\n%s", p.err, out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy not listening on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *proc) signal(sig os.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
}

// wait waits up to d for the process to exit and returns its exit code.
func (p *proc) wait(d time.Duration) int {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(d):
		p.t.Fatalf("proxy still running %v after the signal", d)
	}
	var ee *exec.ExitError
	switch {
	case p.err == nil:
		return 0
	case errors.As(p.err, &ee):
		return ee.ExitCode()
	default:
		p.t.Fatal(p.err)
		return -1
	}
}

// logged returns the log records whose msg starts with prefix.
func (p *proc) logged(prefix string) []map[string]any {
	p.t.Helper()
	data, err := os.ReadFile(p.out)
	if err != nil {
		p.t.Fatal(err)
	}
	var out []map[string]any
	for line := range strings.Lines(string(data)) {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && strings.HasPrefix(fmt.Sprint(r["msg"]), prefix) {
			out = append(out, r)
		}
	}
	return out
}

// waitLogged waits for a record whose msg starts with prefix.
func (p *proc) waitLogged(prefix string) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.logged(prefix)) == 0 {
		if time.Now().After(deadline) {
			p.t.Fatalf("%q not logged", prefix)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// procConfig is a config with one plain HTTP listener on addr, a default
// pool of backend alone, unbuffered logs, and extra appended.
func procConfig(addr, backend, extra string) string {
	return fmt.Sprintf(`
default_pool = "p"

[[listener]]
address = %q
mode = "http"

[pool.p]
backends = [{ address = %q }]

[health]
interval_ms = 0

[log]
buffer_bytes = 0
%s`, addr, backend, extra)
}

// startSlow sends a request for a response that takes d and returns a
// channel that gets the response, or the error.
func startSlow(t *testing.T, addr string, d time.Duration) (<-chan *http.Response, <-chan error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(d + 10*time.Second))
	fmt.Fprintf(c, "GET /slow?ms=%d HTTP/1.1\r\nHost: a.test\r\n\r\n", d.Milliseconds())
	respc, errc := make(chan *http.Response, 1), make(chan error, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
		}
		if err != nil {
			errc <- err
			return
		}
		respc <- resp
	}()
	return respc, errc
}

// Done when: SIGTERM during a 5-second response lets that response
// complete, refuses new connections at once, and exits 0 right after.
func TestSIGTERMDuringResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 5 seconds")
	}
	backend := startBackend(t, "b1")
	addr := freePort(t)
	path := writeFile(t, procConfig(addr, backend, "[timeouts]\nshutdown_ms = 10000\n"))
	p := startProc(t, path, addr)

	respc, errc := startSlow(t, addr, 5*time.Second)
	time.Sleep(500 * time.Millisecond) // the request reaches the backend
	p.signal(syscall.SIGTERM)
	p.waitLogged("proxy shutting down")
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("new connection accepted after SIGTERM")
	}
	var answered time.Time
	select {
	case resp := <-respc:
		answered = time.Now()
		if resp.StatusCode != 200 || !resp.Close || resp.Header.Get(testbackend.HeaderBackend) != "b1" {
			t.Fatalf("status %d close %v backend %q; want 200 from b1 with Connection: close",
				resp.StatusCode, resp.Close, resp.Header.Get(testbackend.HeaderBackend))
		}
	case err := <-errc:
		t.Fatalf("in-flight request failed: %v", err)
	}
	if code := p.wait(5 * time.Second); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if after := time.Since(answered); after > 500*time.Millisecond {
		t.Fatalf("exited %v after the response, want right after", after)
	}
	if recs := p.logged("shutdown timeout reached"); len(recs) != 0 {
		t.Fatal("drain reported a timeout")
	}
}

// Done when: with a shutdown timeout shorter than the response, the
// process exits at the deadline, still with code 0.
func TestSIGTERMDeadline(t *testing.T) {
	backend := startBackend(t, "b1")
	addr := freePort(t)
	path := writeFile(t, procConfig(addr, backend, "[timeouts]\nshutdown_ms = 1000\n"))
	p := startProc(t, path, addr)

	_, errc := startSlow(t, addr, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	sent := time.Now()
	p.signal(syscall.SIGTERM)
	if code := p.wait(5 * time.Second); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if el := time.Since(sent); el < time.Second || el > 1500*time.Millisecond {
		t.Fatalf("exited %v after SIGTERM, want at the 1s deadline", el)
	}
	select {
	case <-errc:
	case <-time.After(time.Second):
		t.Fatal("request cut off at the deadline did not fail")
	}
	if len(p.logged("shutdown timeout reached")) != 1 {
		t.Fatal("timeout not logged")
	}
}

// SIGHUP with a broken config logs the error, counts it, and keeps
// serving; SIGHUP with a good one applies it.
func TestSIGHUP(t *testing.T) {
	b1, b2 := startBackend(t, "b1"), startBackend(t, "b2")
	addr, admin := freePort(t), freePort(t)
	adminCfg := fmt.Sprintf("[admin]\naddress = %q\n", admin)
	path := writeFile(t, procConfig(addr, b1, adminCfg))
	p := startProc(t, path, addr)
	backendOf := func() string {
		t.Helper()
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.Header.Get(testbackend.HeaderBackend)
	}
	metric := func(name string) string {
		t.Helper()
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + admin + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		for line := range strings.Lines(string(body)) {
			if strings.HasPrefix(line, name+" ") {
				return strings.TrimSpace(strings.TrimPrefix(line, name))
			}
		}
		t.Fatalf("%s not in /metrics", name)
		return ""
	}

	replace(t, path, "[pool.p\n")
	p.signal(syscall.SIGHUP)
	p.waitLogged("config reload failed")
	if got := metric(`proxy_config_reloads_total{result="failure"}`); got != "1" {
		t.Fatalf("failures %s, want 1", got)
	}
	if got := metric("proxy_config_last_reload_successful"); got != "0" {
		t.Fatalf("last_reload_successful %s, want 0", got)
	}
	if be := backendOf(); be != "b1" {
		t.Fatalf("after a broken config: %q, want b1", be)
	}

	replace(t, path, procConfig(addr, b2, adminCfg))
	p.signal(syscall.SIGHUP)
	p.waitLogged("config reloaded")
	if be := backendOf(); be != "b2" {
		t.Fatalf("after a good config: %q, want b2", be)
	}
	if got := metric(`proxy_config_reloads_total{result="success"}`); got != "1" {
		t.Fatalf("successes %s, want 1", got)
	}
	p.signal(syscall.SIGINT)
	if code := p.wait(5 * time.Second); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// replace writes src over the file at path by renaming a new file into
// place, as a deploy tool would, so a reload never reads half a file.
func replace(t *testing.T, path, src string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

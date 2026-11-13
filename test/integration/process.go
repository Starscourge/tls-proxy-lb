package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The testbackend binary, built once per test run for ProcBackend.
var (
	buildOnce sync.Once
	binDir    string
	binPath   string
	buildErr  error
)

// testbackendBinary builds cmd/testbackend into a temporary directory,
// once, and returns its path. removeBinary deletes it when the run ends.
func testbackendBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "tls-proxy-lb-integration-")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "testbackend")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/Starscourge/tls-proxy-lb/cmd/testbackend").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build testbackend: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func removeBinary() {
	if binDir != "" {
		os.RemoveAll(binDir)
	}
}

// ProcBackend is a test backend running as a process of its own, so a
// test can do to it what happens to real servers: SIGKILL, which closes
// every socket at once, and SIGSTOP, which leaves them open and silent.
// Its output goes to a file, so no goroutine copies it.
type ProcBackend struct {
	// Addr is 127.0.0.1:port; it stays the same across Restart.
	Addr string

	t    testing.TB
	name string
	args []string
	cmd  *exec.Cmd
	out  *os.File
	runs int
}

// StartProcBackend starts the testbackend binary named name with extra
// flags. It is killed when the test ends.
func StartProcBackend(t testing.TB, name string, args ...string) *ProcBackend {
	t.Helper()
	b := &ProcBackend{t: t, name: name, args: args}
	b.start("0")
	t.Cleanup(b.cleanup)
	return b
}

// start runs a new process on port and waits for it to listen.
func (b *ProcBackend) start(port string) {
	t := b.t
	t.Helper()
	b.runs++
	path := filepath.Join(t.TempDir(), fmt.Sprintf("%s-%d.log", b.name, b.runs))
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-listen", "127.0.0.1", "-port", port, "-name", b.name}, b.args...)
	cmd := exec.Command(testbackendBinary(t), args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		out.Close()
		t.Fatal(err)
	}
	b.cmd, b.out = cmd, out
	addr := waitListening(t, path)
	if b.Addr == "" {
		b.Addr = addr
	} else if addr != b.Addr {
		t.Fatalf("%s restarted on %s, want %s", b.name, addr, b.Addr)
	}
}

// waitListening reads the backend's log until its "listening" line and
// returns the address in it.
func waitListening(t testing.TB, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		for line := range strings.Lines(string(data)) {
			var rec struct{ Msg, Addr string }
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "testbackend listening" {
				return rec.Addr
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("backend did not start:\n%s", data)
	return ""
}

func (b *ProcBackend) signal(sig syscall.Signal) {
	b.t.Helper()
	if err := b.cmd.Process.Signal(sig); err != nil {
		b.t.Fatalf("%s: %v: %v", b.name, sig, err)
	}
}

// Kill sends SIGKILL and waits for the process to exit. The kernel closes
// its sockets: connections in use are reset or closed, and new ones are
// refused.
func (b *ProcBackend) Kill() {
	b.t.Helper()
	b.signal(syscall.SIGKILL)
	b.wait()
}

// Stop sends SIGSTOP. The process keeps its sockets and the kernel keeps
// accepting connections for it, but nothing is read or answered.
func (b *ProcBackend) Stop() { b.t.Helper(); b.signal(syscall.SIGSTOP) }

// Cont sends SIGCONT, resuming a stopped process.
func (b *ProcBackend) Cont() { b.t.Helper(); b.signal(syscall.SIGCONT) }

// Restart starts a new process on the same address. The old one must
// have been killed.
func (b *ProcBackend) Restart() {
	b.t.Helper()
	if b.cmd != nil {
		b.t.Fatalf("%s: Restart while running", b.name)
	}
	_, port, _ := net.SplitHostPort(b.Addr)
	b.start(port)
}

func (b *ProcBackend) wait() {
	_ = b.cmd.Wait()
	b.out.Close()
	b.cmd, b.out = nil, nil
}

func (b *ProcBackend) cleanup() {
	if b.cmd == nil {
		return
	}
	// SIGKILL ends a stopped process too.
	_ = b.cmd.Process.Kill()
	b.wait()
}

package admin

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func start(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Listen(context.Background(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		if err := <-errc; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The index lists the profiles, and the mutex and block profiles are
// served with sampling on as configured.
func TestPprof(t *testing.T) {
	defer runtime.SetMutexProfileFraction(0)
	defer runtime.SetBlockProfileRate(0)
	s := start(t, Options{Address: "127.0.0.1:0", MutexProfileFraction: 5, BlockProfileRate: 1000})
	if got := runtime.SetMutexProfileFraction(-1); got != 5 {
		t.Errorf("mutex profile fraction %d, want 5", got)
	}
	base := "http://" + s.Addr() + "/debug/pprof/"
	code, body := fetch(t, base)
	if code != 200 {
		t.Fatalf("index: %d", code)
	}
	for _, p := range []string{"heap", "goroutine", "mutex", "block", "allocs"} {
		if !strings.Contains(body, p) {
			t.Errorf("index does not list %s", p)
		}
	}
	for _, p := range []string{"mutex?debug=1", "block?debug=1", "heap?debug=1", "goroutine?debug=1", "profile?seconds=1"} {
		if code, _ := fetch(t, base+p); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:9090", "example.com:9090", "nonsense"} {
		if s, err := Listen(context.Background(), Options{Address: addr}, nil); err == nil {
			s.ln.Close()
			t.Errorf("%q accepted", addr)
		}
	}
	for _, addr := range []string{"[::1]:0", "localhost:0"} {
		s, err := Listen(context.Background(), Options{Address: addr}, nil)
		if err != nil {
			t.Logf("%q: %v (no such loopback here?)", addr, err)
			continue
		}
		s.ln.Close()
	}
}

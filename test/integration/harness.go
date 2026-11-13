// Package integration holds the end-to-end tests and the helpers that set up
// their environment: test backends, hand-written raw backends, and the proxy
// running in-process from a generated config file.
//
// Every helper registers its teardown with t.Cleanup, so a test never has to
// stop anything itself and goleak in TestMain sees a clean process.
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// shutdownTimeout bounds each backend's graceful shutdown during cleanup.
const shutdownTimeout = 5 * time.Second

// Backend is a running test backend on a random localhost port.
type Backend struct {
	*testbackend.Server
	// Addr is host:port, ready to put in a proxy config.
	Addr string
	// URL is "http://" + Addr.
	URL string

	t    testing.TB
	name string
	opts BackendOptions
	stop func() // shuts down the current Server
	// listenAddr is where the current Server listens.
	listenAddr string
}

// BackendOptions configures the backends started by StartBackends. The zero
// value gives healthy, instant backends.
type BackendOptions struct {
	Delay    time.Duration
	FailRate float64
	// HealthzFail starts the backend with /healthz failing and every other
	// endpoint working; Server.SetHealthy changes it.
	HealthzFail bool
	// Quiet drops the backend's log lines instead of writing them to the
	// test output, for tests that send thousands of requests. The in-memory
	// request log is kept either way.
	Quiet bool
}

// StartBackends starts n backends named b1..bn, each recording its requests
// and logging to the test output. They are shut down when the test ends.
func StartBackends(t testing.TB, n int, opts BackendOptions) []*Backend {
	t.Helper()
	out := make([]*Backend, 0, n)
	for i := range n {
		out = append(out, StartBackend(t, fmt.Sprintf("b%d", i+1), opts))
	}
	return out
}

// StartBackend starts one named backend. It is shut down when the test ends.
func StartBackend(t testing.TB, name string, opts BackendOptions) *Backend {
	t.Helper()
	b := &Backend{t: t, name: name, opts: opts}
	b.start("127.0.0.1:0")
	b.Addr = b.listenAddr
	b.URL = "http://" + b.Addr
	t.Cleanup(func() { b.stop() })
	return b
}

// Restart shuts the backend down, closing every connection to it, and
// starts a new one with an empty request log on the same address.
func (b *Backend) Restart() {
	b.t.Helper()
	b.stop()
	b.start(b.Addr)
}

// start runs a new Server on addr and sets b.stop to shut it down.
func (b *Backend) start(addr string) {
	t := b.t
	t.Helper()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	if b.opts.Quiet {
		logger = slog.New(slog.DiscardHandler)
	}
	srv, err := testbackend.New(testbackend.Config{
		Name:        b.name,
		Delay:       b.opts.Delay,
		FailRate:    b.opts.FailRate,
		HealthzFail: b.opts.HealthzFail,
		Logger:      logger,
		Record:      true,
	})
	if err != nil {
		t.Fatalf("testbackend %s: %v", b.name, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("testbackend %s: listen: %v", b.name, err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	b.Server = srv
	b.listenAddr = ln.Addr().String()
	b.stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("testbackend %s: shutdown: %v", b.name, err)
		}
		if err := <-errc; err != nil {
			t.Errorf("testbackend %s: serve: %v", b.name, err)
		}
	}
}

// NewClient returns an HTTP client with its own transport and a timeout. Its
// idle connections are closed when the test ends, so they do not show up as
// leaked goroutines.
func NewClient(t testing.TB) *http.Client {
	t.Helper()
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   64,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

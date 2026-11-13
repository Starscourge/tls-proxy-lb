// Package admin implements the admin listener: /metrics in Prometheus
// format, /status as JSON, and the pprof profiles under /debug/pprof/.
//
// It is not on the request path, so it uses http.Server. pprof exposes
// memory contents and can stall the process for a profile's duration, so
// the listener binds only loopback addresses. Prometheus in the lab
// scrapes it from inside the proxy's network namespace.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"runtime"
	"time"
)

// Timeouts for the admin server. WriteTimeout bounds a whole response, so it
// must outlast the longest CPU profile or trace asked for (?seconds=N);
// pprof refuses durations that would not fit.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 5 * time.Minute
	idleTimeout       = time.Minute
)

// Options configures the admin listener.
type Options struct {
	// Address is a loopback host:port.
	Address string
	// MutexProfileFraction and BlockProfileRate, if positive, turn on the
	// mutex and block profiles with runtime.SetMutexProfileFraction and
	// runtime.SetBlockProfileRate. Both are process-wide.
	MutexProfileFraction int
	BlockProfileRate     int
	// Metrics, if set, is served at /metrics.
	Metrics http.Handler
	// Status, if set, is served at /status.
	Status http.Handler
}

// Server is the admin listener.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Listen opens the admin listener and turns on the profiles opts asks for.
// The caller runs Serve and stops it with Shutdown.
func Listen(ctx context.Context, opts Options, logger *slog.Logger) (*Server, error) {
	if err := checkLoopback(opts.Address); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", opts.Address)
	if err != nil {
		return nil, fmt.Errorf("admin: %w", err)
	}
	if opts.MutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(opts.MutexProfileFraction)
	}
	if opts.BlockProfileRate > 0 {
		runtime.SetBlockProfileRate(opts.BlockProfileRate)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{ln: ln, srv: &http.Server{
		Handler:           Handler(opts),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}}, nil
}

// Addr returns the bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve handles requests until Shutdown, then returns nil.
func (s *Server) Serve() error {
	if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin: %w", err)
	}
	return nil
}

// Shutdown stops the listener and waits for requests in progress until ctx
// ends, then closes whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	return err
}

// Handler returns the admin routes: /metrics and /status if opts has them,
// and the pprof index at /debug/pprof/ with every profile under it (heap,
// allocs, goroutine, mutex, block, threadcreate, profile for CPU, trace).
func Handler(opts Options) http.Handler {
	mux := http.NewServeMux()
	if opts.Metrics != nil {
		mux.Handle("/metrics", opts.Metrics)
	}
	if opts.Status != nil {
		mux.Handle("/status", opts.Status)
	}
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// checkLoopback refuses any address whose host is not a loopback IP or
// "localhost". The config parser checks the same; this keeps the guarantee
// for any other caller.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil && a.Unmap().IsLoopback() {
		return nil
	}
	return fmt.Errorf("admin: %q is not a loopback address", addr)
}

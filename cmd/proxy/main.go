// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Each [[listener]] in the config file terminates TLS (mode "https", the
// default), serves plain HTTP ("http") or redirects to HTTPS ("redirect").
// SNI picks the certificate and the pool; requests are balanced across the
// pool by round-robin, least-connections or consistent hashing, over
// upstream connections kept alive and reused. An optional loopback admin
// listener serves pprof.
//
// Usage:
//
//	proxy -config proxy.toml
//	proxy -config proxy.toml -check-config
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/admin"
	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
	"github.com/Starscourge/tls-proxy-lb/internal/logbuf"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1 // bad config, or the proxy failed while running
	exitUsage  = 2 // bad flags
)

type options struct {
	configPath  string
	checkConfig bool
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.configPath, "config", "", "path to the TOML config file (required)")
	fs.BoolVar(&o.checkConfig, "check-config", false, "check the config file, print the result and exit: 0 if valid, 1 if not")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.configPath == "" {
		return o, errors.New("-config is required")
	}
	return o, nil
}

// checkConfig loads the config at path, certificates included, and reports
// on it, like nginx -t: one line on stdout and exitOK if it is valid, every
// problem on stderr and exitFailed if not. Certificate warnings (expired,
// expiring soon) go to stderr without failing the check.
func checkConfig(path string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(path)
	var snap *config.Snapshot
	if err == nil {
		snap, err = config.NewSnapshot(cfg, nil)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	for _, w := range snap.Certs.Warnings(time.Now()) {
		fmt.Fprintf(stderr, "%s: warning: %s\n", path, w)
	}
	certs := snap.Certs.Len()
	if snap.Certs.HasDefault() {
		certs++
	}
	fmt.Fprintf(stdout, "%s: ok (%d listeners, %d routes, %d pools, %d certificates)\n",
		path, len(cfg.Listeners), len(cfg.Routes), len(cfg.Pools), certs)
	return exitOK
}

// handlers returns the connection handler for each listener in snap, in
// order. Every HTTPS listener shares one tls.Config, so a session ticket
// issued on one port resumes on another, and its certificates come from
// whatever snapshot store holds at each handshake.
func handlers(store *config.Store, logger *slog.Logger) ([]conn.Handler, error) {
	tlsConfig := tlsx.ServerConfig(func() *tlsx.CertSet { return store.Load().Certs })
	https := &conn.Proxy{Config: store, Logger: logger, TLS: tlsConfig}
	plain := &conn.Proxy{Config: store, Logger: logger}
	snap := store.Load()
	out := make([]conn.Handler, 0, len(snap.Listeners))
	for _, l := range snap.Listeners {
		switch l.Mode {
		case config.ModeHTTPS:
			out = append(out, https)
		case config.ModeHTTP:
			out = append(out, plain)
		case config.ModeRedirect:
			r := conn.NewRedirect(store, logger, l.RedirectPort)
			if err := r.Validate(); err != nil {
				return nil, fmt.Errorf("listener %s: %w", l.Address, err)
			}
			out = append(out, r)
		default:
			return nil, fmt.Errorf("listener %s: unknown mode %q", l.Address, l.Mode)
		}
	}
	return out, https.Validate()
}

// listening is what run reports once every listener is open.
type listening struct {
	// addrs are the bound proxy listener addresses, in config order.
	addrs []string
	// admin is the bound admin address, or "" without one.
	admin string
}

// adminShutdownTimeout bounds how long a profile in progress may hold up
// shutdown.
const adminShutdownTimeout = 2 * time.Second

// run serves cfg until ctx is cancelled or a listener fails. ready, if
// non-nil, receives the bound addresses once every listener is open.
//
// Goroutines: one accept loop per listener, each started here and stopped
// by srv.Close closing its listener; the admin server's Serve, stopped by
// its Shutdown; and the upstream registry's reaper, stopped by reg.Close.
// run waits for all of them to return.
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger, ready chan<- listening) error {
	reg := upstream.NewRegistry(cfg.Upstream.Options())
	// Deferred first, so it runs after the server has closed every client
	// connection: requests that end later close their upstream connection
	// instead of parking it.
	defer reg.Close()
	snap, err := config.NewSnapshot(cfg, reg)
	if err != nil {
		return err
	}
	for _, w := range snap.Certs.Warnings(time.Now()) {
		logger.Warn("certificate", "warning", w)
	}
	hs, err := handlers(config.NewStore(snap), logger)
	if err != nil {
		return err
	}

	// Open every listener before serving any, so a bad address fails
	// startup instead of leaving a half-open proxy.
	var lc net.ListenConfig
	lns := make([]net.Listener, 0, len(snap.Listeners))
	addrs := make([]string, 0, len(snap.Listeners))
	modes := make([]string, 0, len(snap.Listeners))
	for _, l := range snap.Listeners {
		ln, err := lc.Listen(ctx, "tcp", l.Address)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("listen %s: %w", l.Address, err)
		}
		lns = append(lns, ln)
		addrs = append(addrs, ln.Addr().String())
		modes = append(modes, l.Mode)
	}
	var adm *admin.Server
	if a := snap.Admin; a.Address != "" {
		adm, err = admin.Listen(ctx, admin.Options{
			Address:              a.Address,
			MutexProfileFraction: a.MutexProfileFraction,
			BlockProfileRate:     a.BlockProfileRate,
		}, logger)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return err
		}
	}

	srv := conn.NewServer(snap.Limits.MaxConns, logger)
	// GOMAXPROCS follows the container's CPU limit (Go 1.25 and later), so
	// it can be checked against the limit from here.
	logger.Info("proxy listening", "version", version, "addrs", addrs, "modes", modes,
		"log_buffer_bytes", cfg.Log.BufferBytes,
		"routes", len(cfg.Routes), "pools", len(cfg.Pools), "default_pool", cfg.DefaultPool,
		"max_conns", snap.Limits.MaxConns,
		"max_idle_per_backend", snap.Upstream.MaxIdlePerBackend, "upstream_idle_ms", snap.Upstream.IdleTimeout.Milliseconds(),
		"gomaxprocs", runtime.GOMAXPROCS(0), "num_cpu", runtime.NumCPU())
	started := listening{addrs: addrs}
	adminc := make(chan error, 1)
	if adm != nil {
		started.admin = adm.Addr()
		logger.Info("admin listening", "addr", started.admin, "paths", []string{"/debug/pprof/"},
			"mutex_profile_fraction", snap.Admin.MutexProfileFraction, "block_profile_rate_ns", snap.Admin.BlockProfileRate)
		go func() { adminc <- adm.Serve() }()
	}
	if ready != nil {
		ready <- started
	}

	errc := make(chan error, len(lns))
	for i, ln := range lns {
		go func() { errc <- srv.Serve(ln, hs[i]) }()
	}
	pending := len(lns)
	var serveErr error
	select {
	case err := <-errc:
		pending--
		serveErr = fmt.Errorf("serve: %w", err)
	case err := <-adminc:
		adm = nil
		serveErr = err
	case <-ctx.Done():
		logger.Info("proxy shutting down", "open_conns", srv.Tracker().Len())
	}
	if adm != nil {
		sctx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
		_ = adm.Shutdown(sctx)
		cancel()
		if err := <-adminc; err != nil && serveErr == nil {
			serveErr = err
		}
	}
	srv.Close()
	for range pending {
		if err := <-errc; !errors.Is(err, conn.ErrServerClosed) && serveErr == nil {
			serveErr = fmt.Errorf("serve: %w", err)
		}
	}
	if serveErr != nil {
		return serveErr
	}
	logger.Info("proxy stopped")
	return nil
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(exitOK)
		}
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(exitUsage)
	}
	if o.checkConfig {
		os.Exit(checkConfig(o.configPath, os.Stdout, os.Stderr))
	}
	cfg, err := config.Load(o.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitFailed)
	}
	out, closeLog := logOutput(os.Stdout, cfg.Log)
	logger := slog.New(slog.NewJSONHandler(out, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, cfg, logger, nil)
	stop()
	if err != nil {
		logger.Error("proxy failed", "err", err)
	}
	// os.Exit skips deferred calls, so the buffered log is closed here.
	if cerr := closeLog(); cerr != nil {
		fmt.Fprintln(os.Stderr, "proxy: log:", cerr)
	}
	if err != nil {
		os.Exit(exitFailed)
	}
}

// logOutput returns where log lines go: w through a logbuf.Writer, or w
// itself if the config turns buffering off. The close function writes out
// what is buffered and stops the flusher; it must be called before exit.
func logOutput(w io.Writer, l config.Log) (io.Writer, func() error) {
	if l.BufferBytes <= 0 {
		return w, func() error { return nil }
	}
	b := logbuf.New(w, l.BufferBytes, l.FlushInterval)
	return b, b.Close
}

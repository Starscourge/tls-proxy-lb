// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Phase 3: each [[listener]] in the config file terminates TLS (mode
// "https", the default), serves plain HTTP ("http") or redirects to HTTPS
// ("redirect"). SNI picks the certificate and the pool; requests are
// balanced across the pool by round-robin, least-connections or consistent
// hashing.
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
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
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
		snap, err = config.NewSnapshot(cfg)
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

// run serves cfg until ctx is cancelled or a listener fails. ready, if
// non-nil, receives the bound listener addresses, in config order, once
// every listener is open.
//
// Goroutines: one accept loop per listener, each started here and stopped
// by srv.Close closing its listener; run waits for all of them to return.
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger, ready chan<- []string) error {
	snap, err := config.NewSnapshot(cfg)
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

	srv := conn.NewServer(snap.Limits.MaxConns, logger)
	logger.Info("proxy listening", "version", version, "addrs", addrs, "modes", modes,
		"routes", len(cfg.Routes), "pools", len(cfg.Pools), "default_pool", cfg.DefaultPool,
		"max_conns", snap.Limits.MaxConns)
	if ready != nil {
		ready <- addrs
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
	case <-ctx.Done():
		logger.Info("proxy shutting down", "open_conns", srv.Tracker().Len())
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, logger, nil); err != nil {
		logger.Error("proxy failed", "err", err)
		os.Exit(exitFailed)
	}
}

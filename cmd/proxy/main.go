// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Phase 2: plain HTTP on every [[listener]] in the config file, routed by
// Host to pools balanced by round-robin, least-connections or consistent
// hashing. TLS arrives in Phase 3.
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

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
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

// checkConfig loads the config at path and reports on it, like nginx -t:
// one line on stdout and exitOK if it is valid, every problem on stderr and
// exitFailed if not.
func checkConfig(path string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(path)
	if err == nil {
		_, err = config.NewSnapshot(cfg)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "%s: ok (%d listeners, %d routes, %d pools)\n", path, len(cfg.Listeners), len(cfg.Routes), len(cfg.Pools))
	return exitOK
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
	p := &conn.Proxy{Config: config.NewStore(snap), Logger: logger}
	if err := p.Validate(); err != nil {
		return err
	}

	// Open every listener before serving any, so a bad address fails
	// startup instead of leaving a half-open proxy.
	var lc net.ListenConfig
	lns := make([]net.Listener, 0, len(snap.Listeners))
	addrs := make([]string, 0, len(snap.Listeners))
	for _, a := range snap.Listeners {
		ln, err := lc.Listen(ctx, "tcp", a)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("listen %s: %w", a, err)
		}
		lns = append(lns, ln)
		addrs = append(addrs, ln.Addr().String())
	}

	srv := conn.NewServer(p, snap.Limits.MaxConns, logger)
	logger.Info("proxy listening", "version", version, "addrs", addrs,
		"routes", len(cfg.Routes), "pools", len(cfg.Pools), "default_pool", cfg.DefaultPool,
		"max_conns", snap.Limits.MaxConns)
	if ready != nil {
		ready <- addrs
	}

	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errc <- srv.Serve(ln) }()
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

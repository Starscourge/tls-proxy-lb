// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Phase 1: a plain-HTTP proxy to one backend given on the command line.
// The config file, pools and TLS arrive in Phases 2 and 3.
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

	"github.com/Starscourge/tls-proxy-lb/internal/conn"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	listen         string
	backend        string
	maxConns       int
	maxHeaderBytes int
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.listen, "listen", ":8080", "address to accept client connections on")
	fs.StringVar(&o.backend, "backend", "", "backend host:port every request is sent to (required)")
	fs.IntVar(&o.maxConns, "max-conns", 10000, "maximum open client connections; more are closed at accept")
	fs.IntVar(&o.maxHeaderBytes, "max-header-bytes", conn.DefaultMaxHeaderBytes, "maximum request line plus headers; larger requests get 431")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.backend == "" {
		return o, errors.New("-backend is required")
	}
	if o.maxConns <= 0 {
		return o, fmt.Errorf("-max-conns must be positive, got %d", o.maxConns)
	}
	if o.maxHeaderBytes <= 0 {
		return o, fmt.Errorf("-max-header-bytes must be positive, got %d", o.maxHeaderBytes)
	}
	return o, nil
}

// run serves until ctx is cancelled or the listener fails. ready, if
// non-nil, receives the bound address once the listener is open.
func run(ctx context.Context, o options, logger *slog.Logger, ready chan<- string) error {
	p := &conn.Proxy{
		Backend:        o.backend,
		Timeouts:       conn.DefaultTimeouts(),
		MaxHeaderBytes: o.maxHeaderBytes,
		Logger:         logger,
	}
	if err := p.Validate(); err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.listen)
	if err != nil {
		return err
	}
	srv := conn.NewServer(p, o.maxConns, logger)
	logger.Info("proxy listening", "version", version, "addr", ln.Addr().String(),
		"backend", o.backend, "max_conns", o.maxConns)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		srv.Close()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	logger.Info("proxy shutting down", "open_conns", srv.Tracker().Len())
	srv.Close()
	if err := <-errc; !errors.Is(err, conn.ErrServerClosed) {
		return err
	}
	logger.Info("proxy stopped")
	return nil
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, logger, nil); err != nil {
		logger.Error("proxy failed", "err", err)
		os.Exit(1)
	}
}

// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Phase 0 skeleton: it parses flags, logs that it started and waits for
// SIGINT or SIGTERM. Listeners and the request path arrive in Phase 1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	configPath string
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.configPath, "config", "", "path to the TOML config file (used from Phase 2)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return o, nil
}

// run blocks until ctx is cancelled. It is the place later phases wire the
// listeners, snapshot and background workers into.
func run(ctx context.Context, o options, logger *slog.Logger) error {
	logger.Info("proxy starting", "version", version, "config", o.configPath)
	<-ctx.Done()
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
	if err := run(ctx, o, logger); err != nil {
		logger.Error("proxy failed", "err", err)
		os.Exit(1)
	}
}

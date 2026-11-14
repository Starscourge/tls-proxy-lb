// Command proxy is the TLS-terminating reverse proxy and load balancer.
//
// Each [[listener]] in the config file terminates TLS (mode "https", the
// default), serves plain HTTP ("http") or redirects to HTTPS ("redirect").
// SNI picks the certificate and the pool; requests are balanced across the
// pool by round-robin, least-connections or consistent hashing, over
// upstream connections kept alive and reused. Active and passive health
// checks take failing backends out of rotation, and a request that fails
// before any response is retried on another backend. An optional loopback
// admin listener serves /metrics, /status and pprof.
//
// SIGHUP rereads the config file and applies it without dropping a
// request; an invalid file is logged and the running config kept. SIGTERM
// (or SIGINT) stops accepting, lets requests in progress finish for up to
// [timeouts] shutdown_ms, and exits 0.
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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/app"
	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/logbuf"
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
		snap, err = config.NewSnapshot(cfg, config.Runtime{})
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

// run serves cfg, read from path, until a shutdown signal arrives on sigs
// or a listener fails. ready, if non-nil, receives the bound addresses once
// every listener is open.
//
// sigs is read by this goroutine alone: SIGHUP reloads the config file at
// path (an invalid one is logged and counted, and the old config keeps
// serving); SIGTERM or SIGINT drains open connections for up to the
// config's shutdown_ms and returns nil, whether or not the drain finished
// in time. A listener that fails ends the run with its error, after the
// same drain.
func run(path string, cfg *config.Config, logger *slog.Logger, sigs <-chan os.Signal, ready chan<- app.Listening) error {
	a, err := app.New(cfg, app.Options{Logger: logger, Version: version})
	if err != nil {
		return err
	}
	l, err := a.Start(context.Background())
	if err != nil {
		return err
	}
	if ready != nil {
		ready <- l
	}
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				logger.Info("signal received; reloading the config", "signal", sig.String(), "path", path)
				_ = a.Reload(path) // logged and counted by Reload
				continue
			}
			logger.Info("signal received; shutting down", "signal", sig.String())
			// Connections cut off at the deadline are logged by Shutdown;
			// the exit is still clean.
			_ = a.Shutdown()
			return nil
		case err := <-a.Errors():
			_ = a.Shutdown()
			return err
		}
	}
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
	// Buffered, so a signal that arrives while a reload runs is not lost.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	err = run(o.configPath, cfg, logger, sigs, nil)
	signal.Stop(sigs)
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

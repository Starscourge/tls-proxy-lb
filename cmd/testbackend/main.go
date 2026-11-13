// Command testbackend runs the test backend used by integration tests and the
// Docker lab. See internal/testbackend for the endpoints.
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
	"strconv"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

// shutdownTimeout bounds how long the backend waits for connections to close
// after SIGTERM before closing them forcibly.
const shutdownTimeout = 5 * time.Second

type options struct {
	addr        string
	name        string
	delayMS     int
	failRate    float64
	healthzFail bool
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("testbackend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host, _ := os.Hostname()
	var o options
	port := fs.Int("port", 8000, "TCP port to listen on")
	listen := fs.String("listen", "", "IP to bind (default all interfaces)")
	fs.StringVar(&o.name, "name", host, "backend name, returned in the X-Backend header")
	fs.IntVar(&o.delayMS, "delay-ms", 0, "milliseconds to wait before every response")
	fs.Float64Var(&o.failRate, "fail-rate", 0, "probability in [0,1] that a request gets a 500")
	fs.BoolVar(&o.healthzFail, "healthz-fail", false, "answer /healthz with 500 while every other endpoint works")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *port < 0 || *port > 65535 {
		return o, fmt.Errorf("-port must be in [0, 65535], got %d", *port)
	}
	if o.delayMS < 0 {
		return o, fmt.Errorf("-delay-ms must be >= 0, got %d", o.delayMS)
	}
	if !(o.failRate >= 0 && o.failRate <= 1) {
		return o, fmt.Errorf("-fail-rate must be in [0, 1], got %v", o.failRate)
	}
	if o.name == "" {
		return o, errors.New("-name must not be empty")
	}
	o.addr = net.JoinHostPort(*listen, strconv.Itoa(*port))
	return o, nil
}

// run starts the backend and blocks until ctx is cancelled or serving fails.
// ready, if non-nil, receives the bound address once the listener is open.
func run(ctx context.Context, o options, logger *slog.Logger, ready chan<- string) error {
	srv, err := testbackend.New(testbackend.Config{
		Name:        o.name,
		Delay:       time.Duration(o.delayMS) * time.Millisecond,
		FailRate:    o.failRate,
		HealthzFail: o.healthzFail,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.addr)
	if err != nil {
		return err
	}
	logger.Info("testbackend listening", "addr", ln.Addr().String(), "name", o.name,
		"delay_ms", o.delayMS, "fail_rate", o.failRate, "healthz_fail", o.healthzFail)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("testbackend shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutErr := srv.Shutdown(sctx)
	if err := <-errc; err != nil {
		return err
	}
	return shutErr
}

func main() {
	o, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "testbackend:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, logger, nil); err != nil {
		logger.Error("testbackend failed", "err", err)
		os.Exit(1)
	}
}

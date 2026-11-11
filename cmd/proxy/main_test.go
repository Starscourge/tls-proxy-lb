package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-listen", "127.0.0.1:9000", "-backend", "127.0.0.1:8000", "-max-conns", "5"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := options{listen: "127.0.0.1:9000", backend: "127.0.0.1:8000", maxConns: 5, maxHeaderBytes: 64 << 10}
	if o != want {
		t.Fatalf("got %+v, want %+v", o, want)
	}
	bad := map[string][]string{
		"-backend is required": {},
		"-max-conns":           {"-backend", "x:1", "-max-conns", "0"},
		"-max-header-bytes":    {"-backend", "x:1", "-max-header-bytes", "-1"},
		"unexpected":           {"-backend", "x:1", "stray"},
	}
	for want, args := range bad {
		if _, err := parseFlags(args, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseFlags(%v) = %v, want error mentioning %q", args, err, want)
		}
	}
}

func TestRunProxiesAndStops(t *testing.T) {
	b, err := testbackend.New(testbackend.Config{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	bln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	berr := make(chan error, 1)
	go func() { berr <- b.Serve(bln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b.Shutdown(ctx)
		<-berr
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	errc := make(chan error, 1)
	o := options{listen: "127.0.0.1:0", backend: bln.Addr().String(), maxConns: 10, maxHeaderBytes: 1 << 16}
	go func() { errc <- run(ctx, o, slog.New(slog.DiscardHandler), ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-errc:
		t.Fatalf("run exited early: %v", err)
	}

	tr := &http.Transport{}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	tr.CloseIdleConnections()
	if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "cli" {
		t.Fatalf("status %d X-Backend %q", resp.StatusCode, resp.Header.Get("X-Backend"))
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRunRejectsBadBackend(t *testing.T) {
	err := run(context.Background(), options{listen: "127.0.0.1:0", backend: "no-port", maxConns: 1, maxHeaderBytes: 1}, slog.New(slog.DiscardHandler), nil)
	if err == nil {
		t.Fatal("run accepted a backend without a port")
	}
}

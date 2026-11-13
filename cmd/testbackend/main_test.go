package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"--port", "9001", "--name", "b2", "--delay-ms", "200", "--fail-rate", "0.25", "-healthz-fail"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := options{addr: ":9001", name: "b2", delayMS: 200, failRate: 0.25, healthzFail: true}
	if o != want {
		t.Fatalf("got %+v, want %+v", o, want)
	}

	bad := map[string][]string{
		"port":      {"-port", "70000"},
		"delay-ms":  {"-delay-ms", "-1"},
		"fail-rate": {"-fail-rate", "2"},
		"name":      {"-name", ""},
		"arguments": {"extra"},
	}
	for want, args := range bad {
		if _, err := parseFlags(args, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseFlags(%v) error = %v, want mention of %q", args, err, want)
		}
	}
}

func TestRunServesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, options{addr: "127.0.0.1:0", name: "cli"}, slog.New(slog.DiscardHandler), ready)
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-errc:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not start")
	}

	tr := &http.Transport{}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	tr.CloseIdleConnections()
	if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "cli" {
		t.Fatalf("status %d, X-Backend %q", resp.StatusCode, resp.Header.Get("X-Backend"))
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

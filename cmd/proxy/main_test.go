package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-config", "lab/proxy.toml"}, io.Discard)
	if err != nil || o.configPath != "lab/proxy.toml" {
		t.Fatalf("got %+v, %v", o, err)
	}
	if _, err := parseFlags([]string{"stray"}, io.Discard); err == nil {
		t.Fatal("stray argument accepted")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, options{}, slog.New(slog.DiscardHandler)) }()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

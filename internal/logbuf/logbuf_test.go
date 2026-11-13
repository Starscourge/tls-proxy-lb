package logbuf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// sink records each Write it gets.
type sink struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.writes = append(s.writes, string(p))
	return len(p), nil
}

func (s *sink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// Lines wait in the buffer until it would overflow, then go out in one
// write; a line bigger than the buffer goes out on its own.
func TestWriterBuffers(t *testing.T) {
	s := &sink{}
	w := New(s, 10, time.Hour)
	w.Write([]byte("aaaa\n"))
	w.Write([]byte("bbbb\n"))
	time.Sleep(20 * time.Millisecond)
	if got := s.got(); len(got) != 0 {
		w.Close()
		t.Fatalf("written before the buffer filled: %q", got)
	}
	w.Write([]byte("cc\n"))
	w.Write([]byte(strings.Repeat("x", 20) + "\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"aaaa\nbbbb\n", "cc\n", strings.Repeat("x", 20) + "\n"}
	if got := s.got(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("writes %q, want %q", got, want)
	}
	if _, err := w.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}

func TestWriterFlushesOnInterval(t *testing.T) {
	s := &sink{}
	w := New(s, 1<<10, 20*time.Millisecond)
	defer w.Close()
	w.Write([]byte("line\n"))
	deadline := time.Now().Add(2 * time.Second)
	for len(s.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not flushed by the interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Close writes what is left and stops the flusher (goleak checks it).
func TestWriterClose(t *testing.T) {
	s := &sink{}
	w := New(s, 1<<10, time.Hour)
	w.Write([]byte("last\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.got(); len(got) != 1 || got[0] != "last\n" {
		t.Fatalf("after Close: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// After the underlying writer fails, writes report its error, and so does
// Close.
func TestWriterError(t *testing.T) {
	boom := errors.New("disk full")
	s := &sink{err: boom}
	w := New(s, 8, time.Hour)
	w.Write([]byte("12345"))
	w.Write([]byte("67890")) // hands 12345 to the flusher, which fails
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := w.Write([]byte("x")); errors.Is(err, boom) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("write error never reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := w.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close: %v", err)
	}
}

// Concurrent writers never split or interleave a line.
func TestWriterConcurrent(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	locked := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(p) })
	w := New(locked, 256, time.Millisecond)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 500 {
				fmt.Fprintf(w, "goroutine %d line %03d end\n", g, i)
			}
		})
	}
	wg.Wait()
	w.Close()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 8*500 {
		t.Fatalf("%d lines, want %d", len(lines), 8*500)
	}
	for _, l := range lines {
		var g, i int
		if n, err := fmt.Sscanf(l, "goroutine %d line %d end", &g, &i); n != 2 || err != nil {
			t.Fatalf("broken line %q", l)
		}
	}
}

// Write never waits on the output while a buffer is free: with the
// flusher stuck in a slow write, writers fill the second buffer, and only
// a write that needs a third waits.
func TestWriterDoesNotWaitOnOutput(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var out []string
	first := true
	slow := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			<-release
		}
		mu.Lock()
		out = append(out, string(p))
		mu.Unlock()
		return len(p), nil
	})
	w := New(slow, 8, time.Hour)
	returns := func(p string) chan struct{} {
		ch := make(chan struct{})
		go func() { w.Write([]byte(p)); close(ch) }()
		return ch
	}
	prompt := func(what string, ch chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s waited on the output", what)
		}
	}
	prompt("first buffer", returns("aaaabbbb"))
	prompt("hand-off", returns("cccc")) // flusher now stuck writing aaaabbbb
	prompt("second buffer", returns("d"))
	third := returns("eeee") // needs a third buffer: waits
	select {
	case <-third:
		t.Fatal("Write did not wait with both buffers taken")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	prompt("after the flush", third)
	w.Close()
	if got := strings.Join(out, "|"); got != "aaaabbbb|ccccd|eeee" {
		t.Fatalf("writes %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

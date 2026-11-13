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
// write; a line bigger than the buffer goes straight through.
func TestWriterBuffers(t *testing.T) {
	s := &sink{}
	w := New(s, 10, time.Hour)
	defer w.Close()
	w.Write([]byte("aaaa\n"))
	w.Write([]byte("bbbb\n"))
	if got := s.got(); len(got) != 0 {
		t.Fatalf("written before the buffer filled: %q", got)
	}
	w.Write([]byte("cc\n"))
	w.Write([]byte(strings.Repeat("x", 20) + "\n"))
	want := []string{"aaaa\nbbbb\n", "cc\n", strings.Repeat("x", 20) + "\n"}
	if got := s.got(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("writes %q, want %q", got, want)
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

// After the underlying writer fails, writes report its error.
func TestWriterError(t *testing.T) {
	boom := errors.New("disk full")
	s := &sink{err: boom}
	w := New(s, 8, time.Hour)
	defer w.Close()
	w.Write([]byte("12345"))
	if _, err := w.Write([]byte("67890")); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("later write: err = %v", err)
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

// A slow flush does not hold up writers: they fill the spare buffer while
// the full one is being written, and flushes stay in order.
func TestWriterWritesOutsideTheLock(t *testing.T) {
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
	w.Write([]byte("aaaa"))
	w.Write([]byte("bbbb"))
	done := make(chan struct{})
	go func() { w.Write([]byte("cccc")); close(done) }() // flushes aaaabbbb, blocks in w
	time.Sleep(20 * time.Millisecond)
	// This write lands in the spare buffer without waiting for the flush.
	wrote := make(chan struct{})
	go func() { w.Write([]byte("d")); close(wrote) }()
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("Write waited for a flush in progress")
	}
	close(release)
	<-done
	w.Close()
	if got := strings.Join(out, "|"); got != "aaaabbbb|ccccd" && got != "aaaabbbb|dcccc" {
		t.Fatalf("writes %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

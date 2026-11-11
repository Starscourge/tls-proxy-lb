package httpx

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// tcpPair returns two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	a, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { a.Close(); r.c.Close() })
	return a, r.c
}

func TestBodyCountsAndEOF(t *testing.T) {
	b := NewBody(strings.NewReader("hello world"), nil, 0)
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	if b.N() != 11 || !b.EOF() || b.Err() != nil {
		t.Fatalf("N=%d EOF=%v Err=%v", b.N(), b.EOF(), b.Err())
	}
}

type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) { c.reads++; return c.r.Read(p) }

func TestBodyCloseDoesNotDrain(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	b := NewBody(src, nil, 0)
	buf := make([]byte, 10)
	if _, err := b.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if src.reads != 1 || b.N() != 10 || b.EOF() {
		t.Fatalf("Close read the source: reads=%d N=%d EOF=%v", src.reads, b.N(), b.EOF())
	}
}

func TestBodyErrRecordsFirstError(t *testing.T) {
	boom := errors.New("boom")
	b := NewBody(io.MultiReader(strings.NewReader("ab"), errReader{boom}), nil, 0)
	_, err := io.ReadAll(b)
	if !errors.Is(err, boom) || !errors.Is(b.Err(), boom) || b.EOF() || b.N() != 2 {
		t.Fatalf("err=%v Err=%v EOF=%v N=%d", err, b.Err(), b.EOF(), b.N())
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestBodyDeadlineMovesWithProgress: a sender that trickles one byte every
// 30 ms for 300 ms never trips a 100 ms timeout, because each read extends it.
func TestBodyDeadlineMovesWithProgress(t *testing.T) {
	a, b := tcpPair(t)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 10 {
			time.Sleep(30 * time.Millisecond)
			if _, err := a.Write([]byte{'x'}); err != nil {
				return
			}
		}
		a.(*net.TCPConn).CloseWrite()
	})
	defer wg.Wait()
	body := NewBody(b, b, 100*time.Millisecond)
	got, err := io.ReadAll(body)
	if err != nil || len(got) != 10 {
		t.Fatalf("got %d bytes, err %v", len(got), err)
	}
}

func TestBodyDeadlineFiresOnStall(t *testing.T) {
	_, b := tcpPair(t)
	body := NewBody(b, b, 100*time.Millisecond)
	t0 := time.Now()
	_, err := body.Read(make([]byte, 10))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(t0); d < 90*time.Millisecond || d > time.Second {
		t.Fatalf("timed out after %v, want about 100ms", d)
	}
	if !errors.Is(body.Err(), os.ErrDeadlineExceeded) {
		t.Fatalf("Err = %v", body.Err())
	}
}

func TestBodyAbortUnblocksRead(t *testing.T) {
	_, b := tcpPair(t)
	body := NewBody(b, b, time.Minute)
	errc := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 10))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	body.Abort()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("blocked read returned %v, want ErrAborted", err)
		}
		if !errors.Is(body.Err(), ErrAborted) {
			t.Fatalf("Err = %v, want ErrAborted", body.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Abort did not unblock the read")
	}
	// Later reads fail at once, even though Read sets a fresh deadline.
	if _, err := body.Read(make([]byte, 10)); !errors.Is(err, ErrAborted) {
		t.Fatalf("read after abort: %v, want ErrAborted", err)
	}
}

// recordingConn records the size of each Write.
type recordingConn struct {
	net.Conn
	mu     sync.Mutex
	writes []int
}

func (r *recordingConn) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.writes = append(r.writes, len(p))
	r.mu.Unlock()
	return r.Conn.Write(p)
}

func TestDeadlineWriterReadFromUsesPooledBuffer(t *testing.T) {
	a, b := tcpPair(t)
	rc := &recordingConn{Conn: a}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	done := make(chan []byte, 1)
	go func() {
		got, _ := io.ReadAll(b)
		done <- got
	}()
	// Through a small bufio.Writer, as the proxy uses it: the copy bypasses
	// the 4 KiB buffer once it is empty.
	bw := bufio.NewWriterSize(&DeadlineWriter{Conn: rc, Timeout: 5 * time.Second}, 4096)
	bw.WriteString("header\n")
	if _, err := io.Copy(bw, io.LimitReader(bytes.NewReader(payload), int64(len(payload)))); err != nil {
		t.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	a.(*net.TCPConn).CloseWrite()
	got := <-done
	if !bytes.Equal(got, append([]byte("header\n"), payload...)) {
		t.Fatalf("received %d bytes, want %d", len(got), len(payload)+7)
	}
	var big int
	for _, n := range rc.writes {
		if n > CopyBufferSize {
			t.Fatalf("write of %d bytes exceeds CopyBufferSize", n)
		}
		if n == CopyBufferSize {
			big++
		}
	}
	if big < len(payload)/CopyBufferSize-2 {
		t.Fatalf("only %d full-buffer writes out of %d writes; copy did not use the pooled buffer", big, len(rc.writes))
	}
}

func TestDeadlineWriterTimesOut(t *testing.T) {
	a, _ := tcpPair(t) // the peer never reads
	a.(*net.TCPConn).SetWriteBuffer(4096)
	w := &DeadlineWriter{Conn: a, Timeout: 100 * time.Millisecond}
	chunk := make([]byte, 64<<10)
	t0 := time.Now()
	var err error
	for err == nil && time.Since(t0) < 10*time.Second {
		_, err = w.Write(chunk)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// TestDeadlineWriterExtendRead: with ExtendRead, a read waiting on the same
// connection times out Timeout after the last write, not before.
func TestDeadlineWriterExtendRead(t *testing.T) {
	a, b := tcpPair(t)
	go io.Copy(io.Discard, b)
	w := &DeadlineWriter{Conn: a, Timeout: 150 * time.Millisecond, ExtendRead: true}
	a.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	errc := make(chan error, 1)
	t0 := time.Now()
	go func() {
		_, err := a.Read(make([]byte, 1))
		errc <- err
	}()
	for range 5 {
		time.Sleep(50 * time.Millisecond)
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	err := <-errc
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(t0); d < 350*time.Millisecond {
		t.Fatalf("read timed out after %v; writes did not extend it", d)
	}
}

func TestHeaderLimitReader(t *testing.T) {
	l := &HeaderLimitReader{R: strings.NewReader(strings.Repeat("a", 100))}
	l.Limit(30)
	got, err := io.ReadAll(l)
	if !errors.Is(err, ErrHeaderTooLarge) || len(got) != 30 || !l.Exhausted() {
		t.Fatalf("got %d bytes, err %v, exhausted %v", len(got), err, l.Exhausted())
	}
	l.Unlimit()
	if l.Exhausted() {
		t.Fatal("Exhausted after Unlimit")
	}
	rest, err := io.ReadAll(l)
	if err != nil || len(rest) != 70 {
		t.Fatalf("after Unlimit: %d bytes, %v", len(rest), err)
	}
}

func TestHeaderLimitReaderUnderLimit(t *testing.T) {
	l := &HeaderLimitReader{R: strings.NewReader("short")}
	l.Limit(100)
	got, err := io.ReadAll(l)
	if err != nil || string(got) != "short" || l.Exhausted() {
		t.Fatalf("got %q, %v, exhausted %v", got, err, l.Exhausted())
	}
}

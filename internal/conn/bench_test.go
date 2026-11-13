package conn

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// The benchmarks measure the proxy's own cost per request, allocations
// above all. The client and backend are raw sockets with fixed buffers that
// allocate nothing per request, so allocs/op is the proxy's: header parsing,
// the request and response rewrite, buffers, the dial when there is no reuse
// and the access-log line, which goes to a JSON handler writing to
// io.Discard.
//
//	go test -run '^$' -bench . -benchmem ./internal/conn

const benchBodySize = 1 << 10

var benchResponse = []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: text/plain\r\n\r\nok")

// benchBackend answers every request on a connection with benchResponse and
// closes after a request that asked it to. POST requests carry exactly
// benchBodySize body bytes, which it reads and drops.
func benchBackend(b *testing.B) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := map[net.Conn]struct{}{}
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = struct{}{}
			mu.Unlock()
			wg.Go(func() {
				defer func() {
					mu.Lock()
					delete(conns, c)
					mu.Unlock()
					c.Close()
				}()
				serveBench(c)
			})
		}
	})
	b.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

func serveBench(c net.Conn) {
	br := bufio.NewReaderSize(c, 8<<10)
	body := make([]byte, benchBodySize)
	for {
		post, closeAfter := false, false
		for first := true; ; first = false {
			line, err := br.ReadSlice('\n')
			if err != nil {
				return
			}
			if first {
				post = bytes.HasPrefix(line, []byte("POST "))
			}
			if bytes.EqualFold(line, []byte("Connection: close\r\n")) {
				closeAfter = true
			}
			if len(line) == 2 {
				break
			}
		}
		if post {
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
		}
		if _, err := c.Write(benchResponse); err != nil || closeAfter {
			return
		}
	}
}

// benchProxy runs a proxy in front of backend with reuse on or off.
func benchProxy(b *testing.B, backend string, reuse bool) string {
	b.Helper()
	c, err := config.Parse("bench.toml", fmt.Appendf(nil,
		"default_pool = \"p\"\n[pool.p]\nbackends = [{ address = %q }]\n[[listener]]\naddress = \"127.0.0.1:0\"\nmode = \"http\"\n", backend))
	if err != nil {
		b.Fatal(err)
	}
	c.Timeouts = testTimeouts()
	if !reuse {
		c.Upstream.MaxIdlePerBackend = 0
	}
	reg := upstream.NewRegistry(c.Upstream.Options())
	b.Cleanup(reg.Close)
	snap, err := config.NewSnapshot(c, reg)
	if err != nil {
		b.Fatal(err)
	}
	p := &Proxy{Config: config.NewStore(snap), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	s := NewServer(0, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln, p) }()
	b.Cleanup(func() { s.Close(); <-errc })
	return ln.Addr().String()
}

// benchClient sends req on one keep-alive connection b.N times and reads
// each response, which must have a 2-byte body.
func benchClient(b *testing.B, addr string, req []byte) {
	b.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReaderSize(c, 8<<10)
	body := make([]byte, 2)
	round := func() error {
		if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := c.Write(req); err != nil {
			return err
		}
		line, err := br.ReadSlice('\n')
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(line, []byte("HTTP/1.1 200 ")) {
			return fmt.Errorf("status line %q", line)
		}
		for {
			line, err := br.ReadSlice('\n')
			if err != nil {
				return err
			}
			if len(line) == 2 {
				break
			}
		}
		_, err = io.ReadFull(br, body)
		return err
	}
	// Warm up: the first request dials, and pools fill.
	for range 10 {
		if err := round(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := round(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGET() []byte {
	return []byte("GET /bench HTTP/1.1\r\nHost: bench.test\r\nUser-Agent: bench\r\nAccept: */*\r\n\r\n")
}

func benchPOST() []byte {
	return append([]byte("POST /bench HTTP/1.1\r\nHost: bench.test\r\nUser-Agent: bench\r\nContent-Type: application/octet-stream\r\nContent-Length: "+
		strconv.Itoa(benchBodySize)+"\r\n\r\n"), bytes.Repeat([]byte("x"), benchBodySize)...)
}

func BenchmarkProxy(b *testing.B) {
	for _, reuse := range []bool{false, true} {
		for _, m := range []struct {
			name string
			req  []byte
		}{{"GET", benchGET()}, {"POST", benchPOST()}} {
			b.Run(fmt.Sprintf("%s/reuse=%v", m.name, reuse), func(b *testing.B) {
				benchClient(b, benchProxy(b, benchBackend(b), reuse), m.req)
			})
		}
	}
}

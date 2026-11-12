package integration

// Phase 1 "Done when" checks that run in-process. The wrk run against a
// -race build is bench/phase1/wrk-race.sh.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/testbackend"
)

func sum(r io.Reader) ([32]byte, int64) {
	h := sha256.New()
	n, _ := io.Copy(h, r)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, n
}

// TestBodiesArriveIdentical: GET, a 10 MB POST, a chunked upload and a
// chunked response all arrive byte for byte.
func TestBodiesArriveIdentical(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme})
		c := p.Client(t)

		t.Run("GET with Content-Length", func(t *testing.T) {
			const n = 5 << 20
			resp, err := c.Get(p.URL + fmt.Sprintf("/bytes?n=%d", n))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, gotN := sum(resp.Body)
			want, _ := sum(testbackend.Pattern(n))
			if resp.ContentLength != n || gotN != n || got != want {
				t.Fatalf("CL %d, read %d bytes, digest match %v", resp.ContentLength, gotN, got == want)
			}
		})

		t.Run("POST 10 MB", func(t *testing.T) {
			b.ResetRequests()
			payload := make([]byte, 10<<20)
			rand.Read(payload)
			resp, err := c.Post(p.URL+"/echo", "application/octet-stream", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, gotN := sum(resp.Body)
			if got != sha256.Sum256(payload) {
				t.Fatalf("echo differs: %d bytes back of %d", gotN, len(payload))
			}
			recs := b.Requests()
			if len(recs) != 1 || recs[0].ContentLength != int64(len(payload)) || recs[0].BodyBytes != int64(len(payload)) {
				t.Fatalf("backend saw %+v", recs)
			}
		})

		t.Run("chunked upload", func(t *testing.T) {
			b.ResetRequests()
			payload := make([]byte, 3<<20+17)
			rand.Read(payload)
			pr, pw := io.Pipe()
			go func() {
				// Uneven writes, so chunk boundaries do not line up with buffers.
				for off := 0; off < len(payload); {
					n := min(len(payload)-off, 7919)
					if _, err := pw.Write(payload[off : off+n]); err != nil {
						return
					}
					off += n
				}
				pw.Close()
			}()
			req, _ := http.NewRequest(http.MethodPost, p.URL+"/echo", pr) // no length: chunked
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, gotN := sum(resp.Body)
			if got != sha256.Sum256(payload) {
				t.Fatalf("echo differs: %d bytes back of %d", gotN, len(payload))
			}
			recs := b.Requests()
			if len(recs) != 1 || len(recs[0].TransferEncoding) != 1 || recs[0].TransferEncoding[0] != "chunked" {
				t.Fatalf("backend did not receive a chunked request: %+v", recs)
			}
		})

		t.Run("chunked response", func(t *testing.T) {
			const n = 3<<20 + 5
			resp, err := c.Get(p.URL + fmt.Sprintf("/bytes?n=%d&chunked=1", n))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, gotN := sum(resp.Body)
			want, _ := sum(testbackend.Pattern(n))
			if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
				t.Fatalf("response Transfer-Encoding %v, want chunked", resp.TransferEncoding)
			}
			if gotN != n || got != want {
				t.Fatalf("read %d bytes, digest match %v", gotN, got == want)
			}
		})
	})
}

// TestThousandRequestsOneConnection: 1,000 sequential requests on one
// keep-alive connection all succeed, and the proxy accepted exactly one
// connection.
func TestThousandRequestsOneConnection(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Logger: discard()})
		c := p.Dial(t, "a.test", nil)
		br := bufio.NewReader(c)
		for i := range 1000 {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(c, "GET /?i=%d HTTP/1.1\r\nHost: a.test\r\n\r\n", i)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || string(body) != "hello from b1\n" || resp.Close {
				t.Fatalf("request %d: status %d close %v body %q err %v", i, resp.StatusCode, resp.Close, body, err)
			}
		}
		if n := p.Tracker().Accepted(); n != 1 {
			t.Fatalf("proxy accepted %d connections, want 1", n)
		}
		if n := len(b.Requests()); n != 1000 {
			t.Fatalf("backend saw %d requests, want 1000", n)
		}
	})
}

func TestBackendDownReturns502(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		p := StartProxy(t, RefusedAddr(t), ProxyOptions{Scheme: scheme})
		resp, err := p.Client(t).Get(p.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", resp.StatusCode)
		}
	})
}

// TestBackendTimeoutReturns504: a backend sleeping past UpstreamResponse,
// whether on /slow or through its configured delay, gets 504 at about the
// timeout.
func TestBackendTimeoutReturns504(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		const timeout = 300 * time.Millisecond
		for _, tc := range []struct {
			name  string
			opts  BackendOptions
			path  string
			limit time.Duration
		}{
			{"slow endpoint", BackendOptions{}, "/slow?ms=3000", timeout},
			{"delayed backend", BackendOptions{Delay: 3 * time.Second}, "/", timeout},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := StartBackend(t, "b1", tc.opts)
				p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Timeouts: config.Timeouts{UpstreamResponse: timeout}})
				t0 := time.Now()
				resp, err := p.Client(t).Get(p.URL + tc.path)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				d := time.Since(t0)
				if resp.StatusCode != http.StatusGatewayTimeout {
					t.Fatalf("status %d, want 504", resp.StatusCode)
				}
				if d < timeout || d > timeout+time.Second {
					t.Fatalf("504 after %v, want about %v", d, timeout)
				}
			})
		}
	})
}

// TestHalfHeaderClosedAtTimeout: a client that sends half a header and
// stalls is closed at the header timeout, with no reply.
func TestHalfHeaderClosedAtTimeout(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		const timeout = 300 * time.Millisecond
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Timeouts: config.Timeouts{HeaderRead: timeout}})
		c := p.Dial(t, "a.test", nil)
		t0 := time.Now()
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: a.te")
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(make([]byte, 1))
		d := time.Since(t0)
		if n != 0 || err == nil {
			t.Fatalf("read %d bytes, err %v; want a close with no reply", n, err)
		}
		if d < timeout-20*time.Millisecond || d > timeout+time.Second {
			t.Fatalf("closed after %v, want about %v", d, timeout)
		}
		if n := len(b.Requests()); n != 0 {
			t.Fatalf("backend saw %d requests", n)
		}
	})
}

func countFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count fds: %v", err)
	}
	return len(ents)
}

// TestGoroutinesAndFDsReturnToBaseline: after 200 concurrent keep-alive
// clients finish, goroutine and fd counts return to where they started.
func TestGoroutinesAndFDsReturnToBaseline(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Logger: discard()})
		runtime.GC()
		baseG, baseFD := runtime.NumGoroutine(), countFDs(t)

		const clients, perClient = 200, 50
		var wg sync.WaitGroup
		errs := make(chan error, clients)
		for range clients {
			wg.Go(func() {
				c, err := p.DialErr("a.test", nil)
				if err != nil {
					errs <- err
					return
				}
				defer c.Close()
				br := bufio.NewReader(c)
				for i := range perClient {
					c.SetDeadline(time.Now().Add(10 * time.Second))
					path := "/"
					if i%5 == 0 {
						path = "/bytes?n=50000"
					}
					fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: a.test\r\n\r\n", path)
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						errs <- err
						return
					}
					io.Copy(io.Discard, resp.Body)
					if resp.StatusCode != 200 {
						errs <- fmt.Errorf("status %d", resp.StatusCode)
						return
					}
				}
			})
		}
		peakG := runtime.NumGoroutine()
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("client: %v", err)
		}

		var g, fd int
		deadline := time.Now().Add(5 * time.Second)
		for {
			g, fd = runtime.NumGoroutine(), countFDs(t)
			if (g <= baseG && fd <= baseFD && p.Tracker().Len() == 0) || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("goroutines: baseline %d, during load %d, after %d; fds: baseline %d, after %d", baseG, peakG, g, baseFD, fd)
		if g > baseG || fd > baseFD || p.Tracker().Len() != 0 {
			t.Fatalf("not back to baseline: goroutines %d (baseline %d), fds %d (baseline %d), open conns %d",
				g, baseG, fd, baseFD, p.Tracker().Len())
		}
		if n := len(b.Requests()); n != clients*perClient {
			t.Fatalf("backend saw %d requests, want %d", n, clients*perClient)
		}
	})
}

// TestSlowReaderDoesNotGrowMemory: a client reading a 1 GB response more
// slowly than the backend sends it holds the backend back through TCP flow
// control instead of filling the proxy's memory.
func TestSlowReaderDoesNotGrowMemory(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		if testing.Short() {
			t.Skip("1 GB transfer; skipped with -short")
		}
		const size = 1 << 30
		const limit = 16 << 20 // allowed heap growth
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Timeouts: config.Timeouts{ClientIO: 30 * time.Second}})

		heapInuse := func() uint64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return m.HeapInuse
		}
		runtime.GC()
		base := heapInuse()

		var peak uint64
		stop := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					peak = max(peak, heapInuse())
				}
			}
		}()

		// A small receive buffer keeps the client's own kernel queue from
		// absorbing much of the stream.
		c := p.Dial(t, "a.test", func(tc *net.TCPConn) { tc.SetReadBuffer(64 << 10) })
		fmt.Fprintf(c, "GET /bytes?n=%d HTTP/1.1\r\nHost: a.test\r\n\r\n", size)
		br := bufio.NewReader(c)
		c.SetReadDeadline(time.Now().Add(time.Minute))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		buf := make([]byte, 32<<10)
		var n int64
		for reads := 0; ; reads++ {
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
			m, err := resp.Body.Read(buf)
			n += int64(m)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("after %d bytes: %v", n, err)
			}
			if reads%4 == 3 {
				time.Sleep(time.Millisecond) // at most 128 KiB per ms, about 130 MB/s
			}
		}
		elapsed := time.Since(t0)
		close(stop)
		<-sampled

		growth := int64(peak) - int64(base)
		t.Logf("read %d MiB in %v (%.0f MB/s); heap in use: baseline %.1f MiB, peak %.1f MiB, growth %.1f MiB",
			n>>20, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds()/1e6,
			float64(base)/(1<<20), float64(peak)/(1<<20), float64(growth)/(1<<20))
		if n != size {
			t.Fatalf("read %d bytes, want %d", n, size)
		}
		if growth > limit {
			t.Fatalf("heap grew %d MiB during the transfer, limit %d MiB", growth>>20, limit>>20)
		}
	})
}

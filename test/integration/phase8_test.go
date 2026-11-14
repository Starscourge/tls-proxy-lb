package integration

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
)

// TestUncappedUploadDoesNotGrowMemory: with max_body_bytes off (the
// default), a 1 GiB chunked upload streams through fixed buffers. The
// proxy never holds a body to retry it (a request with a body is retried
// only when the dial fails, before any of it is read), so the size of a
// body costs it nothing but time.
func TestUncappedUploadDoesNotGrowMemory(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		if testing.Short() {
			t.Skip("1 GiB transfer; skipped with -short")
		}
		const size = 1 << 30
		const chunk = 32 << 10
		const limit = 16 << 20 // allowed heap growth
		b := StartBackend(t, "b1", BackendOptions{Quiet: true, NoRecord: true})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Timeouts: config.Timeouts{ClientIO: 30 * time.Second}})

		heapInuse := func() uint64 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return m.HeapInuse
		}
		runtime.GC()
		base := heapInuse()
		var peak uint64
		stop, sampled := make(chan struct{}), make(chan struct{})
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

		c := p.Dial(t, "a.test", nil)
		werr := make(chan error, 1)
		t0 := time.Now()
		go func() {
			bw := bufio.NewWriterSize(c, 64<<10)
			fmt.Fprintf(bw, "POST /echo HTTP/1.1\r\nHost: a.test\r\nTransfer-Encoding: chunked\r\n\r\n")
			data := make([]byte, chunk)
			for sent := 0; sent < size; sent += chunk {
				c.SetWriteDeadline(time.Now().Add(30 * time.Second))
				fmt.Fprintf(bw, "%x\r\n", chunk)
				bw.Write(data)
				if _, err := bw.WriteString("\r\n"); err != nil {
					werr <- err
					return
				}
			}
			bw.WriteString("0\r\n\r\n")
			werr <- bw.Flush()
		}()
		br := bufio.NewReader(c)
		c.SetReadDeadline(time.Now().Add(time.Minute))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 32<<10)
		var n int64
		for {
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
			m, err := resp.Body.Read(buf)
			n += int64(m)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("echo after %d bytes: %v", n, err)
			}
		}
		if err := <-werr; err != nil {
			t.Fatalf("upload: %v", err)
		}
		elapsed := time.Since(t0)
		close(stop)
		<-sampled
		growth := int64(peak) - int64(base)
		t.Logf("streamed %d MiB up and back in %v (%.0f MB/s); heap in use: baseline %.1f MiB, peak %.1f MiB, growth %.1f MiB",
			n>>20, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds()/1e6,
			float64(base)/(1<<20), float64(peak)/(1<<20), float64(growth)/(1<<20))
		if resp.StatusCode != 200 || n != size {
			t.Fatalf("status %d, echoed %d bytes, want 200 and %d", resp.StatusCode, n, size)
		}
		if growth > limit {
			t.Fatalf("heap grew %d MiB during the upload, limit %d MiB", growth>>20, limit>>20)
		}
	})
}

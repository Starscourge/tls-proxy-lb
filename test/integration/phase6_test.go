package integration

// Phase 6 "Done when" checks that need the proxy running. The exact-rate
// test with an injected clock and the million-key memory test are unit
// tests in internal/ratelimit; the k6 run and the mutex profile under load
// are bench/phase6.

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
)

// rateCounts tallies the statuses a paced client got.
type rateCounts struct {
	ok, limited, other atomic.Int64
	// span is the time from the first send to the last, measured on the
	// monotonic clock: the wall clock can jump (WSL2 resyncs it).
	span time.Duration
}

// pace sends GET url from c at rate requests per second for d, without
// waiting for one response before sending the next (an open loop, like k6's
// constant-arrival-rate), and waits for the last.
func pace(c *http.Client, url string, rate float64, d time.Duration, out *rateCounts) {
	interval := time.Duration(float64(time.Second) / rate)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * interval)
		if at.Sub(start) >= d {
			break
		}
		time.Sleep(time.Until(at))
		out.span = time.Since(start)
		wg.Go(func() {
			resp, err := c.Get(url)
			if err != nil {
				out.other.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				out.ok.Add(1)
			case http.StatusTooManyRequests:
				out.limited.Add(1)
			default:
				out.other.Add(1)
			}
		})
	}
	wg.Wait()
}

// TestRateLimitAcceptedRate: one client at twice the limit gets the burst
// and then the configured rate, within 5%, and a second client on another
// address at half the limit is not limited at all.
func TestRateLimitAcceptedRate(t *testing.T) {
	const (
		limit = 100.0
		burst = 10
		run   = 3 * time.Second
	)
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		body := fmt.Sprintf("default_pool = \"default\"\n\n[pool.default]\n%s\n[rate_limit]\nrate = %g\nburst = %d\n",
			Backends(b.Addr), limit, burst)
		p := StartProxyConfig(t, body, ProxyOptions{Scheme: scheme, Admin: true, Logger: discard()})

		var heavy, light rateCounts
		var wg sync.WaitGroup
		wg.Go(func() { pace(p.Client(t), p.URL+"/", 2*limit, run, &heavy) })
		wg.Go(func() { pace(p.ClientFrom(t, "127.0.0.2"), p.URL+"/", limit/2, run, &light) })
		wg.Wait()

		elapsed := heavy.span
		want := burst + limit*elapsed.Seconds()
		got := float64(heavy.ok.Load())
		t.Logf("%s: heavy client sent %d over %v: %d accepted (want %.0f), %d limited; light client: %d accepted, %d limited",
			scheme, heavy.ok.Load()+heavy.limited.Load()+heavy.other.Load(), elapsed.Round(time.Millisecond),
			heavy.ok.Load(), want, heavy.limited.Load(), light.ok.Load(), light.limited.Load())
		if heavy.other.Load() != 0 || light.other.Load() != 0 {
			t.Fatalf("errors or unexpected statuses: heavy %d, light %d", heavy.other.Load(), light.other.Load())
		}
		if math.Abs(got-want) > 0.05*want {
			t.Fatalf("accepted %v requests, want %.0f within 5%%", got, want)
		}
		if heavy.limited.Load() == 0 {
			t.Fatal("the client at twice the limit was never limited")
		}
		if light.limited.Load() != 0 {
			t.Fatalf("the second client was limited %d times", light.limited.Load())
		}

		// /metrics counts the 429s for the default pool's route ("").
		metrics := scrape(t, p)
		line := fmt.Sprintf(`proxy_rate_limited_requests_total{route=""} %d`, heavy.limited.Load())
		if !strings.Contains(metrics, line) {
			t.Fatalf("/metrics has no %q", line)
		}
		if !strings.Contains(metrics, "proxy_rate_limit_buckets 2") {
			t.Fatal("/metrics does not show two buckets")
		}
	})
}

// TestRateLimitPerRoute: each route has its own limit and its own buckets,
// and a route with rate = 0 is not limited.
func TestRateLimitPerRoute(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		body := fmt.Sprintf(`
[[route]]
host = "a.test"
pool = "p"
rate_limit = { rate = 0.01, burst = 2 }

[[route]]
host = "b.test"
pool = "p"
rate_limit = { rate = 0.01, burst = 4 }

[[route]]
host = "c.test"
pool = "p"
rate_limit = { rate = 0 }

[pool.p]
%s
[rate_limit]
rate  = 0.01
burst = 1
`, Backends(b.Addr))
		p := StartProxyConfig(t, body, ProxyOptions{Scheme: scheme, Logger: discard()})
		c := p.Client(t)
		for host, wantOK := range map[string]int{"a.test": 2, "b.test": 4, "c.test": 20} {
			ok := 0
			for range 20 {
				resp, err := c.Get(p.URLFor(host) + "/")
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				switch resp.StatusCode {
				case 200:
					ok++
				case 429:
					if resp.Header.Get("Retry-After") != "100" {
						t.Fatalf("%s: Retry-After %q", host, resp.Header.Get("Retry-After"))
					}
				default:
					t.Fatalf("%s: status %d", host, resp.StatusCode)
				}
			}
			if ok != wantOK {
				t.Fatalf("%s: %d of 20 accepted, want %d", host, ok, wantOK)
			}
		}
	})
}

// TestPerIPConnectionCapAtAccept: connections past the per-IP cap are closed
// at accept, before any TLS handshake, while another address still gets in.
func TestPerIPConnectionCapAtAccept(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{})
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, MaxConnsPerIP: 3, Admin: true, Logger: discard()})
		for range 3 {
			p.Dial(t, "a.test", nil)
		}
		waitFor(t, "three connections tracked", func() bool { return p.Tracker().Len() == 3 })

		c, err := p.DialErr("a.test", nil)
		if scheme == config.ModeHTTPS {
			if err == nil {
				c.Close()
				t.Fatal("handshake completed past the per-IP cap")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if !closedByPeer(c) {
				t.Fatal("connection past the per-IP cap was not closed")
			}
		}
		if n := p.Tracker().RejectedByName("per_ip"); n != 1 {
			t.Fatalf("per-IP rejections %d, want 1", n)
		}

		resp, err := p.ClientFrom(t, "127.0.0.2").Get(p.URL + "/")
		if err != nil {
			t.Fatalf("another address: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("another address: status %d", resp.StatusCode)
		}
		if m := scrape(t, p); !strings.Contains(m, `proxy_client_connections_rejected_total{reason="per_ip"} 1`) {
			t.Fatal("/metrics does not count the per-IP rejection")
		}
	})
}

// closedByPeer writes a request on c and reports whether the proxy closed
// the connection instead of answering.
func closedByPeer(c net.Conn) bool {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n")
	_, err := http.ReadResponse(bufio.NewReader(c), nil)
	return err != nil && !isTimeoutErr(err)
}

func isTimeoutErr(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// scrape fetches /metrics from the admin listener.
func scrape(t *testing.T, p *Proxy) string {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Get(p.AdminURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitFor polls cond every 10 ms for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

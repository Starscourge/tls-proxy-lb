package integration

// Phase 4 "Done when" checks that need the proxy running. The rules for
// when a connection is reused or a request retried are unit-tested in
// internal/conn; the wrk runs, profiles and allocation counts are in
// bench/phase4.

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestUpstreamReuseSteadyState: under steady load from 100 keep-alive
// clients, the backends almost stop seeing new connections once the pools
// have warmed up. Counted at the backends, independently of the proxy.
//
// Each backend's pool grows to the most requests that backend has had in
// flight at once. Round-robin does not even that out, and a stall (the race
// detector, GC) can pile requests onto one backend, so a pool can still grow
// in a burst after warm-up, up to 100 per backend. Without -race the count
// is flat within about two seconds; bench/phase4/reuse.sh records it per
// second under wrk. Here the bound is a fraction of the requests: without
// reuse it would be all of them.
func TestUpstreamReuseSteadyState(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		bs := StartBackends(t, 3, BackendOptions{Quiet: true})
		p := StartProxyConfig(t, "default_pool = \"default\"\n\n[pool.default]\n"+Backends(Addrs(bs)...),
			ProxyOptions{Scheme: scheme, Logger: discard()})
		accepted := func() (n int64) {
			for _, b := range bs {
				n += b.Stats().ConnsAccepted
			}
			return n
		}

		const clients = 100
		var stop atomic.Bool
		var requests atomic.Int64
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
				for !stop.Load() {
					c.SetDeadline(time.Now().Add(10 * time.Second))
					fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n")
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
					requests.Add(1)
				}
			})
		}
		const warmup, measure = 3 * time.Second, 2 * time.Second
		time.Sleep(warmup)
		a0, r0 := accepted(), requests.Load()
		time.Sleep(measure)
		a1, r1 := accepted(), requests.Load()
		stop.Store(true)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("client: %v", err)
		}

		newConns, reqs := a1-a0, r1-r0
		t.Logf("warm-up: %d upstream connections; then %d requests over %v with %d new connections (%.1f/s)",
			a0, reqs, measure, newConns, float64(newConns)/measure.Seconds())
		if reqs < 1000 {
			t.Fatalf("only %d requests in %v", reqs, measure)
		}
		// No backend can have more than 100 requests in flight, so no pool
		// needs more than 100 connections, and none is ever lost.
		if a1 > clients*int64(len(bs)) {
			t.Errorf("%d upstream connections for %d clients and %d backends", a1, clients, len(bs))
		}
		if float64(newConns) > 0.005*float64(reqs) {
			t.Errorf("%d new upstream connections for %d requests after warm-up, want under 0.5%%", newConns, reqs)
		}
	})
}

// TestBackendRestartNoBadGateway: a backend restarted between two requests
// closes every connection the proxy keeps to it. The next request must
// still succeed: the liveness check finds the dead connection, and the
// retry covers one that dies as the request goes out.
func TestBackendRestartNoBadGateway(t *testing.T) {
	ForEachScheme(t, func(t *testing.T, scheme string) {
		b := StartBackend(t, "b1", BackendOptions{Quiet: true})
		logs, logger := NewLogBuffer()
		p := StartProxy(t, b.Addr, ProxyOptions{Scheme: scheme, Logger: logger})
		c := p.Client(t)
		for i := range 5 {
			if status, name := get(t, c, p, "a.test", nil); status != 200 || name != "b1" {
				t.Fatalf("round %d before restart: status %d backend %q", i, status, name)
			}
			b.Restart()
			if status, name := get(t, c, p, "a.test", nil); status != 200 || name != "b1" {
				t.Fatalf("round %d after restart: status %d backend %q", i, status, name)
			}
		}
		for _, r := range logs.Records("access") {
			if r["status"] != float64(200) {
				t.Errorf("access log: %v", r)
			}
		}
	})
}

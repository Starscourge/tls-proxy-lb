package integration

// Phase 2 "Done when" checks that need the proxy running. The unit-level
// ones are in internal/lb (remapping, spread, allocations) and
// internal/config and cmd/proxy (invalid configs).

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// get sends one GET to url with the given Host and headers and returns the
// status and X-Backend.
func get(t testing.TB, c *http.Client, url, host string, hdr http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Backend")
}

// pool renders a pool table.
func pool(name, algorithm, extra string, addrs ...string) string {
	return fmt.Sprintf("\n[pool.%s]\nalgorithm = %q\n%s%s", name, algorithm, extra, Backends(addrs...))
}

// TestRoundRobinExactSplit: 3,000 requests over three backends land exactly
// 1,000 on each, though they come from 30 concurrent clients.
func TestRoundRobinExactSplit(t *testing.T) {
	bs := StartBackends(t, 3, BackendOptions{Quiet: true})
	p := StartProxyConfig(t, `default_pool = "rr"`+"\n"+pool("rr", "round_robin", "", Addrs(bs)...), ProxyOptions{Logger: discard()})
	c := NewClient(t)

	const workers, per = 30, 100
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range per {
				status, name := get(t, c, p.URL+"/", "rr.test", nil)
				if status != 200 {
					t.Errorf("status %d", status)
					return
				}
				mu.Lock()
				got[name]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, b := range bs {
		// Both what the clients saw and what each backend logged.
		if got[b.Name()] != 1000 || len(b.Requests()) != 1000 {
			t.Errorf("%s: %d responses, %d requests logged; want 1000 each", b.Name(), got[b.Name()], len(b.Requests()))
		}
	}
	t.Logf("split: %v", got)
}

// loadSplit runs clients concurrent clients, each sending requests in a
// loop for d, and returns the responses per backend.
func loadSplit(t *testing.T, url string, clients int, d time.Duration) map[string]int {
	t.Helper()
	c := NewClient(t)
	var mu sync.Mutex
	got := map[string]int{}
	var failed atomic.Int64
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for range clients {
		wg.Go(func() {
			for time.Now().Before(stop) {
				status, name := get(t, c, url, "lb.test", nil)
				if status != 200 {
					failed.Add(1)
					continue
				}
				mu.Lock()
				got[name]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Errorf("%d requests failed", n)
	}
	return got
}

func share(got map[string]int, name string) (float64, int) {
	total := 0
	for _, n := range got {
		total += n
	}
	return float64(got[name]) / float64(total), total
}

// TestLeastConnAvoidsSlowBackend: with one of three backends 200 ms slower
// and 50 concurrent clients, least-connections sends the slow backend a
// clearly smaller share. Round-robin, run the same way, is the contrast:
// it sends a third of the requests regardless. The logged split is
// recorded in docs/DESIGN.md.
func TestLeastConnAvoidsSlowBackend(t *testing.T) {
	const clients, d = 50, 3 * time.Second
	fast := StartBackends(t, 2, BackendOptions{Quiet: true})
	slow := StartBackend(t, "slow", BackendOptions{Delay: 200 * time.Millisecond, Quiet: true})
	addrs := append(Addrs(fast), slow.Addr)

	results := map[string]map[string]int{}
	for _, alg := range []string{"round_robin", "least_conn"} {
		p := StartProxyConfig(t, `default_pool = "p"`+"\n"+pool("p", alg, "", addrs...), ProxyOptions{Logger: discard()})
		got := loadSplit(t, p.URL+"/", clients, d)
		results[alg] = got
		s, total := share(got, "slow")
		t.Logf("%-11s %6d requests in %v: b1 %d, b2 %d, slow %d (slow share %.1f%%)",
			alg, total, d, got["b1"], got["b2"], got["slow"], 100*s)
	}

	rr, _ := share(results["round_robin"], "slow")
	lc, _ := share(results["least_conn"], "slow")
	// Round-robin picks are exact; only requests cut off at the end blur it.
	if rr < 0.30 || rr > 0.37 {
		t.Errorf("round-robin slow share %.3f, want about 1/3", rr)
	}
	// "Clearly smaller": under half of a fair third.
	if lc >= 1.0/6 {
		t.Errorf("least-connections slow share %.3f, want < 1/6", lc)
	}
}

// TestConsistentHashSameKeySameBackend: every request for one key reaches
// one backend, through the proxy, for both the header key and the
// client-IP fallback, while different keys spread over the pool.
func TestConsistentHashSameKeySameBackend(t *testing.T) {
	bs := StartBackends(t, 3, BackendOptions{Quiet: true})
	p := StartProxyConfig(t, `default_pool = "ch"`+"\n"+pool("ch", "consistent_hash", "hash_key = \"header:X-User-Id\"\n", Addrs(bs)...), ProxyOptions{Logger: discard()})
	c := NewClient(t)

	const users, per = 60, 10
	owner := map[string]string{}
	used := map[string]bool{}
	for round := range per {
		for u := range users {
			user := fmt.Sprintf("user-%d", u)
			status, name := get(t, c, p.URL+"/", "ch.test", http.Header{"X-User-Id": {user}})
			if status != 200 {
				t.Fatalf("status %d", status)
			}
			if round == 0 {
				owner[user] = name
				used[name] = true
			} else if owner[user] != name {
				t.Fatalf("%s went to %s, then to %s", user, owner[user], name)
			}
		}
	}
	if len(used) != 3 {
		t.Errorf("%d users used only %v", users, used)
	}

	// No header: keyed on the client IP, 127.0.0.1 for every request.
	_, first := get(t, c, p.URL+"/", "ch.test", nil)
	for range 50 {
		if _, name := get(t, c, p.URL+"/", "ch.test", nil); name != first {
			t.Fatalf("client-IP key went to %s, then to %s", first, name)
		}
	}
}

// TestRoutesByHost: hosts on one listener reach their own pools, unknown
// hosts get 404 without a default pool, and backends see Host unchanged.
func TestRoutesByHost(t *testing.T) {
	a := StartBackend(t, "a1", BackendOptions{})
	b := StartBackend(t, "b1", BackendOptions{})
	p := StartProxyConfig(t, `
[[route]]
host = "a.test"
pool = "pool_a"

[[route]]
host = "b.test"
pool = "pool_b"
`+pool("pool_a", "least_conn", "", a.Addr)+pool("pool_b", "round_robin", "", b.Addr), ProxyOptions{})
	c := NewClient(t)
	for host, want := range map[string]string{"a.test": "a1", "B.test:443": "b1"} {
		if status, name := get(t, c, p.URL+"/", host, nil); status != 200 || name != want {
			t.Errorf("Host %s: status %d X-Backend %q, want %s", host, status, name, want)
		}
	}
	if status, name := get(t, c, p.URL+"/", "c.test", nil); status != http.StatusNotFound || name != "" {
		t.Errorf("unknown host: status %d X-Backend %q, want 404 from the proxy", status, name)
	}
	if recs := b.Requests(); len(recs) != 1 || recs[0].Host != "B.test:443" {
		t.Errorf("pool_b backend saw %+v", recs)
	}
}

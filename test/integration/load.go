package integration

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Load sends GET requests to the proxy from a number of workers, each
// with its own keep-alive connection, one request after another, until
// Stop. It records every failure with the time its request started.
type Load struct {
	stop atomic.Bool
	wg   sync.WaitGroup
	ok   atomic.Int64

	mu       sync.Mutex
	failures []LoadFailure
	backends map[string]int64 // successes by X-Backend
}

// LoadFailure is one failed request: a status other than 200 or an error.
type LoadFailure struct {
	Start time.Time
	Err   string
}

// StartLoad starts workers sending GET path to p. It is stopped when the
// test ends, if not before.
func StartLoad(t testing.TB, p *Proxy, workers int, path string) *Load {
	t.Helper()
	l := &Load{backends: map[string]int64{}}
	url := p.URL + path
	for range workers {
		c := p.Client(t)
		c.Timeout = 10 * time.Second
		l.wg.Go(func() {
			for !l.stop.Load() {
				start := time.Now()
				backend, err := getOnce(c, url)
				l.mu.Lock()
				if err != nil {
					l.failures = append(l.failures, LoadFailure{Start: start, Err: err.Error()})
				} else {
					l.ok.Add(1)
					l.backends[backend]++
				}
				l.mu.Unlock()
			}
		})
	}
	t.Cleanup(l.Stop)
	return l
}

func getOnce(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return "", fmt.Errorf("status %d, body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Header.Get("X-Backend"), nil
}

// Stop stops the workers and waits for their last requests.
func (l *Load) Stop() {
	l.stop.Store(true)
	l.wg.Wait()
}

// OK returns the number of successful requests.
func (l *Load) OK() int64 { return l.ok.Load() }

// Failures returns every failure so far.
func (l *Load) Failures() []LoadFailure {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LoadFailure(nil), l.failures...)
}

// Split counts the failures of requests started before t and at or after
// it.
func (l *Load) Split(t time.Time) (before, after int) {
	for _, f := range l.Failures() {
		if f.Start.Before(t) {
			before++
		} else {
			after++
		}
	}
	return before, after
}

// Backends returns the successes by the backend that answered.
func (l *Load) Backends() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.backends))
	for k, v := range l.backends {
		out[k] = v
	}
	return out
}

// Reset clears the counts and failures.
func (l *Load) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = nil
	l.backends = map[string]int64{}
	l.ok.Store(0)
}

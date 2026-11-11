package testbackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// start runs a Server on a random localhost port and returns its base URL and
// a client. Everything is torn down when the test ends.
func start(t *testing.T, cfg Config) (*Server, string, *http.Client) {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "b1"
	}
	cfg.Record = true
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()
	tr := &http.Transport{}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-errc; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, "http://" + ln.Addr().String(), client
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestNewValidates(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing name", Config{}, "Name"},
		{"negative delay", Config{Name: "x", Delay: -time.Second}, "Delay"},
		{"fail rate above 1", Config{Name: "x", FailRate: 1.5}, "FailRate"},
		{"fail rate below 0", Config{Name: "x", FailRate: -0.1}, "FailRate"},
		{"fail rate NaN", Config{Name: "x", FailRate: math.NaN()}, "FailRate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New(%+v) error = %v, want mention of %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestEndpoints(t *testing.T) {
	_, base, c := start(t, Config{Name: "alpha"})
	cases := []struct {
		path string
		want string
	}{
		{"/", "hello from alpha\n"},
		{"/any/other/path", "hello from alpha\n"},
		{"/healthz", "ok\n"},
		{"/slow?ms=1", "slow response from alpha after 1ms\n"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := get(t, c, base+tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get(HeaderBackend); got != "alpha" {
				t.Fatalf("%s = %q, want alpha", HeaderBackend, got)
			}
			if body != tc.want {
				t.Fatalf("body = %q, want %q", body, tc.want)
			}
		})
	}
}

func TestSlowBadParam(t *testing.T) {
	_, base, c := start(t, Config{})
	for _, q := range []string{"abc", "-5"} {
		resp, _ := get(t, c, base+"/slow?ms="+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("ms=%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestSlowSleeps(t *testing.T) {
	_, base, c := start(t, Config{})
	t0 := time.Now()
	get(t, c, base+"/slow?ms=150")
	if d := time.Since(t0); d < 150*time.Millisecond {
		t.Fatalf("/slow?ms=150 returned after %v", d)
	}
}

func TestEchoContentLength(t *testing.T) {
	s, base, c := start(t, Config{})
	payload := make([]byte, 1<<20)
	rand.Read(payload)
	resp, err := c.Post(base+"/echo", "application/x-test", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo body differs: got %d bytes, want %d", len(got), len(payload))
	}
	if resp.ContentLength != int64(len(payload)) {
		t.Fatalf("response Content-Length = %d, want %d", resp.ContentLength, len(payload))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-test" {
		t.Fatalf("Content-Type = %q", ct)
	}
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].BodyBytes != int64(len(payload)) || reqs[0].ContentLength != int64(len(payload)) {
		t.Fatalf("request log = %+v", reqs)
	}
}

func TestEchoChunked(t *testing.T) {
	s, base, c := start(t, Config{})
	payload := bytes.Repeat([]byte("chunk-"), 50_000)
	// An io.Reader with no known length makes the client send chunked.
	req, err := http.NewRequest(http.MethodPost, base+"/echo", io.MultiReader(bytes.NewReader(payload)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo body differs: got %d bytes, want %d", len(got), len(payload))
	}
	if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("response Transfer-Encoding = %v, want [chunked]", resp.TransferEncoding)
	}
	reqs := s.Requests()
	if len(reqs) != 1 || len(reqs[0].TransferEncoding) != 1 || reqs[0].TransferEncoding[0] != "chunked" {
		t.Fatalf("request log = %+v", reqs)
	}
}

func TestBytes(t *testing.T) {
	_, base, c := start(t, Config{})
	for _, tc := range []struct {
		query   string
		n       int64
		chunked bool
	}{
		{"n=0", 0, false},
		{"n=10", 10, false},
		{"n=200000", 200000, false},
		{"n=10&chunked=1", 10, true},
		{"n=200000&chunked=1", 200000, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			resp, body := get(t, c, base+"/bytes?"+tc.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			want, _ := io.ReadAll(Pattern(tc.n))
			if body != string(want) {
				t.Fatalf("body differs from Pattern(%d): got %d bytes", tc.n, len(body))
			}
			isChunked := len(resp.TransferEncoding) == 1 && resp.TransferEncoding[0] == "chunked"
			if isChunked != tc.chunked {
				t.Fatalf("Transfer-Encoding %v, want chunked=%v", resp.TransferEncoding, tc.chunked)
			}
			if !tc.chunked && resp.ContentLength != tc.n {
				t.Fatalf("Content-Length %d, want %d", resp.ContentLength, tc.n)
			}
		})
	}
	for _, q := range []string{"", "n=-1", "n=x"} {
		if resp, _ := get(t, c, base+"/bytes?"+q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestPatternIsStable(t *testing.T) {
	a, _ := io.ReadAll(Pattern(200000))
	// Read the same prefix in odd-sized pieces.
	var b []byte
	r := Pattern(200000)
	buf := make([]byte, 7777)
	for {
		n, err := r.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(a, b) || len(a) != 200000 {
		t.Fatal("Pattern differs between read sizes")
	}
	if !bytes.Equal(a[:65521], a[65521:2*65521]) {
		t.Fatal("Pattern does not repeat its block")
	}
}

func TestDelay(t *testing.T) {
	_, base, c := start(t, Config{Delay: 100 * time.Millisecond})
	t0 := time.Now()
	get(t, c, base+"/")
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("response after %v, want >= 100ms", d)
	}
}

func TestFailRate(t *testing.T) {
	t.Run("always", func(t *testing.T) {
		_, base, c := start(t, Config{FailRate: 1})
		for _, p := range []string{"/", "/healthz", "/echo"} {
			resp, _ := get(t, c, base+p)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("%s: status = %d, want 500", p, resp.StatusCode)
			}
			if resp.Header.Get(HeaderBackend) != "b1" {
				t.Errorf("%s: missing %s on failure", p, HeaderBackend)
			}
		}
	})
	t.Run("never", func(t *testing.T) {
		_, base, c := start(t, Config{FailRate: 0})
		for range 20 {
			if resp, _ := get(t, c, base+"/"); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
		}
	})
	t.Run("fraction", func(t *testing.T) {
		// A deterministic sequence: every other draw is below the rate.
		draws := []float64{0.1, 0.9}
		i := 0
		_, base, c := start(t, Config{FailRate: 0.5, Rand: func() float64 {
			v := draws[i%len(draws)]
			i++
			return v
		}})
		var fails int
		for range 10 {
			if resp, _ := get(t, c, base+"/"); resp.StatusCode == http.StatusInternalServerError {
				fails++
			}
		}
		if fails != 5 {
			t.Fatalf("fails = %d of 10, want 5", fails)
		}
	})
}

func TestRequestLog(t *testing.T) {
	s, base, c := start(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, base+"/path?q=1", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Host = "a.test"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	reqs := s.Requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d records, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != "GET" || r.Path != "/path" || r.RawQuery != "q=1" || r.Host != "a.test" ||
		r.Status != 200 || r.Header.Get("X-Forwarded-For") != "203.0.113.7" {
		t.Fatalf("record = %+v", r)
	}
	s.ResetRequests()
	if n := len(s.Requests()); n != 0 {
		t.Fatalf("after reset: %d records", n)
	}
}

// TestShutdownInterruptsSlow checks that Shutdown does not wait out a long
// /slow request: the handler's context is cancelled and it returns at once.
func TestShutdownInterruptsSlow(t *testing.T) {
	s, err := New(Config{Name: "b1", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := c.Get("http://" + ln.Addr().String() + "/slow?ms=60000")
		if err == nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, func() bool { return s.InFlight() == 1 })

	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	<-done
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Status != StatusClientGone {
		t.Fatalf("request log = %+v, want one record with status %d", reqs, StatusClientGone)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

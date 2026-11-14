package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// fastOptions checks every 10 ms, so state changes take tens of
// milliseconds.
func fastOptions() Options {
	return Options{
		Path:            "/healthz",
		Interval:        10 * time.Millisecond,
		Timeout:         200 * time.Millisecond,
		Rise:            2,
		Fall:            3,
		PassiveFailures: 3,
		Eject:           100 * time.Millisecond,
	}
}

// backend is an httptest server whose /healthz answers with status, which
// the test may change.
type backend struct {
	*httptest.Server
	status atomic.Int32
	checks atomic.Int64
	agent  atomic.Value
}

func newBackend(t *testing.T) *backend {
	b := &backend{}
	b.status.Store(http.StatusOK)
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		b.checks.Add(1)
		b.agent.Store(r.UserAgent())
		w.WriteHeader(int(b.status.Load()))
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *backend) addr() string { return strings.TrimPrefix(b.URL, "http://") }

func newChecker(t *testing.T, o Options) *Checker {
	c := NewChecker(Config{Options: o})
	t.Cleanup(c.Close)
	return c
}

// waitState waits up to d for t to reach want.
func waitState(t *testing.T, tg *Target, want State, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for tg.State() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: state %v after %v, want %v (status %+v)", tg.Addr(), tg.State(), d, want, tg.Status())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNilTargetIsHealthy(t *testing.T) {
	var tg *Target
	tg.ReportFailure()
	tg.ReportSuccess()
	if !tg.Healthy() || tg.State() != Healthy || tg.Transitions(Unhealthy) != 0 {
		t.Fatal("nil target not healthy")
	}
	var c *Checker
	if c.Gen() != 0 {
		t.Fatal("nil checker generation")
	}
}

func TestActiveFallAndRise(t *testing.T) {
	b := newBackend(t)
	c := newChecker(t, fastOptions())
	tg := c.Target(b.addr())
	if tg != c.Target(b.addr()) {
		t.Fatal("second Target call made a new target")
	}
	// The first check runs at once.
	deadline := time.Now().Add(time.Second)
	for b.checks.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no check within a second")
		}
		time.Sleep(time.Millisecond)
	}
	if got := b.agent.Load(); got != userAgent {
		t.Errorf("User-Agent %q, want %q", got, userAgent)
	}
	if !tg.Healthy() {
		t.Fatalf("not healthy: %+v", tg.Status())
	}

	gen := c.Gen()
	b.status.Store(http.StatusInternalServerError)
	waitState(t, tg, Unhealthy, time.Second)
	if c.Gen() == gen {
		t.Fatal("generation did not change")
	}
	st := tg.Status()
	if st.LastCheck == nil || st.LastCheck.OK || st.LastCheck.Status != 500 || st.LastCheck.Error != "status 500" {
		t.Fatalf("last check %+v", st.LastCheck)
	}
	if st.ConsecutiveFailures < 3 || st.ConsecutiveSuccesses != 0 {
		t.Fatalf("runs %+v", st)
	}

	b.status.Store(http.StatusNoContent)
	waitState(t, tg, Healthy, time.Second)
	if st := tg.Status(); st.ConsecutiveSuccesses < 2 || st.Transitions["unhealthy"] != 1 || st.Transitions["healthy"] != 1 {
		t.Fatalf("status %+v", st)
	}
}

// TestActiveRedirectIsSuccess: a 3xx counts as an answer and is not
// followed.
func TestActiveRedirectIsSuccess(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := newChecker(t, fastOptions())
	tg := c.Target(strings.TrimPrefix(srv.URL, "http://"))
	time.Sleep(100 * time.Millisecond)
	if !tg.Healthy() || followed.Load() {
		t.Fatalf("healthy %v, followed %v", tg.Healthy(), followed.Load())
	}
}

func TestActiveRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c := newChecker(t, fastOptions())
	tg := c.Target(addr)
	waitState(t, tg, Unhealthy, time.Second)
	if st := tg.Status(); st.LastCheck == nil || st.LastCheck.Status != 0 || !strings.Contains(st.LastCheck.Error, "refused") {
		t.Fatalf("last check %+v", st.LastCheck)
	}
}

// TestActiveTimeout: a backend that accepts and never answers fails its
// checks by timeout.
func TestActiveTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	o := fastOptions()
	o.Timeout = 30 * time.Millisecond
	c := newChecker(t, o)
	tg := c.Target(strings.TrimPrefix(srv.URL, "http://"))
	waitState(t, tg, Unhealthy, 2*time.Second)
	if st := tg.Status(); st.LastCheck == nil || !strings.Contains(st.LastCheck.Error, "deadline exceeded") {
		t.Fatalf("last check %+v", st.LastCheck)
	}
}

func TestPassiveEjectThenActiveReadmits(t *testing.T) {
	b := newBackend(t)
	var ejected sync.Map
	o := fastOptions()
	c := NewChecker(Config{Options: o, OnEject: func(a string) { ejected.Store(a, true) }})
	t.Cleanup(c.Close)
	tg := c.Target(b.addr())

	// A success between failures ends the run.
	tg.ReportFailure()
	tg.ReportFailure()
	tg.ReportSuccess()
	tg.ReportFailure()
	tg.ReportFailure()
	if !tg.Healthy() {
		t.Fatal("ejected without a full run of failures")
	}
	gen := c.Gen()
	tg.ReportFailure()
	// Ejection is immediate, made by the reporting goroutine.
	if tg.State() != Ejected || c.Gen() == gen {
		t.Fatalf("state %v, generation moved %v", tg.State(), c.Gen() != gen)
	}
	// More failures while ejected change nothing.
	tg.ReportFailure()
	if tg.Transitions(Ejected) != 1 {
		t.Fatalf("%d ejections", tg.Transitions(Ejected))
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := ejected.Load(b.addr()); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OnEject not called")
		}
		time.Sleep(time.Millisecond)
	}
	if st := tg.Status(); st.EjectedUntil == nil || st.State != "ejected" {
		t.Fatalf("status %+v", st)
	}

	// The backoff ends, the backend is unhealthy until rise checks pass,
	// then healthy.
	waitState(t, tg, Unhealthy, time.Second)
	waitState(t, tg, Healthy, time.Second)
	if st := tg.Status(); st.EjectedUntil != nil || st.PassiveFailures != 0 || st.Transitions["unhealthy"] != 1 {
		t.Fatalf("status %+v", st)
	}
}

// TestEjectionStaysWhileBackendFails: after the backoff, a backend that
// still fails its checks stays out.
func TestEjectionStaysWhileBackendFails(t *testing.T) {
	b := newBackend(t)
	b.status.Store(http.StatusServiceUnavailable)
	o := fastOptions()
	o.Fall = 100 // active checks alone would take a second to eject it
	c := newChecker(t, o)
	tg := c.Target(b.addr())
	for range 3 {
		tg.ReportFailure()
	}
	waitState(t, tg, Unhealthy, time.Second)
	time.Sleep(100 * time.Millisecond)
	if tg.State() != Unhealthy {
		t.Fatalf("state %v, want unhealthy while checks fail", tg.State())
	}
}

func TestPassiveWithoutActiveChecks(t *testing.T) {
	o := fastOptions()
	o.Interval = 0
	c := newChecker(t, o)
	tg := c.Target("127.0.0.1:1") // never dialled
	for range 3 {
		tg.ReportFailure()
	}
	if tg.State() != Ejected {
		t.Fatalf("state %v", tg.State())
	}
	// With nothing to re-admit it, the backoff's end does.
	waitState(t, tg, Healthy, time.Second)
	if tg.Status().LastCheck != nil {
		t.Fatal("checked with active checks off")
	}
}

func TestPassiveOff(t *testing.T) {
	o := fastOptions()
	o.Interval, o.PassiveFailures = 0, 0
	c := newChecker(t, o)
	tg := c.Target("127.0.0.1:1")
	for range 100 {
		tg.ReportFailure()
	}
	if !tg.Healthy() {
		t.Fatal("ejected with passive checks off")
	}
}

// TestSyncStopsRemovedProber is the "Done when" check: removing a backend
// stops its prober goroutine, and the others keep running. Each prober
// checks once at start and then not for an hour, so no check is in flight
// when goleak looks.
func TestSyncStopsRemovedProber(t *testing.T) {
	a, b, x := newBackend(t), newBackend(t), newBackend(t)
	o := fastOptions()
	o.Interval, o.Eject = time.Hour, time.Hour
	c := newChecker(t, o)
	c.Sync([]string{a.addr(), b.addr()})
	waitChecks(t, a, 1)
	waitChecks(t, b, 1)
	// The goroutines running now, the probers of a and b among them, are
	// the baseline.
	base := goleak.IgnoreCurrent()

	c.Sync([]string{a.addr(), b.addr(), x.addr()})
	tx := c.Target(x.addr())
	waitChecks(t, x, 1)
	if len(c.Targets()) != 3 {
		t.Fatalf("%d targets", len(c.Targets()))
	}
	c.Sync([]string{a.addr(), b.addr()})
	goleak.VerifyNone(t, base)

	got := c.Targets()
	if len(got) != 2 || got[0].Addr() > got[1].Addr() {
		t.Fatalf("targets %v %v", got[0].Addr(), got[1].Addr())
	}
	// a's prober still runs: it handles an ejection.
	ta := c.Target(a.addr())
	for range 3 {
		ta.ReportFailure()
	}
	deadline := time.Now().Add(time.Second)
	for ta.Status().EjectedUntil == nil {
		if time.Now().After(deadline) {
			t.Fatal("remaining prober did not handle the ejection")
		}
		time.Sleep(time.Millisecond)
	}
	if c.Target(x.addr()) == tx {
		t.Fatal("re-adding returned the removed target")
	}
}

func waitChecks(t *testing.T, b *backend, n int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for b.checks.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d checks, want %d", b.checks.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTargetAfterClose(t *testing.T) {
	c := NewChecker(Config{Options: fastOptions()})
	c.Close()
	tg := c.Target("127.0.0.1:1")
	if !tg.Healthy() {
		t.Fatal("target after close not healthy")
	}
	c.Sync(nil) // stops nothing that is running
	c.Close()
}

func TestNextProbeJitter(t *testing.T) {
	c := NewChecker(Config{Options: Options{Interval: time.Second}})
	defer c.Close()
	tg := newTarget(c, "x:1")
	seen := map[time.Duration]bool{}
	for range 100 {
		d := tg.nextProbe()
		if d < 900*time.Millisecond || d > 1100*time.Millisecond {
			t.Fatalf("next probe in %v, want within 10%% of a second", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct waits", len(seen))
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{Healthy: "healthy", Unhealthy: "unhealthy", Ejected: "ejected", 7: "State(7)"} {
		if s.String() != want {
			t.Errorf("%d: %q", s, s.String())
		}
	}
}

// Probers also stop when the checker's parent context ends; Close
// afterwards returns at once.
func TestCheckerContext(t *testing.T) {
	a := newBackend(t)
	o := fastOptions()
	o.Interval, o.Eject = time.Hour, time.Hour
	base := goleak.IgnoreCurrent()
	ctx, cancel := context.WithCancel(context.Background())
	c := NewCheckerContext(ctx, Config{Options: o})
	tg := c.Target(a.addr())
	waitChecks(t, a, 1)
	cancel()
	select {
	case <-tg.done:
	case <-time.After(2 * time.Second):
		t.Fatal("prober still running after the context ended")
	}
	c.Close()
	goleak.VerifyNone(t, base)
}

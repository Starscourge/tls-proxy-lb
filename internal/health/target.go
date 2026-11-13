package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

// State is a backend's health.
type State int32

// The states. The zero value is Healthy, so a backend starts in rotation.
const (
	Healthy State = iota
	Unhealthy
	Ejected
	numStates
)

// States lists every state, for metrics.
var States = []State{Healthy, Unhealthy, Ejected}

func (s State) String() string {
	switch s {
	case Healthy:
		return "healthy"
	case Unhealthy:
		return "unhealthy"
	case Ejected:
		return "ejected"
	}
	return fmt.Sprintf("State(%d)", int32(s))
}

// maxProbeBody is how much of a check response is read before the
// connection is closed.
const maxProbeBody = 4 << 10

// Check is the result of one active check.
type Check struct {
	Time time.Time `json:"time"`
	OK   bool      `json:"ok"`
	// Status is the response status, or 0 if no response arrived.
	Status     int     `json:"status,omitempty"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// Status is a target's health as reported by the admin /status page.
type Status struct {
	Address string `json:"address"`
	State   string `json:"state"`
	// Since is when the target entered its current state, as the prober
	// saw it.
	Since time.Time `json:"since"`
	// EjectedUntil is when the ejection backoff ends, while ejected.
	EjectedUntil *time.Time `json:"ejected_until,omitempty"`
	// ConsecutiveSuccesses and ConsecutiveFailures are the current runs of
	// active check results.
	ConsecutiveSuccesses int `json:"consecutive_successes"`
	ConsecutiveFailures  int `json:"consecutive_failures"`
	// PassiveFailures is the current run of failures in live traffic.
	PassiveFailures int64 `json:"passive_failures"`
	// Transitions counts changes into each state since startup.
	Transitions map[string]uint64 `json:"transitions"`
	LastCheck   *Check            `json:"last_check,omitempty"`
}

// Target is the health of one backend address. Its state is read by every
// pick and written by its prober and, for ejection, by request goroutines.
// Methods on a nil Target report a healthy backend and do nothing else, so
// a backend without health checks is always in rotation.
type Target struct {
	addr string
	url  string
	c    *Checker

	state   atomic.Int32
	passive atomic.Int64
	// ejectReq wakes the prober after a request goroutine ejects the
	// target. One pending wake is enough, so sends never block.
	ejectReq    chan struct{}
	transitions [numStates]atomic.Uint64
	// info is what the prober last published for Status.
	info atomic.Pointer[probeInfo]

	cancel context.CancelFunc // stops the prober
	done   chan struct{}      // closed when the prober returns
}

// probeInfo is the prober's view of a target, published whole after each
// change so Status reads a consistent copy without a lock.
type probeInfo struct {
	since          time.Time
	ejectedUntil   time.Time
	okRun, failRun int
	last           *Check
}

func newTarget(c *Checker, addr string) *Target {
	t := &Target{
		addr:     addr,
		url:      "http://" + addr + c.opts.Path,
		c:        c,
		ejectReq: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	t.info.Store(&probeInfo{since: time.Now()})
	return t
}

// Addr returns the backend address.
func (t *Target) Addr() string { return t.addr }

// State returns the current state.
func (t *Target) State() State {
	if t == nil {
		return Healthy
	}
	return State(t.state.Load())
}

// Healthy reports whether balancers may pick the backend.
func (t *Target) Healthy() bool { return t.State() == Healthy }

// Transitions returns how many times the target has entered state s.
func (t *Target) Transitions(s State) uint64 {
	if t == nil || s < 0 || s >= numStates {
		return 0
	}
	return t.transitions[s].Load()
}

// ReportSuccess records a request the backend answered with a status below
// 500, ending any run of passive failures.
func (t *Target) ReportSuccess() {
	// Load first: the common case writes nothing, so busy backends do not
	// bounce the counter's cache line between cores.
	if t != nil && t.passive.Load() != 0 {
		t.passive.Store(0)
	}
}

// ReportFailure records a request that failed at the backend: a dial
// failure, a timeout, a reset or close before the response, or a 5xx. The
// failure that completes a run of PassiveFailures ejects a healthy
// backend.
func (t *Target) ReportFailure() {
	if t == nil {
		return
	}
	threshold := int64(t.c.opts.PassiveFailures)
	if threshold <= 0 || t.passive.Add(1) < threshold {
		return
	}
	if t.state.CompareAndSwap(int32(Healthy), int32(Ejected)) {
		t.c.changed(t, Ejected)
		select {
		case t.ejectReq <- struct{}{}:
		default:
		}
	}
}

// Status returns the target's health for the admin /status page.
func (t *Target) Status() Status {
	info := t.info.Load()
	s := Status{
		Address:              t.addr,
		State:                t.State().String(),
		Since:                info.since,
		ConsecutiveSuccesses: info.okRun,
		ConsecutiveFailures:  info.failRun,
		PassiveFailures:      t.passive.Load(),
		Transitions:          make(map[string]uint64, numStates),
		LastCheck:            info.last,
	}
	if !info.ejectedUntil.IsZero() {
		until := info.ejectedUntil
		s.EjectedUntil = &until
	}
	for _, st := range States {
		s.Transitions[st.String()] = t.transitions[st].Load()
	}
	return s
}

// stop ends the prober and waits for it.
func (t *Target) stop() {
	if t.cancel != nil {
		t.cancel()
	}
	<-t.done
}

// run is the prober goroutine. It owns the Machine and every state change
// but the one into Ejected.
func (t *Target) run(ctx context.Context) {
	defer close(t.done)
	o := t.c.opts
	active := o.Interval > 0
	m := NewMachine(o.Rise, o.Fall, true)
	info := *t.info.Load()
	publish := func() {
		info.okRun, info.failRun = m.Runs()
		cp := info
		t.info.Store(&cp)
	}

	var probeC <-chan time.Time
	var probeTimer *time.Timer
	if active {
		// The first check runs at once; later ones are spread by jitter,
		// so probers started together do not stay in step.
		probeTimer = time.NewTimer(0)
		defer probeTimer.Stop()
		probeC = probeTimer.C
	}
	var ejectC <-chan time.Time
	ejectTimer := time.NewTimer(time.Hour)
	ejectTimer.Stop()
	defer ejectTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-t.ejectReq:
			if t.State() != Ejected {
				continue
			}
			now := time.Now()
			info.since, info.ejectedUntil = now, now.Add(o.Eject)
			m.Reset(false)
			t.passive.Store(0)
			ejectTimer.Reset(o.Eject)
			ejectC = ejectTimer.C
			publish()
			t.c.log.Warn("backend ejected", "backend", t.addr, "reason", "passive failures",
				"failures", o.PassiveFailures, "eject_ms", o.Eject.Milliseconds())
			if t.c.onEject != nil {
				t.c.onEject(t.addr)
			}

		case <-ejectC:
			ejectC = nil
			to := Healthy
			if active {
				// Active checks decide when it returns; start them now.
				to = Unhealthy
				probeTimer.Reset(0)
			}
			t.passive.Store(0)
			t.state.Store(int32(to))
			t.c.changed(t, to)
			info.since, info.ejectedUntil = time.Now(), time.Time{}
			publish()
			t.c.log.Info("backend ejection ended", "backend", t.addr, "state", to.String())

		case <-probeC:
			if t.State() == Ejected {
				probeTimer.Reset(t.nextProbe())
				continue
			}
			chk := t.probe(ctx)
			if ctx.Err() != nil {
				return
			}
			info.last = &chk
			if healthy, changed := m.Observe(chk.OK); changed {
				from, to := Healthy, Unhealthy
				if healthy {
					from, to = Unhealthy, Healthy
				}
				// A request goroutine may have ejected a healthy target
				// during the check; then the CAS fails and the pending
				// ejection wins.
				if t.state.CompareAndSwap(int32(from), int32(to)) {
					if to == Healthy {
						t.passive.Store(0)
					}
					t.c.changed(t, to)
					info.since = time.Now()
					if to == Healthy {
						t.c.log.Info("backend healthy", "backend", t.addr, "successes", o.Rise)
					} else {
						t.c.log.Warn("backend unhealthy", "backend", t.addr, "failures", o.Fall, "error", chk.Error)
					}
				}
			}
			publish()
			probeTimer.Reset(t.nextProbe())
		}
	}
}

// nextProbe returns the wait before the next check: the interval, give or
// take a tenth.
func (t *Target) nextProbe() time.Duration {
	iv := t.c.opts.Interval
	spread := iv / 5
	if spread <= 0 {
		return iv
	}
	return iv - spread/2 + rand.N(spread)
}

// probe runs one active check: GET Path, bounded by Timeout. A 2xx or 3xx
// response, read up to maxProbeBody, is a success.
func (t *Target) probe(ctx context.Context) Check {
	start := time.Now()
	chk := Check{Time: start}
	ctx, cancel := context.WithTimeout(ctx, t.c.opts.Timeout)
	defer cancel()
	err := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := t.c.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		chk.Status = resp.StatusCode
		if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody)); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}()
	chk.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		chk.Error = err.Error()
	} else {
		chk.OK = true
	}
	return chk
}

package health

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Checker.
type Options struct {
	// Path is requested from each backend by active checks.
	Path string
	// Interval is the time between active checks of one backend. Zero
	// turns active checks off.
	Interval time.Duration
	// Timeout bounds one active check: dial, request and response.
	Timeout time.Duration
	// Rise and Fall are the runs of active check successes and failures
	// that make a backend healthy and unhealthy.
	Rise, Fall int
	// PassiveFailures is the run of failures in live traffic that ejects a
	// backend. Zero turns passive ejection off.
	PassiveFailures int
	// Eject is how long an ejected backend stays out before active checks
	// may re-admit it.
	Eject time.Duration
}

// DefaultOptions returns the settings used when the config sets none.
func DefaultOptions() Options {
	return Options{
		Path:            "/healthz",
		Interval:        2 * time.Second,
		Timeout:         time.Second,
		Rise:            2,
		Fall:            3,
		PassiveFailures: 3,
		Eject:           10 * time.Second,
	}
}

// Config is what NewChecker takes.
type Config struct {
	Options
	// Logger receives one line per state change. Nil discards.
	Logger *slog.Logger
	// OnEject, if set, is called from the prober goroutine when a backend
	// is ejected, with its address. The proxy uses it to close the
	// backend's idle connections.
	OnEject func(addr string)
}

// userAgent is sent with active checks, so backends can tell them apart.
const userAgent = "tls-proxy-lb-health"

// Checker owns the health state of every backend address and the prober
// goroutine of each.
type Checker struct {
	opts    Options
	log     *slog.Logger
	onEject func(string)
	client  *http.Client
	gen     atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	targets map[string]*Target
	closed  bool
}

// NewChecker returns a checker with no targets. Close must be called to
// stop the probers that Target and Sync start.
func NewChecker(cfg Config) *Checker {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Checker{
		opts:    cfg.Options,
		log:     log,
		onEject: cfg.OnEject,
		// Each check opens its own connection and closes it after the
		// response, so checks exercise the dial and accept path a new
		// client would, and no idle probe connection outlives a check.
		// Redirects count as answers, not as something to follow.
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:       (&net.Dialer{}).DialContext,
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ctx:     ctx,
		cancel:  cancel,
		targets: make(map[string]*Target),
	}
}

// Options returns the checker's settings.
func (c *Checker) Options() Options { return c.opts }

// Gen returns a number that changes whenever any target changes state. A
// nil checker's generation is always zero.
func (c *Checker) Gen() uint64 {
	if c == nil {
		return 0
	}
	return c.gen.Load()
}

// Target returns the target for addr, creating it and starting its prober
// on first use. It is called while building a config snapshot, not per
// request. A new target starts healthy and is checked at once.
func (c *Checker) Target(addr string) *Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.targets[addr]; ok {
		return t
	}
	t := newTarget(c, addr)
	c.targets[addr] = t
	if c.closed {
		close(t.done)
		return t
	}
	ctx, cancel := context.WithCancel(c.ctx)
	t.cancel = cancel
	go t.run(ctx)
	return t
}

// Sync makes addrs the set of checked addresses: targets for new addresses
// are created, and targets for addresses not in addrs are removed, their
// probers stopped. It returns once every removed prober has returned.
func (c *Checker) Sync(addrs []string) {
	for _, a := range addrs {
		c.Target(a)
	}
	c.mu.Lock()
	var removed []*Target
	for a, t := range c.targets {
		if !slices.Contains(addrs, a) {
			removed = append(removed, t)
			delete(c.targets, a)
		}
	}
	c.mu.Unlock()
	for _, t := range removed {
		t.stop()
	}
}

// Targets returns every target, sorted by address.
func (c *Checker) Targets() []*Target {
	c.mu.Lock()
	out := make([]*Target, 0, len(c.targets))
	for _, t := range c.targets {
		out = append(out, t)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b *Target) int { return strings.Compare(a.addr, b.addr) })
	return out
}

// Close stops every prober and waits for them to return. Targets keep
// their last state.
func (c *Checker) Close() {
	c.mu.Lock()
	c.closed = true
	targets := make([]*Target, 0, len(c.targets))
	for _, t := range c.targets {
		targets = append(targets, t)
	}
	c.mu.Unlock()
	c.cancel()
	for _, t := range targets {
		<-t.done
	}
	c.client.CloseIdleConnections()
}

// changed records a state change: one more transition into to, and a new
// generation so pools rebuild their healthy sets.
func (c *Checker) changed(t *Target, to State) {
	t.transitions[to].Add(1)
	c.gen.Add(1)
}

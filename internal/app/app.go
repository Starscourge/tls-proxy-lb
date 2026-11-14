// Package app wires the proxy together and runs it: the process-wide
// runtime (upstream pools, health checker, rate-limit table, metrics), the
// connection server, the config snapshot store, the listeners and the admin
// server. cmd/proxy drives it from signals: SIGHUP calls Reload, SIGTERM
// calls Shutdown.
//
// Goroutines, and what stops each:
//
//   - one accept loop per listener: Server.CloseListener when a reload
//     removes the listener, or Shutdown;
//   - one per client connection: its handler returning, the drain, or the
//     hard close when the drain runs out of time (conn.Server.Shutdown);
//   - the admin server's Serve: its Shutdown, after the drain;
//   - the health probers, the upstream reaper and the rate-limit sweeper:
//     the root context, cancelled by Shutdown, whose Close calls then wait
//     for them.
//
// Reload and Shutdown are called from one goroutine (the signal loop); a
// mutex makes concurrent calls safe anyway, one at a time.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/admin"
	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/conn"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// adminShutdownTimeout bounds how long a profile in progress may hold up
// shutdown.
const adminShutdownTimeout = 2 * time.Second

// Options are the settings that do not come from the config file.
type Options struct {
	// Logger receives the proxy's log. Nil discards.
	Logger *slog.Logger
	// Version is reported in the startup log line.
	Version string
}

// ErrClosed is returned by Reload once Shutdown has started.
var ErrClosed = errors.New("app: shutting down")

// App is one running proxy.
type App struct {
	log     *slog.Logger
	version string

	root   context.Context
	cancel context.CancelFunc

	reg   *upstream.Registry
	chk   *health.Checker
	tab   *ratelimit.Table
	m     *metrics.Metrics
	srv   *conn.Server
	store *config.Store
	// https and plain are the handlers of every HTTPS and plain HTTP
	// listener, for the life of the process: https holds the one
	// tls.Config, whose session ticket keys survive reloads.
	https, plain *conn.Proxy

	adm    *admin.Server
	errc   chan error
	served sync.WaitGroup

	// mu serialises Reload and Shutdown and guards what they change.
	mu        sync.Mutex
	cfg       *config.Config
	listeners []*listener
	closing   bool
}

// listener is one open listening socket and the handler its connections
// get, which a reload may swap.
type listener struct {
	key  listenerKey
	conf config.Listener
	ln   net.Listener
	h    *handlerSlot
}

// listenerKey identifies a listener across reloads: its configured
// address, and how many earlier listeners in the file have the same one
// (only port 0 may repeat). A reload keeps the socket of a listener whose
// key it still has, even if its mode changes.
type listenerKey struct {
	addr string
	n    int
}

func keys(ls []config.Listener) []listenerKey {
	seen := make(map[string]int, len(ls))
	out := make([]listenerKey, len(ls))
	for i, l := range ls {
		out[i] = listenerKey{l.Address, seen[l.Address]}
		seen[l.Address]++
	}
	return out
}

// handlerSlot is a conn.Handler that delegates to a handler a reload can
// replace. A connection keeps the handler current when it was accepted.
type handlerSlot struct{ p atomic.Pointer[handlerBox] }

type handlerBox struct{ h conn.Handler }

func (s *handlerSlot) set(h conn.Handler) { s.p.Store(&handlerBox{h}) }

func (s *handlerSlot) ServeConn(ctx context.Context, tc *conn.TrackedConn) {
	s.p.Load().h.ServeConn(ctx, tc)
}

// New builds the runtime and the first snapshot from cfg. It opens no
// sockets; Start does. If New fails, nothing is left running.
func New(cfg *config.Config, o Options) (*App, error) {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	a := &App{log: logger, version: o.Version, cfg: cfg, errc: make(chan error, 1)}
	a.root, a.cancel = context.WithCancel(context.Background())
	a.reg = upstream.NewRegistryContext(a.root, cfg.Upstream.Options())
	// An ejected backend's idle connections are likely dead; close them.
	// Lookup, not Pool: a passive ejection reported by a request on an old
	// snapshot must not bring back the pool of a removed address.
	a.chk = health.NewCheckerContext(a.root, health.Config{
		Options: cfg.Health,
		Logger:  logger,
		OnEject: func(addr string) {
			if p := a.reg.Lookup(addr); p != nil {
				p.Flush()
			}
		},
	})
	// One table for the life of the process, so a reloaded snapshot keeps
	// every client's bucket.
	a.tab = ratelimit.NewContext(a.root, ratelimit.Options{MaxEntries: cfg.RateLimit.MaxEntries, SweepInterval: cfg.RateLimit.Sweep})
	a.srv = conn.NewServer(conn.Limits{}, logger)
	a.m = metrics.New(metrics.Sources{Upstream: a.reg, Health: a.chk, ClientConns: a.srv.Tracker().Len,
		ClientConnsRejected: a.srv.Tracker().RejectedByName, RateLimit: a.tab})
	snap, err := config.NewSnapshot(cfg, a.runtime())
	if err != nil {
		a.closeRuntime()
		return nil, err
	}
	a.srv.Tracker().SetLimits(trackerLimits(snap))
	a.store = config.NewStore(snap)
	a.https = &conn.Proxy{Config: a.store, Logger: logger, Metrics: a.m,
		TLS: tlsx.ServerConfig(func() *tlsx.CertSet { return a.store.Load().Certs })}
	a.plain = &conn.Proxy{Config: a.store, Logger: logger, Metrics: a.m}
	if _, err := a.handlers(snap); err != nil {
		a.closeRuntime()
		return nil, err
	}
	return a, nil
}

func (a *App) runtime() config.Runtime {
	return config.Runtime{Conns: a.reg, Health: a.chk, Metrics: a.m, RateLimit: a.tab}
}

func trackerLimits(s *config.Snapshot) conn.Limits {
	return conn.Limits{MaxConns: s.Limits.MaxConns, MaxPerIP: s.Limits.MaxConnsPerIP, Exempt: s.TrustedProxies}
}

// closeRuntime stops the background goroutines and closes idle upstream
// connections.
func (a *App) closeRuntime() {
	a.cancel()
	a.chk.Close()
	a.reg.Close()
	a.tab.Close()
}

// handlers returns the handler for each of snap's listeners, in order.
func (a *App) handlers(snap *config.Snapshot) ([]conn.Handler, error) {
	out := make([]conn.Handler, 0, len(snap.Listeners))
	for _, l := range snap.Listeners {
		switch l.Mode {
		case config.ModeHTTPS:
			out = append(out, a.https)
		case config.ModeHTTP:
			out = append(out, a.plain)
		case config.ModeRedirect:
			r := conn.NewRedirect(a.store, a.log, l.RedirectPort)
			if err := r.Validate(); err != nil {
				return nil, fmt.Errorf("listener %s: %w", l.Address, err)
			}
			out = append(out, r)
		default:
			return nil, fmt.Errorf("listener %s: unknown mode %q", l.Address, l.Mode)
		}
	}
	return out, nil
}

// Listening is what Start reports once every listener is open.
type Listening struct {
	// Addrs are the bound proxy listener addresses, in config order.
	Addrs []string
	// Admin is the bound admin address, or "" without one.
	Admin string
}

// Start opens every listener and the admin listener and starts serving. If
// any fails to open, Start closes what it opened, stops the runtime and
// returns the error; the App must not be used afterwards. Otherwise
// Shutdown must be called.
func (a *App) Start(ctx context.Context) (Listening, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := a.store.Load()
	hs, err := a.handlers(snap)
	if err != nil {
		a.closeRuntime()
		return Listening{}, err
	}
	// Open every listener before serving any, so a bad address fails
	// startup instead of leaving a half-open proxy.
	ks := keys(snap.Listeners)
	lns, err := openListeners(ctx, snap.Listeners)
	if err != nil {
		a.closeRuntime()
		return Listening{}, err
	}
	if ad := snap.Admin; ad.Address != "" {
		a.adm, err = admin.Listen(ctx, admin.Options{
			Address:              ad.Address,
			MutexProfileFraction: ad.MutexProfileFraction,
			BlockProfileRate:     ad.BlockProfileRate,
			Metrics:              a.m.Handler(),
			Status:               admin.StatusSource{Config: a.store, Health: a.chk, Conns: a.reg},
		}, a.log)
		if err != nil {
			closeAll(lns)
			a.closeRuntime()
			return Listening{}, err
		}
	}
	for i, ln := range lns {
		l := &listener{key: ks[i], conf: snap.Listeners[i], ln: ln, h: &handlerSlot{}}
		l.h.set(hs[i])
		a.listeners = append(a.listeners, l)
	}
	out := Listening{Addrs: a.addrsLocked()}
	modes := make([]string, len(snap.Listeners))
	for i, l := range snap.Listeners {
		modes[i] = l.Mode
	}
	cfg := a.cfg
	// GOMAXPROCS follows the container's CPU limit (Go 1.25 and later), so
	// it can be checked against the limit from here.
	a.log.Info("proxy listening", "version", a.version, "addrs", out.Addrs, "modes", modes,
		"log_buffer_bytes", cfg.Log.BufferBytes,
		"routes", len(cfg.Routes), "pools", len(cfg.Pools), "default_pool", cfg.DefaultPool,
		"max_conns", snap.Limits.MaxConns, "max_conns_per_ip", snap.Limits.MaxConnsPerIP,
		"rate_limit", snap.RateLimit.Default.Rate, "rate_burst", snap.RateLimit.Default.Burst,
		"rate_limit_max_entries", a.tab.MaxEntries(), "trusted_proxies", len(snap.TrustedProxies),
		"max_idle_per_backend", snap.Upstream.MaxIdlePerBackend, "upstream_idle_ms", snap.Upstream.IdleTimeout.Milliseconds(),
		"max_retries", snap.Upstream.MaxRetries,
		"health_interval_ms", snap.Health.Interval.Milliseconds(), "health_rise", snap.Health.Rise, "health_fall", snap.Health.Fall,
		"passive_failures", snap.Health.PassiveFailures, "eject_ms", snap.Health.Eject.Milliseconds(),
		"shutdown_ms", snap.Timeouts.Shutdown.Milliseconds(),
		"gomaxprocs", runtime.GOMAXPROCS(0), "num_cpu", runtime.NumCPU())
	for _, w := range snap.Certs.Warnings(time.Now()) {
		a.log.Warn("certificate", "warning", w)
	}
	if a.adm != nil {
		out.Admin = a.adm.Addr()
		a.log.Info("admin listening", "addr", out.Admin, "paths", []string{"/metrics", "/status", "/debug/pprof/"},
			"mutex_profile_fraction", snap.Admin.MutexProfileFraction, "block_profile_rate_ns", snap.Admin.BlockProfileRate)
		a.served.Add(1)
		go func() {
			defer a.served.Done()
			if err := a.adm.Serve(); err != nil {
				a.fail(err)
			}
		}()
	}
	for _, l := range a.listeners {
		a.serve(l)
	}
	return out, nil
}

// openListeners opens a socket for each of ls, or none.
func openListeners(ctx context.Context, ls []config.Listener) ([]net.Listener, error) {
	var lc net.ListenConfig
	out := make([]net.Listener, 0, len(ls))
	for _, l := range ls {
		ln, err := lc.Listen(ctx, "tcp", l.Address)
		if err != nil {
			closeAll(out)
			return nil, fmt.Errorf("listen %s: %w", l.Address, err)
		}
		out = append(out, ln)
	}
	return out, nil
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		ln.Close()
	}
}

// serve starts l's accept loop.
func (a *App) serve(l *listener) {
	a.served.Add(1)
	go func() {
		defer a.served.Done()
		if err := a.srv.Serve(l.ln, l.h); !errors.Is(err, conn.ErrServerClosed) {
			a.fail(fmt.Errorf("serve %s: %w", l.ln.Addr(), err))
		}
	}()
}

// fail reports a listener that stopped on its own. The first such error is
// kept for Errors; the signal loop shuts down on it.
func (a *App) fail(err error) {
	select {
	case a.errc <- err:
	default:
	}
}

// Errors delivers the first error that stopped a listener or the admin
// server without being asked to.
func (a *App) Errors() <-chan error { return a.errc }

// Addrs returns the bound address of every proxy listener, in config order.
func (a *App) Addrs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addrsLocked()
}

func (a *App) addrsLocked() []string {
	out := make([]string, len(a.listeners))
	for i, l := range a.listeners {
		out[i] = l.ln.Addr().String()
	}
	return out
}

// Store returns the snapshot store.
func (a *App) Store() *config.Store { return a.store }

// Metrics returns the proxy's metrics.
func (a *App) Metrics() *metrics.Metrics { return a.m }

// Server returns the connection server.
func (a *App) Server() *conn.Server { return a.srv }

// Health returns the health checker.
func (a *App) Health() *health.Checker { return a.chk }

// Upstream returns the upstream connection registry.
func (a *App) Upstream() *upstream.Registry { return a.reg }

// Reload reads the config file at path and, if it is valid, makes it the
// config every new request uses. On any error the old config keeps
// serving, unchanged. Either way the outcome is logged and counted in
// proxy_config_reloads_total.
func (a *App) Reload(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return ErrClosed
	}
	start := time.Now()
	err := a.reload(path)
	a.m.Reload(err == nil)
	if err != nil {
		a.log.Error("config reload failed; the old config is still serving", "path", path, "err", err)
		return err
	}
	snap := a.store.Load()
	a.log.Info("config reloaded", "path", path, "addrs", a.addrsLocked(),
		"routes", len(a.cfg.Routes), "pools", len(a.cfg.Pools), "backends", len(snap.Addrs()),
		"reload_ms", float64(time.Since(start).Microseconds())/1000)
	for _, w := range snap.Certs.Warnings(time.Now()) {
		a.log.Warn("certificate", "warning", w)
	}
	return nil
}

// reload is Reload's work; a.mu is held. The steps, each of which leaves
// the old config serving if it fails:
//
//  1. Parse and validate the file.
//  2. Build a complete snapshot: pools, balancers, certificates, limiters.
//     Backends present before keep their health, idle connections and
//     in-flight counts, which belong to the address, not the snapshot.
//  3. Open the listeners the new config adds.
//
// Then the swap, which cannot fail: store the snapshot, so new requests
// use it while requests in flight finish on the old one; serve the added
// listeners, and close the removed ones, whose open connections carry on.
// Last, whatever happened, the runtime forgets the backends and routes the
// current snapshot does not have: their probers stop, their idle
// connections close and their series leave /metrics.
func (a *App) reload(path string) error {
	defer a.retain()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if kept := cfg.KeepStartup(a.cfg); len(kept) > 0 {
		a.log.Warn("config reload: settings that apply only after a restart were changed; keeping the running values",
			"settings", kept)
	}
	snap, err := config.NewSnapshot(cfg, a.runtime())
	if err != nil {
		return err
	}
	hs, err := a.handlers(snap)
	if err != nil {
		return err
	}

	ks := keys(snap.Listeners)
	old := make(map[listenerKey]*listener, len(a.listeners))
	for _, l := range a.listeners {
		old[l.key] = l
	}
	var added []config.Listener
	for i, k := range ks {
		if old[k] == nil {
			added = append(added, snap.Listeners[i])
		}
	}
	lns, err := openListeners(context.Background(), added)
	if err != nil {
		return err
	}

	a.store.Store(snap)
	a.cfg = cfg
	a.srv.Tracker().SetLimits(trackerLimits(snap))
	next := make([]*listener, len(ks))
	for i, k := range ks {
		l := old[k]
		if l == nil {
			l = &listener{key: k, ln: lns[0], h: &handlerSlot{}}
			lns = lns[1:]
			l.h.set(hs[i])
			a.serve(l)
		} else {
			delete(old, k)
			l.h.set(hs[i])
		}
		l.conf = snap.Listeners[i]
		next[i] = l
	}
	for _, l := range old {
		a.srv.CloseListener(l.ln)
		a.log.Info("listener closed by reload", "addr", l.ln.Addr().String())
	}
	a.listeners = next
	return nil
}

// retain makes the runtime match the current snapshot.
func (a *App) retain() {
	snap := a.store.Load()
	addrs := snap.Addrs()
	a.chk.Sync(addrs)
	a.reg.Retain(addrs)
	a.m.Retain(snap.Backends(), snap.Routes())
}

// Shutdown stops the proxy gracefully: it closes the listeners, so new
// connections are refused, and gives open connections the current
// config's shutdown timeout to finish (see conn.Server.Shutdown). Past it,
// what remains is closed. Then the admin server and every background
// goroutine stop. It returns context.DeadlineExceeded if connections had
// to be cut off, which is still a clean exit.
func (a *App) Shutdown() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return ErrClosed
	}
	a.closing = true
	timeout := a.store.Load().Timeouts.Shutdown
	start := time.Now()
	a.log.Info("proxy shutting down", "open_conns", a.srv.Tracker().Len(), "shutdown_ms", timeout.Milliseconds())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	drainErr := a.srv.Shutdown(ctx)
	cancel()
	if drainErr != nil {
		a.log.Warn("shutdown timeout reached; closed the remaining connections",
			"shutdown_ms", timeout.Milliseconds())
	}
	if a.adm != nil {
		actx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
		_ = a.adm.Shutdown(actx)
		cancel()
	}
	a.served.Wait()
	a.closeRuntime()
	a.log.Info("proxy stopped", "drain_ms", float64(time.Since(start).Microseconds())/1000)
	return drainErr
}

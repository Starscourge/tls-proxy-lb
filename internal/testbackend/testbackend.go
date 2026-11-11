// Package testbackend is the HTTP server that stands in for a real backend in
// tests and in the Docker lab.
//
// It is not on the proxy's request path, so it is built on net/http's
// http.Server. Every response carries the backend's name in an X-Backend
// header, and every request is logged (and optionally kept in memory) so tests
// can assert on exactly what the proxy forwarded.
//
// Endpoints:
//
//	/         200 with a short text body naming the backend (catch-all path)
//	/healthz  200 "ok"
//	/echo     200 with the request body streamed back unchanged
//	/slow     200 after sleeping ?ms=N milliseconds (default 5000)
//	/bytes    200 with ?n=N bytes of a fixed pattern (see Pattern), sent with
//	          Content-Length, or chunked with &chunked=1
//
// Config.Delay and Config.FailRate apply to every endpoint, /healthz included,
// so a backend started with FailRate 1 also fails its health checks.
package testbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// HeaderBackend is the response header that carries the backend's name.
const HeaderBackend = "X-Backend"

// StatusClientGone is recorded (never sent) for a request whose context ended
// before a response was written, as nginx does with 499.
const StatusClientGone = 499

// DefaultSlow is how long /slow sleeps when no ms query parameter is given.
const DefaultSlow = 5 * time.Second

// Config controls a test backend's behaviour.
type Config struct {
	// Name is returned in the X-Backend header. Required.
	Name string
	// Delay is added before every response.
	Delay time.Duration
	// FailRate is the probability, in [0, 1], that a request gets a 500
	// instead of its normal response.
	FailRate float64
	// Logger receives one line per request. Nil discards the log.
	Logger *slog.Logger
	// Record keeps every request in memory for Requests. Meant for in-process
	// tests; the standalone binary leaves it off so memory stays flat.
	Record bool
	// Rand returns a number in [0, 1) used for FailRate. Nil uses math/rand/v2.
	Rand func() float64
}

// RequestRecord is what the backend saw for one request.
type RequestRecord struct {
	Time             time.Time
	Method           string
	Path             string
	RawQuery         string
	Proto            string
	Host             string
	RemoteAddr       string
	Header           http.Header
	ContentLength    int64
	TransferEncoding []string
	// BodyBytes is how many request-body bytes the handler read. Only /echo
	// reads the body; other endpoints leave it at 0.
	BodyBytes int64
	Status    int
	Duration  time.Duration
}

// Server is a test backend. Create it with New, run it with Serve and stop it
// with Shutdown.
type Server struct {
	cfg  Config
	srv  *http.Server
	rand func() float64

	// ctx is the parent of every request context. Shutdown cancels it so
	// sleeping handlers return at once instead of holding shutdown open.
	ctx    context.Context
	cancel context.CancelFunc

	// handlers tracks running handlers so Shutdown can wait for them, even
	// after a forced Close. hmu and stopped make sure no handler is added once
	// Shutdown has started waiting.
	handlers sync.WaitGroup
	hmu      sync.Mutex
	stopped  bool
	inflight atomic.Int64

	mu   sync.Mutex
	reqs []RequestRecord
}

// New validates cfg and returns a Server that is not yet listening.
func New(cfg Config) (*Server, error) {
	if cfg.Name == "" {
		return nil, errors.New("testbackend: Name is required")
	}
	if cfg.Delay < 0 {
		return nil, fmt.Errorf("testbackend: Delay must be >= 0, got %v", cfg.Delay)
	}
	if !(cfg.FailRate >= 0 && cfg.FailRate <= 1) { // also rejects NaN
		return nil, fmt.Errorf("testbackend: FailRate must be in [0, 1], got %v", cfg.FailRate)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, rand: cfg.Rand}
	if s.rand == nil {
		s.rand = rand.Float64
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
		ErrorLog:          slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Name returns the backend's name.
func (s *Server) Name() string { return s.cfg.Name }

// Serve accepts connections on ln until Shutdown. It returns nil after a
// clean shutdown and the accept error otherwise. It takes ownership of ln.
func (s *Server) Serve(ln net.Listener) error {
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server: it cancels in-flight handlers, closes the
// listener and connections, and waits for every handler to return. If ctx
// expires first, connections are closed forcibly; Shutdown still waits for
// handlers, which return promptly because their context is cancelled.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	s.hmu.Lock()
	s.stopped = true
	s.hmu.Unlock()
	s.handlers.Wait()
	return err
}

// enter registers a running handler. It fails once Shutdown is waiting.
func (s *Server) enter() bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	if s.stopped {
		return false
	}
	s.handlers.Add(1)
	return true
}

// InFlight returns the number of requests currently being handled.
func (s *Server) InFlight() int64 { return s.inflight.Load() }

// Requests returns a copy of the requests recorded so far, oldest first. It
// is empty unless Config.Record is set.
func (s *Server) Requests() []RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestRecord, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// ResetRequests clears the in-memory request log.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	s.reqs = nil
	s.mu.Unlock()
}

// Handler returns the backend's handler, for use with httptest or another
// http.Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/echo", s.echo)
	mux.HandleFunc("/slow", s.slow)
	mux.HandleFunc("/bytes", s.bytes)
	mux.HandleFunc("/", s.root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.enter() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		defer s.handlers.Done()
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
		s.serve(mux, w, r)
	})
}

func (s *Server) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set(HeaderBackend, s.cfg.Name)
	rw := &recorder{ResponseWriter: w}
	body := &countingReader{r: r.Body}
	r.Body = body

	switch {
	case !sleep(r.Context(), s.cfg.Delay):
		// Client went away or server is shutting down; nobody reads a reply.
	case s.cfg.FailRate > 0 && s.rand() < s.cfg.FailRate:
		http.Error(rw, "injected failure", http.StatusInternalServerError)
	default:
		next.ServeHTTP(rw, r)
	}
	if rw.status == 0 {
		if r.Context().Err() != nil {
			rw.status = StatusClientGone
		} else {
			rw.status = http.StatusOK
		}
	}
	s.record(r, rw.status, body.n, time.Since(start))
}

func (s *Server) record(r *http.Request, status int, bodyBytes int64, d time.Duration) {
	rec := RequestRecord{
		Time:             time.Now(),
		Method:           r.Method,
		Path:             r.URL.Path,
		RawQuery:         r.URL.RawQuery,
		Proto:            r.Proto,
		Host:             r.Host,
		RemoteAddr:       r.RemoteAddr,
		Header:           r.Header.Clone(),
		ContentLength:    r.ContentLength,
		TransferEncoding: append([]string(nil), r.TransferEncoding...),
		BodyBytes:        bodyBytes,
		Status:           status,
		Duration:         d,
	}
	s.cfg.Logger.LogAttrs(context.Background(), slog.LevelInfo, "request",
		slog.String("backend", s.cfg.Name),
		slog.String("method", rec.Method),
		slog.String("path", rec.Path),
		slog.String("query", rec.RawQuery),
		slog.String("proto", rec.Proto),
		slog.String("host", rec.Host),
		slog.String("remote", rec.RemoteAddr),
		slog.String("x_forwarded_for", r.Header.Get("X-Forwarded-For")),
		slog.String("x_forwarded_proto", r.Header.Get("X-Forwarded-Proto")),
		slog.Int64("content_length", rec.ContentLength),
		slog.Any("transfer_encoding", rec.TransferEncoding),
		slog.Int64("body_bytes", rec.BodyBytes),
		slog.Int("status", rec.Status),
		slog.Float64("duration_ms", float64(rec.Duration.Microseconds())/1000),
	)
	if s.cfg.Record {
		s.mu.Lock()
		s.reqs = append(s.reqs, rec)
		s.mu.Unlock()
	}
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "hello from %s\n", s.cfg.Name)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// echo streams the request body back. A known request length is mirrored in
// Content-Length; otherwise the response is chunked.
func (s *Server) echo(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if r.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	}
	// Allow the response to start while the request body is still arriving.
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, r.Body); err != nil {
		s.cfg.Logger.Warn("echo copy failed", "backend", s.cfg.Name, "err", err)
	}
}

func (s *Server) slow(w http.ResponseWriter, r *http.Request) {
	d := DefaultSlow
	if v := r.URL.Query().Get("ms"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			http.Error(w, "ms must be a non-negative integer", http.StatusBadRequest)
			return
		}
		d = time.Duration(ms) * time.Millisecond
	}
	if !sleep(r.Context(), d) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "slow response from %s after %v\n", s.cfg.Name, d)
}

// bytes streams n bytes of Pattern. The pattern block is shared and never
// written, so a 1 GB response costs no memory.
func (s *Server) bytes(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	if err != nil || n < 0 {
		http.Error(w, "n must be a non-negative integer", http.StatusBadRequest)
		return
	}
	chunked := r.URL.Query().Get("chunked") == "1"
	w.Header().Set("Content-Type", "application/octet-stream")
	if !chunked {
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	}
	w.WriteHeader(http.StatusOK)
	if chunked {
		// Flushing before the body forces chunked framing even when the
		// whole body would fit in the server's buffer.
		_ = http.NewResponseController(w).Flush()
	}
	if _, err := io.Copy(w, Pattern(n)); err != nil {
		s.cfg.Logger.Debug("bytes copy stopped", "backend", s.cfg.Name, "err", err)
	}
}

// patternBlock is the repeating unit of Pattern: a fixed pseudo-random block
// whose length is prime, so the pattern never lines up with buffer sizes.
var patternBlock = func() []byte {
	b := make([]byte, 65521)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}()

// Pattern returns a reader of the first n bytes that /bytes?n=N serves, so
// tests can check a body byte for byte without storing it.
func Pattern(n int64) io.Reader {
	return io.LimitReader(&patternReader{}, n)
}

type patternReader struct{ off int }

func (p *patternReader) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		c := copy(b[n:], patternBlock[p.off:])
		n += c
		p.off = (p.off + c) % len(patternBlock)
	}
	return n, nil
}

// sleep waits for d or until ctx is done. It reports whether the full
// duration elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// recorder captures the status code written by a handler.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// countingReader counts body bytes read by the handler.
type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

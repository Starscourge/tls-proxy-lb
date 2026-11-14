package conn

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
	"github.com/Starscourge/tls-proxy-lb/internal/lb"
	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
	"github.com/Starscourge/tls-proxy-lb/internal/ratelimit"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

const (
	// errorWriteTimeout bounds writing a proxy-generated error response.
	errorWriteTimeout = time.Second
	// lingerTimeout and lingerMaxBytes bound the drain before closing a
	// connection that may hold unread request bytes. Closing with unread
	// data sends a reset, which can destroy a response the client has not
	// read yet.
	lingerTimeout  = 500 * time.Millisecond
	lingerMaxBytes = 256 << 10
	// writerGrace bounds the wait, once a response is complete, for a
	// request writer that is still running (bodyWriter.finished).
	writerGrace = 50 * time.Millisecond
	// maxKeptRaw is the largest header-block buffer a connection keeps
	// between requests.
	maxKeptRaw = 16 << 10
)

// StatusClientClosed is logged (never sent) when the client goes away before
// a response, as nginx does.
const StatusClientClosed = 499

// Proxy routes each request on a client connection by its Host header to a
// pool, picks a healthy backend with the pool's balancer and forwards the
// request over a kept-alive or new upstream connection, trying another
// backend if the first fails before answering. It implements Handler.
//
// With TLS set, each connection starts with a TLS handshake, and every
// request on it must name the host its SNI named (421 otherwise).
type Proxy struct {
	// Config holds the current config snapshot. Each request loads it once
	// and uses that snapshot to the end.
	Config *config.Store
	// Logger receives one "access" line per request plus errors. Nil
	// discards.
	Logger *slog.Logger
	// TLS, if non-nil, makes the proxy terminate TLS (tlsx.ServerConfig).
	// Nil serves plain HTTP.
	TLS *tls.Config
	// Metrics, if set, counts requests that reach no pool. Requests that
	// do are counted in their pool's and backend's stats.
	Metrics *metrics.Metrics

	// afterRequestWrite, if set, runs in the request writer goroutine after
	// the request is sent and before the writer reports done. Tests use it
	// to hold the writer back past the response.
	afterRequestWrite func()
	// onRequest, if set, is called with each request the proxy accepts,
	// before it is routed. Tests use it to panic on purpose.
	onRequest func(*http.Request)
}

// Validate checks the proxy's settings.
func (p *Proxy) Validate() error {
	if p.Config == nil || p.Config.Load() == nil {
		return errors.New("proxy: Config must hold a snapshot")
	}
	return nil
}

// ServeConn runs the keep-alive loop for one client connection: read a
// request, proxy it, write the response, repeat until either side closes,
// a deadline passes or an error makes the framing unsafe to continue.
func (p *Proxy) ServeConn(ctx context.Context, tc *TrackedConn) {
	snap := p.Config.Load()
	cc := newClientConn(p, tc, snap)
	defer cc.release()
	if p.TLS != nil {
		tlsConn, ok := cc.handshake(ctx)
		if !ok {
			return
		}
		// Close sends close_notify, so a client reading a close-delimited
		// body can tell its end from a truncation. crypto/tls bounds that
		// write at 5 seconds; the Server then closes the socket.
		defer tlsConn.Close()
	}
	for first := true; cc.serveRequest(ctx, first); first = false {
	}
	if cc.linger {
		cc.lingeringClose()
	}
}

// handshake runs the TLS handshake as its own step, bounded by the
// Handshake timeout, and switches the connection's I/O to the TLS
// connection. A failed handshake is logged and ends the connection.
func (cc *clientConn) handshake(ctx context.Context) (*tls.Conn, bool) {
	start := time.Now()
	deadline := start.Add(cc.t.Handshake)
	// The deadline bounds every read and write of the handshake; the
	// context also ends it if the server closes first.
	if err := cc.tc.SetDeadline(deadline); err != nil {
		return nil, false
	}
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	tlsConn := tls.Server(cc.tc, cc.p.TLS)
	err := tlsConn.HandshakeContext(hctx)
	st := tlsConn.ConnectionState()
	if err != nil {
		cc.log.LogAttrs(context.Background(), slog.LevelInfo, "tls handshake failed",
			slog.String("client", cc.ip),
			slog.String("sni", st.ServerName),
			slog.Bool("timeout", isTimeout(err) || errors.Is(err, context.DeadlineExceeded)),
			slog.Float64("handshake_ms", ms(time.Since(start))),
			slog.String("error", err.Error()))
		return nil, false
	}
	if err := cc.tc.SetDeadline(time.Time{}); err != nil {
		return nil, false
	}
	cc.c = tlsConn
	cc.lr.R = tlsConn
	cc.dw.Conn = tlsConn
	cc.scheme = "https"
	cc.sni = tlsx.NormaliseName(st.ServerName)
	cc.tlsInfo = tlsInfo{version: tls.VersionName(st.Version), resumed: st.DidResume}
	return tlsConn, true
}

// clientConn is the state of one client connection. It belongs to the
// connection's goroutine, except that the request-body writer goroutine
// reads through br while a request with a body is in flight.
type clientConn struct {
	p   *Proxy
	tc  *TrackedConn
	ip  string
	log *slog.Logger

	// c is what requests are read from and responses written to: tc, or
	// the TLS connection over it once the handshake is done. tc keeps the
	// connection's tracking state either way.
	c net.Conn
	// scheme is "http" or "https", sent upstream as X-Forwarded-Proto.
	scheme string
	// sni is the normalised server name from the TLS handshake, empty for
	// plain HTTP or a client that sent none.
	sni     string
	tlsInfo tlsInfo
	// redirectPort, if non-zero, makes the connection answer every request
	// with a redirect to HTTPS on that port instead of proxying it.
	redirectPort int

	// t and maxHeader come from the snapshot of the current request, or of
	// the previous one while waiting for the next. A new connection starts
	// with the snapshot current at accept.
	t         config.Timeouts
	maxHeader int64
	// info is handed to the balancer. It lives here, not on the stack, so
	// passing it through the Balancer interface does not allocate.
	info lb.RequestInfo

	lr httpx.HeaderLimitReader
	br *bufio.Reader
	dw httpx.DeadlineWriter
	bw *bufio.Writer

	// raw holds the current request's header block as it arrived, for
	// httpx.CheckRaw. requests counts the requests read on the connection.
	raw      []byte
	requests int64

	// linger is set when unread request bytes may be left on the
	// connection, so it must be closed with a drain.
	linger bool

	// Per-request state, kept here so each request reuses it rather than
	// allocating its own. Each is valid for one request at a time.
	rec       accessRecord
	reqBody   httpx.Body
	respBody  httpx.Body
	reqFlush  flushingReader
	respFlush flushingReader
	writer    bodyWriter
	// interimSent is set once a 1xx response has been relayed for the
	// current request, after which it cannot go to another backend.
	interimSent bool
	// tried holds the backends the current request has been sent to, and
	// pickBuf the candidates for its next one.
	tried   []*lb.Backend
	pickBuf []*lb.Backend
}

// tlsInfo is what the access log reports about a connection's TLS session.
type tlsInfo struct {
	version string
	resumed bool
}

func newClientConn(p *Proxy, tc *TrackedConn, snap *config.Snapshot) *clientConn {
	cc := &clientConn{p: p, tc: tc, c: tc, scheme: "http", ip: clientIP(tc.RemoteAddr()), log: p.Logger}
	if cc.log == nil {
		cc.log = slog.New(slog.DiscardHandler)
	}
	cc.use(snap)
	cc.lr.R = tc
	cc.br = httpx.NewReader(&cc.lr)
	cc.dw = httpx.DeadlineWriter{Conn: tc, Timeout: cc.t.ClientIO}
	cc.bw = httpx.NewWriter(&cc.dw)
	return cc
}

// release returns the connection's buffers to their pools. It runs last,
// once the request loop and any lingering close are done.
func (cc *clientConn) release() {
	httpx.PutReader(cc.br)
	httpx.PutWriter(cc.bw)
	cc.br, cc.bw = nil, nil
}

// use takes the connection's settings from snap.
func (cc *clientConn) use(snap *config.Snapshot) {
	cc.t = snap.Timeouts
	cc.maxHeader = int64(snap.Limits.MaxHeaderBytes)
}

// serveRequest handles one request and reports whether the connection can
// carry another.
func (cc *clientConn) serveRequest(ctx context.Context, first bool) bool {
	wait := cc.t.Idle
	if first {
		wait = cc.t.HeaderRead
	} else {
		cc.tc.SetState(StateIdle)
	}
	if err := cc.c.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return false
	}
	// Checked after the state and the deadline are set, so a drain that
	// starts now either is seen here or finds the connection idle and
	// moves its deadline to now (Tracker.Drain). A new connection still
	// gets its first request.
	if !first && cc.tc.Draining() {
		return false
	}
	cc.lr.Limit(cc.maxHeader)
	// Wait for the first byte of the next request. EOF, the idle timeout or
	// a closed connection all end the connection quietly.
	if _, err := cc.br.Peek(1); err != nil {
		return false
	}

	// The request starts with its first byte, and so does its snapshot: a
	// request that arrives after a long idle wait is served by the config
	// current now, not the one current when the wait began. The header
	// limit for this request was set above, from the previous snapshot.
	snap := cc.p.Config.Load()
	cc.use(snap)
	start := time.Now()
	cc.tc.SetState(StateActive)
	headerDeadline := start.Add(cc.t.HeaderRead)
	if err := cc.c.SetReadDeadline(headerDeadline); err != nil {
		return false
	}
	req, reason, err := httpx.ReadRequest(cc.br, &cc.lr, &cc.raw, snap.Limits.Request())
	// A large header block grows raw; a connection does not keep that
	// much memory between requests.
	if cap(cc.raw) > maxKeptRaw {
		cc.raw = nil
	}
	if err != nil {
		// A deadline hit mid-line can surface as a parse error of the
		// partial line rather than a timeout, so the clock decides.
		timedOut := isTimeout(err) || !time.Now().Before(headerDeadline)
		cc.headerError(start, err, timedOut)
		return false
	}
	cc.requests++
	if reason != httpx.ReasonNone {
		cc.p.Metrics.BadRequest(reason)
		return cc.reject(cc.newRecord(start, req), req, reason.Status(), reason.String())
	}
	if cc.p.onRequest != nil {
		cc.p.onRequest(req)
	}
	if cc.redirectPort != 0 {
		return cc.redirect(snap, req, start)
	}
	return cc.proxy(ctx, snap, req, start)
}

// reject answers a request the proxy will not forward with status, before
// anything has been sent upstream, and ends the connection.
func (cc *clientConn) reject(rec *accessRecord, req *http.Request, status int, reason string) bool {
	cc.linger = req.Body != http.NoBody
	cc.writeError(status, req.Method == http.MethodHead)
	rec.status, rec.err = status, errors.New(reason)
	cc.logAccess(rec)
	return false
}

// headerError answers a request whose header block could not be read.
func (cc *clientConn) headerError(start time.Time, err error, timedOut bool) {
	rec := &accessRecord{start: start, upstream: -1, err: err}
	switch {
	case cc.lr.Exhausted():
		rec.status = http.StatusRequestHeaderFieldsTooLarge
		cc.p.Metrics.BadRequest(httpx.ReasonHeaderTooLarge)
	case timedOut:
		// A client that stalls mid-header gets no reply, only a close.
		rec.status = http.StatusRequestTimeout
		cc.logAccess(rec)
		return
	case isClientGone(err):
		rec.status = StatusClientClosed
		cc.logAccess(rec)
		return
	default:
		rec.status = http.StatusBadRequest
		cc.p.Metrics.BadRequest(httpx.ReasonMalformed)
	}
	cc.linger = true
	cc.writeError(rec.status, false)
	cc.logAccess(rec)
}

// proxy routes req to a pool and forwards it to a backend the pool's
// balancer picks, then the response back. If the backend fails before any
// response byte has reached the client, the request may go to another
// backend (see retryElsewhere). It reports whether the client connection
// can carry another request.
func (cc *clientConn) proxy(ctx context.Context, snap *config.Snapshot, req *http.Request, start time.Time) bool {
	rec := cc.newRecord(start, req)
	clientKeepAlive := !req.Close && req.ProtoAtLeast(1, 1)
	if max := snap.Limits.MaxRequestsPerConn; max > 0 && cc.requests >= max {
		// The connection's last request: its response says so.
		clientKeepAlive = false
	}
	head := req.Method == http.MethodHead

	if reason := cc.misdirected(snap, req.Host); reason != "" {
		return cc.reject(rec, req, http.StatusMisdirectedRequest, reason)
	}
	target := snap.Lookup(req.Host)
	if target == nil {
		return cc.reject(rec, req, http.StatusNotFound, "no route for host")
	}
	pool := target.Pool
	rec.pool, rec.poolStats = pool.Name, pool.Stats
	// The rate limit comes after routing, since each route has its own,
	// and before a backend is chosen, so a refused request costs no
	// balancer pick and no upstream connection.
	if target.Limiter != nil {
		client := ratelimit.ClientAddr(cc.tc.Peer(), req.Header["X-Forwarded-For"], snap.TrustedProxies)
		if ok, wait := target.Limiter.Allow(client); !ok {
			target.RateLimited.Add(1)
			return cc.tooManyRequests(rec, req, client, wait, clientKeepAlive)
		}
	}
	// The balancer sees the headers as the client sent them, before the
	// proxy adds its own.
	cc.info = lb.RequestInfo{ClientIP: cc.ip, Header: req.Header}
	defer func() { cc.info.Header = nil }()
	b := pool.Pick(&cc.info)
	if b == nil {
		// Every backend is unhealthy: answer at once, without a dial.
		return cc.reject(rec, req, http.StatusServiceUnavailable, "no healthy backend")
	}
	httpx.PrepareRequest(req, httpx.Forwarding{ClientIP: cc.ip, Proto: cc.scheme,
		Trusted: snap.TrustedProxies.Contains(cc.tc.Peer())}, b.Conns.Reuse())

	var reqBody *httpx.Body
	if req.Body != nil && req.Body != http.NoBody {
		// Bytes already sent upstream are flushed before waiting on the
		// client, so a slow upload streams instead of sitting in a buffer.
		// forward points the flush at the connection it sends on.
		cc.reqFlush = flushingReader{r: req.Body, src: cc.br, wrap: wrapUpstreamErr}
		reqBody = &cc.reqBody
		reqBody.Reset(&cc.reqFlush, cc.c, cc.t.ClientIO)
		reqBody.SetLimits(snap.Limits.MaxBodyBytes, snap.Limits.MinBodyRate, time.Now(), snap.Timeouts.BodyGrace)
		reqBody.SetRequest(req)
		req.Body = reqBody
	}
	cc.interimSent = false
	cc.tried = append(cc.tried[:0], b)
	for {
		res := cc.forward(ctx, rec, pool, b, req, reqBody, head, clientKeepAlive)
		if res.done {
			return res.keepAlive
		}
		var next *lb.Backend
		reason, ok := cc.retryElsewhere(ctx, res, req, reqBody)
		if ok && len(cc.tried) <= snap.Upstream.MaxRetries {
			next = pool.PickExcluding(&cc.info, cc.tried, &cc.pickBuf)
		}
		if next == nil {
			cc.answerFailure(rec, res, reqBody, head)
			return false
		}
		pool.Stats.Retry(reason)
		rec.retries++
		b = next
		cc.tried = append(cc.tried, b)
	}
}

// attempt is the outcome of forwarding a request to one backend.
type attempt struct {
	// done is set once a response was received and relayed, or failed
	// while being relayed: the request is finished either way. keepAlive
	// is then whether the client connection can carry another.
	done      bool
	keepAlive bool
	// Otherwise the request failed before a response: dialErr if no
	// connection could be opened, else the errors writing the request and
	// reading the response.
	dialErr  error
	writeErr error
	readErr  error
}

// forward sends req to b and relays its response. A failure before the
// response headers is returned without answering the client, so the
// caller can try another backend; everything after is handled here. Each
// outcome is reported to b's health: dial failures, timeouts, resets and
// 5xx responses as failures, other responses as successes.
func (cc *clientConn) forward(ctx context.Context, rec *accessRecord, pool *lb.Pool, b *lb.Backend, req *http.Request, reqBody *httpx.Body, head, clientKeepAlive bool) attempt {
	// The count covers the whole exchange, until the response has been
	// written to the client or has failed.
	b.Acquire()
	defer b.Release()
	rec.backend, rec.backendStats = b.Addr, b.Stats
	rec.reused, rec.upstream = false, -1

	upStart := time.Now()
	uc, err := b.Conns.Get(ctx, cc.t.UpstreamConnect, cc.t.UpstreamResponse)
	if err != nil {
		cc.reportFailure(ctx, b)
		return attempt{dialErr: err}
	}
	rec.reused = uc.Reused()
	if reqBody != nil {
		cc.reqFlush.w = uc.W
	}
	cc.tc.SetUpstream(uc)
	// Every path that does not park the connection closes it. Deferred
	// before the writer's stop, so it runs after the writer is joined. uc
	// is nil once parked, or if the retry's dial failed.
	defer func() {
		cc.tc.SetUpstream(nil)
		if uc != nil {
			uc.Discard()
		}
	}()

	w := &cc.writer
	*w = bodyWriter{}
	defer w.stop() // reached on every path; a no-op once joined
	resp, err := cc.exchange(uc, req, reqBody, w)
	if err != nil && retryStale(uc, req, reqBody, w, err) {
		// The backend closed a kept-alive connection as the request went
		// out, and sent nothing back. Once more, on a new connection.
		uc.Discard()
		rec.retried = true
		pool.Stats.Retry(metrics.RetryStale)
		uc, err = b.Conns.Dial(ctx, cc.t.UpstreamConnect, cc.t.UpstreamResponse)
		cc.tc.SetUpstream(uc)
		if err != nil {
			cc.reportFailure(ctx, b)
			return attempt{dialErr: err}
		}
		rec.reused = false
		resp, err = cc.exchange(uc, req, reqBody, w)
	}
	if err != nil {
		w.stop()
		res := attempt{writeErr: w.err, readErr: err}
		if !clientFailed(reqBody, err) {
			cc.reportFailure(ctx, b)
		}
		return res
	}
	rec.upstream = time.Since(upStart)
	rec.status = resp.StatusCode
	upstreamKeepAlive := keepsConnection(resp)

	var respBody *httpx.Body
	if resp.Body != nil && resp.Body != http.NoBody {
		// Everything received so far, headers included, reaches the client
		// before the proxy waits on the backend for more. Without this a
		// slow stream would sit in the 4 KiB buffer, and a backend failing
		// mid-body would leave the client with nothing at all.
		cc.respFlush = flushingReader{r: resp.Body, src: uc.R, w: cc.bw, wrap: wrapClientErr}
		respBody = &cc.respBody
		respBody.Reset(&cc.respFlush, uc, cc.t.UpstreamResponse)
		resp.Body = respBody
	}
	keepAlive := clientKeepAlive
	if w.done() && reqBody != nil && !reqBody.EOF() {
		// The backend stopped taking the body; what is left of it is
		// still on the client connection.
		keepAlive = false
	}
	if cc.tc.Draining() {
		// Shutting down: this response is the connection's last.
		keepAlive = false
	}
	keepAlive = httpx.PrepareResponse(resp, req, keepAlive)

	cc.dw.Timeout = cc.t.ClientIO
	werr := resp.Write(noReadFrom{cc.bw})
	if werr == nil {
		werr = cc.bw.Flush()
	}
	// The response is complete or failed; either way the backend gets no
	// more of the request body. If the connection is otherwise fit to
	// reuse, a running writer gets a short grace to finish; one that does
	// not means the backend answered before taking the whole body, so the
	// connection is not reused. Otherwise the writer is stopped at once.
	reusable := werr == nil && upstreamKeepAlive && (respBody == nil || respBody.EOF())
	writerFinished := reusable && w.finished()
	w.stop()
	if respBody != nil {
		rec.bytesOut = respBody.N()
	}
	if reqBody != nil {
		rec.bytesIn = reqBody.N()
	}
	// When the client's request body failed (too slow, too large, cut
	// off), the writer closed the upstream connection, so a response
	// body that fails after it failed because of the client: the backend
	// did nothing wrong, and is neither charged nor credited. Without
	// this a slow-body attack on a streaming endpoint ejected every
	// backend (bench/phase8/slowhttptest.sh). Short-circuited, so the
	// success path calls neither clientBodyFailed nor isClientErr, whose
	// errors.As targets escape to the heap.
	clientBodyErr := reqBody != nil && reqBody.Err() != nil && clientBodyFailed(reqBody)
	upstreamBodyFailed := !clientBodyErr && respBody != nil && respBody.Err() != nil && !isClientErr(respBody.Err())
	switch {
	case clientBodyErr:
	case upstreamBodyFailed || resp.StatusCode >= 500:
		cc.reportFailure(ctx, b)
	default:
		b.Health.ReportSuccess()
	}
	if werr != nil {
		// Mid-response the status line is already out, so the only signal
		// left is closing the connection.
		switch {
		case clientBodyErr:
			rec.err = fmt.Errorf("client body: %w", reqBody.Err())
		case upstreamBodyFailed:
			rec.err = fmt.Errorf("upstream body: %w", respBody.Err())
		default:
			rec.err = fmt.Errorf("client write: %w", werr)
		}
		cc.logAccess(rec)
		return attempt{done: true}
	}
	if reqBody != nil && !reqBody.EOF() {
		keepAlive = false
		cc.linger = true
	}
	// Reuse needs the request sent whole, the response read whole, and a
	// backend that framed it and did not ask to close. The connection is
	// parked before logging, so the next request can have it at once.
	if reusable && writerFinished && w.err == nil &&
		(reqBody == nil || reqBody.EOF()) {
		cc.tc.SetUpstream(nil)
		b.Conns.Put(uc)
		uc = nil
	}
	cc.logAccess(rec)
	return attempt{done: true, keepAlive: keepAlive}
}

// reportFailure counts a failure against b's health, unless the server is
// closing: then the proxy cut the request off, not the backend.
func (cc *clientConn) reportFailure(ctx context.Context, b *lb.Backend) {
	if ctx.Err() == nil {
		b.Health.ReportFailure()
	}
}

// clientFailed reports whether a request that got no response failed
// because of the client: its body broke off, or relaying a 1xx to it
// failed.
func clientFailed(reqBody *httpx.Body, readErr error) bool {
	return clientBodyFailed(reqBody) || isClientErr(readErr)
}

// isClientErr reports whether err is, or wraps, a failure writing to the
// client.
func isClientErr(err error) bool {
	var ce clientError
	return errors.As(err, &ce)
}

// retryElsewhere reports whether a request whose attempt failed before a
// response may be sent to another backend, and why. Whether another
// backend is left, and the retry limit, are the caller's to check. All of
// these must hold:
//
//   - The server is not closing.
//   - No byte of a response has reached the client. A relayed 1xx counts:
//     the client has started reading this backend's answer.
//   - The failure was the backend's, not the client's.
//   - A failed dial is retried for any method: nothing was sent, and not a
//     byte of the request body has been read from the client.
//   - Any later failure (reset, close, timeout, a malformed response) is
//     retried only for an idempotent method without a body. The backend
//     may have acted on the request, which is harmless only if doing it
//     twice is; and a body streams from the client as it is sent, so it
//     cannot be sent again.
func (cc *clientConn) retryElsewhere(ctx context.Context, res attempt, req *http.Request, reqBody *httpx.Body) (metrics.RetryReason, bool) {
	if ctx.Err() != nil || cc.interimSent {
		return 0, false
	}
	if res.dialErr != nil {
		return metrics.RetryDial, true
	}
	if reqBody != nil || !idempotent(req.Method) || clientFailed(reqBody, res.readErr) {
		return 0, false
	}
	if isTimeout(res.readErr) {
		return metrics.RetryTimeout, true
	}
	return metrics.RetryError, true
}

// answerFailure answers a request that no backend answered, by the last
// attempt's failure.
func (cc *clientConn) answerFailure(rec *accessRecord, res attempt, reqBody *httpx.Body, head bool) {
	if res.dialErr != nil {
		cc.dialError(rec, res.dialErr, head, reqBody != nil && !reqBody.EOF())
		return
	}
	cc.responseError(rec, reqBody, res.writeErr, res.readErr, head)
}

// exchange sends req on uc and reads the response headers. w is started
// as the request's writer; the caller joins it.
func (cc *clientConn) exchange(uc *upstream.Conn, req *http.Request, reqBody *httpx.Body, w *bodyWriter) (*http.Response, error) {
	if err := uc.SetReadDeadline(time.Now().Add(cc.t.UpstreamResponse)); err != nil {
		*w = bodyWriter{}
		return nil, err
	}
	w.start(cc, uc, req, reqBody, cc.p.afterRequestWrite)
	return cc.readResponse(uc, req)
}

// retryStale reports whether a request that failed on uc may be sent again
// to the same backend on a new connection. All of these must hold:
//
//   - uc was reused: a backend may close an idle keep-alive connection at
//     any moment, and a request sent just as it does finds the connection
//     dead. A new connection failing is a real failure.
//   - Not a byte of response arrived. Once one has, the backend has acted
//     on the request, and a 1xx may already have reached the client.
//   - The failure is the connection closing or resetting, not a timeout,
//     which means a slow backend rather than a stale connection.
//   - The request has no body. A body streams from the client as it is
//     sent, so what has gone cannot be sent again.
//   - Either nothing was written, so the backend cannot have seen the
//     request, or the method is idempotent, so seeing it twice is harmless.
//     A POST whose bytes left before the connection died may have been
//     processed by a backend that crashed before answering.
//
// The writer must be joined; a request without a body is written inline,
// so it always is. This retry does not count against max_retries: it
// says nothing about the backend's health.
func retryStale(uc *upstream.Conn, req *http.Request, reqBody *httpx.Body, w *bodyWriter, err error) bool {
	if !uc.Reused() || reqBody != nil || !w.done() || uc.BytesRead() != 0 {
		return false
	}
	if w.err != nil {
		err = w.err
	}
	if isTimeout(err) || !isConnClosed(err) {
		return false
	}
	return uc.BytesWritten() == 0 || idempotent(req.Method)
}

// isConnClosed reports whether err means the peer closed or reset the
// connection.
func isConnClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// idempotent reports whether method is idempotent and safe to repeat
// (RFC 9110 section 9.2.2). PUT and DELETE are idempotent too, but a
// retried request here has no body, and those are rare enough without
// one that the narrower list costs nothing.
func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// keepsConnection reports whether the backend's framing of resp lets the
// connection carry another request: it did not ask to close, and the body
// has a length or is chunked rather than ending when the connection does.
// It must be called before PrepareResponse rewrites the framing.
func keepsConnection(resp *http.Response) bool {
	if resp.Close {
		return false
	}
	return resp.Body == nil || resp.Body == http.NoBody ||
		resp.ContentLength >= 0 || slices.Contains(resp.TransferEncoding, "chunked")
}

// dialError answers a request whose upstream connection could not be
// opened: 504 if the dial timed out, 502 otherwise.
func (cc *clientConn) dialError(rec *accessRecord, err error, head, linger bool) {
	rec.status = http.StatusBadGateway
	if isTimeout(err) {
		rec.status = http.StatusGatewayTimeout
	}
	rec.err = fmt.Errorf("dial: %w", err)
	cc.linger = linger
	cc.writeError(rec.status, head)
	cc.logAccess(rec)
}

// tooManyRequests answers a request over its client's rate limit with 429
// and a Retry-After of wait, rounded up to seconds. A request without a body
// keeps the connection, if the client wants it: closing it would make a
// client that retries pay for a new connection, and a TLS handshake, every
// time. A request with a body ends the connection with a lingering close,
// as other refusals do, since the unread body is still on it.
func (cc *clientConn) tooManyRequests(rec *accessRecord, req *http.Request, client netip.Addr, wait time.Duration, clientKeepAlive bool) bool {
	keepAlive := clientKeepAlive && req.Body == http.NoBody && !cc.tc.Draining()
	cc.linger = req.Body != http.NoBody
	cc.dw.Timeout = errorWriteTimeout
	err := httpx.WriteTooManyRequests(cc.bw, ratelimit.RetryAfter(wait), keepAlive, req.Method == http.MethodHead)
	rec.status = http.StatusTooManyRequests
	rec.err = rateLimitedError{client: client}
	if err != nil {
		rec.err = fmt.Errorf("%w; client write: %w", rec.err, err)
	}
	cc.logAccess(rec)
	return keepAlive && err == nil
}

// rateLimitedError is the access log's error for a request refused by the
// rate limit. It names the address the limit was applied to, which differs
// from the connection's when a trusted proxy forwarded the request.
type rateLimitedError struct{ client netip.Addr }

func (e rateLimitedError) Error() string { return "rate limited: client " + e.client.String() }

// misdirected returns why a request for host must not be served on this
// connection, or "" if it may. On a TLS connection the request's host must
// be the one its SNI named: the certificate the client checked was chosen
// for that name, and so was the pool. A connection without SNI got the
// default certificate and belongs to the default pool, so it may not ask
// for a host that has a route of its own. Plain HTTP has no SNI to check.
func (cc *clientConn) misdirected(snap *config.Snapshot, host string) string {
	if cc.scheme != "https" {
		return ""
	}
	h := config.NormaliseHost(host)
	switch {
	case cc.sni == "" && snap.HasRoute(h):
		return "host has a route but the connection sent no SNI"
	case cc.sni != "" && h != cc.sni:
		return "host does not match SNI"
	}
	return ""
}

// readResponse reads the backend's final response, relaying any 1xx interim
// responses (such as 100 Continue) to the client on the way.
func (cc *clientConn) readResponse(uc *upstream.Conn, req *http.Request) (*http.Response, error) {
	return httpx.ReadResponse(uc.R, req, cc.relayInterim)
}

// relayInterim sends a 1xx response on to the client. Once one has gone,
// the request cannot be retried on another backend.
func (cc *clientConn) relayInterim(resp *http.Response) error {
	cc.interimSent = true
	cc.dw.Timeout = cc.t.ClientIO
	if err := httpx.WriteInterim(cc.bw, resp); err != nil {
		return clientError{err}
	}
	return nil
}

// responseError answers a request whose response headers never arrived.
// Who failed decides the status: the client while sending its body, the
// client while receiving a 1xx, or the backend.
func (cc *clientConn) responseError(rec *accessRecord, reqBody *httpx.Body, writeErr, readErr error, head bool) {
	if reqBody != nil {
		rec.bytesIn = reqBody.N()
	}
	cc.linger = reqBody != nil && !reqBody.EOF()
	var ce clientError
	switch {
	case clientBodyFailed(reqBody):
		rec.err = fmt.Errorf("client body: %w", reqBody.Err())
		rec.status = clientFailureStatus(reqBody.Err())
	case errors.As(readErr, &ce):
		rec.err = fmt.Errorf("client write: %w", ce.err)
		rec.status = StatusClientClosed
	case isTimeout(readErr):
		rec.err = fmt.Errorf("upstream response: %w", readErr)
		rec.status = http.StatusGatewayTimeout
	default:
		rec.err = fmt.Errorf("upstream response: %w", readErr)
		if writeErr != nil {
			rec.err = fmt.Errorf("upstream request: %w (then %v)", writeErr, readErr)
		}
		rec.status = http.StatusBadGateway
	}
	if rec.status != StatusClientClosed {
		cc.writeError(rec.status, head)
	}
	cc.logAccess(rec)
}

// clientFailureStatus maps an error reading the request body to a status:
// 408 for a stall, 499 for a client that left, 400 for broken framing such
// as a malformed chunk.
func clientFailureStatus(err error) int {
	switch {
	case errors.Is(err, httpx.ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, httpx.ErrBodyTooSlow):
		return http.StatusRequestTimeout
	case isTimeout(err):
		return http.StatusRequestTimeout
	case isClientGone(err):
		return StatusClientClosed
	}
	return http.StatusBadRequest
}

// isClientGone reports whether err means the peer closed or reset the
// connection.
func isClientGone(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// clientError marks a failure writing to the client.
type clientError struct{ err error }

func (e clientError) Error() string { return "client: " + e.err.Error() }
func (e clientError) Unwrap() error { return e.err }

// upstreamError marks a failure writing to the backend.
type upstreamError struct{ err error }

func (e upstreamError) Error() string { return "upstream: " + e.err.Error() }
func (e upstreamError) Unwrap() error { return e.err }

func wrapClientErr(err error) error   { return clientError{err} }
func wrapUpstreamErr(err error) error { return upstreamError{err} }

// clientBodyFailed reports whether reading the request body failed because
// of the client: not an abort by the proxy, not a failed flush upstream.
// Call only after the body's reader is done.
func clientBodyFailed(b *httpx.Body) bool {
	if b == nil || b.Err() == nil || errors.Is(b.Err(), httpx.ErrAborted) {
		return false
	}
	var ue upstreamError
	return !errors.As(b.Err(), &ue)
}

// noReadFrom hides bufio.Writer's ReadFrom from Request.Write and
// Response.Write. ReadFrom reads straight into the writer's own buffer, and
// flushingReader flushes that same writer from inside the read, which would
// corrupt it. Without ReadFrom the copy goes through net/http's pooled copy
// buffer and plain Writes. WriteByte stays visible so Request.Write does not
// wrap the writer in a new bufio.Writer of its own.
type noReadFrom struct{ bw *bufio.Writer }

func (n noReadFrom) Write(p []byte) (int, error)       { return n.bw.Write(p) }
func (n noReadFrom) WriteByte(c byte) error            { return n.bw.WriteByte(c) }
func (n noReadFrom) WriteString(s string) (int, error) { return n.bw.WriteString(s) }

// flushingReader flushes w before a read from r that may block, which is
// when src, the buffered reader r reads from, has nothing buffered. It
// forwards data as it arrives, at the cost of a flush per socket read when
// the stream is slow. Flush errors are tagged with wrap, so the caller can
// tell them from read errors.
type flushingReader struct {
	r    io.Reader
	src  *bufio.Reader
	w    *bufio.Writer
	wrap func(error) error
}

func (f *flushingReader) Read(p []byte) (int, error) {
	if f.src.Buffered() == 0 && f.w != nil && f.w.Buffered() > 0 {
		if err := f.w.Flush(); err != nil {
			return 0, f.wrap(err)
		}
	}
	return f.r.Read(p)
}

// writeError sends a fixed error response; the connection closes after it.
func (cc *clientConn) writeError(status int, head bool) {
	cc.dw.Timeout = errorWriteTimeout
	_ = httpx.WriteError(cc.bw, status, head)
}

// lingeringClose half-closes the connection and drains a bounded amount of
// unread input before the Server closes it, so the response already sent is
// not destroyed by a reset.
//
// Over TLS, close_notify goes first (crypto/tls bounds that write at 5
// seconds) and then the TCP half-close; the drain reads through the TLS
// connection, which is what consumes the client's remaining records.
func (cc *clientConn) lingeringClose() {
	if tlsConn, ok := cc.c.(*tls.Conn); ok {
		_ = tlsConn.CloseWrite()
	}
	if cw, ok := cc.tc.Conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = cc.c.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.CopyN(io.Discard, cc.c, lingerMaxBytes)
}

// bodyWriter sends a request to the backend. A request with a body is
// written from a separate goroutine, so the response can be read while the
// body is still streaming: a backend that answers early, or streams its
// reply while reading (like /echo), would otherwise stall both sides once
// the socket buffers fill.
//
// The goroutine is owned by the connection goroutine, which always joins it
// with stop before touching the client reader again.
type bodyWriter struct {
	uc      *upstream.Conn
	reqBody *httpx.Body
	ch      chan struct{} // nil when the request was written inline
	err     error         // valid once joined
}

// start sends req on uc, inline or from a new goroutine; w must not be
// running. after, if non-nil, runs in the goroutine once the request is
// sent (Proxy.afterRequestWrite).
func (w *bodyWriter) start(cc *clientConn, uc *upstream.Conn, req *http.Request, reqBody *httpx.Body, after func()) {
	*w = bodyWriter{uc: uc, reqBody: reqBody}
	if reqBody == nil {
		// No body: nothing can block on the client, so write inline.
		w.err = writeRequest(uc, req)
		return
	}
	w.ch = make(chan struct{})
	go func() {
		defer func() {
			// The writer is its own goroutine, out of reach of the
			// connection goroutine's recover. A panic here fails the
			// request: the upstream connection is closed, which wakes
			// the response reader.
			if v := recover(); v != nil {
				w.err = fmt.Errorf("request writer panic: %v", v)
				uc.Close()
				cc.writerPanicked(v)
			}
			close(w.ch)
		}()
		w.err = writeRequest(uc, req)
		if after != nil {
			after()
		}
		if w.err != nil && clientBodyFailed(reqBody) {
			// The client failed mid-body. No useful response can follow;
			// closing the upstream wakes the response reader at once.
			uc.Close()
		}
	}()
}

func writeRequest(uc *upstream.Conn, req *http.Request) error {
	if err := req.Write(noReadFrom{uc.W}); err != nil {
		return err
	}
	return uc.W.Flush()
}

// done reports whether the writer has finished. Once it returns true, err
// and the request body's counters may be read.
func (w *bodyWriter) done() bool {
	if w.ch == nil {
		return true
	}
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

// finished reports whether the writer finished on its own, waiting up to
// writerGrace for it. It is called only once the response is complete
// and the connection is otherwise fit to reuse.
//
// An instant check is not enough. A backend that read the whole body has
// had the writer's last flush, but the goroutine may not have returned
// from it yet: the backend can answer and the response reach the client
// first. Taken for an early answer, that would close a reusable
// connection. A writer still running after the grace is blocked on the
// client's body or on a backend that stopped reading it.
func (w *bodyWriter) finished() bool {
	if w.done() {
		return true
	}
	t := time.NewTimer(writerGrace)
	defer t.Stop()
	select {
	case <-w.ch:
		return true
	case <-t.C:
		return false
	}
}

// stop joins the writer, aborting it first if it is still running: the
// client body read is made to fail and the upstream connection is closed,
// which unblocks whichever side the writer is waiting on.
func (w *bodyWriter) stop() {
	if w.done() {
		return
	}
	w.reqBody.Abort()
	w.uc.Close()
	<-w.ch
}

// writerPanicked logs and counts a panic recovered in the request writer
// goroutine.
func (cc *clientConn) writerPanicked(v any) {
	cc.log.Error("panic writing a request upstream; failing it",
		"client", cc.ip, "panic", v, "stack", string(debug.Stack()))
	cc.p.Metrics.Panic()
}

// clientIP returns the address part of a remote address, without brackets
// or an IPv4-in-IPv6 prefix.
func clientIP(a net.Addr) string {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap().String()
	}
	if host, _, err := net.SplitHostPort(a.String()); err == nil {
		return host
	}
	return a.String()
}

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
	"slices"
	"syscall"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
	"github.com/Starscourge/tls-proxy-lb/internal/lb"
	"github.com/Starscourge/tls-proxy-lb/internal/tlsx"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

const (
	clientReadBufferSize  = 4 << 10
	clientWriteBufferSize = 4 << 10

	// errorWriteTimeout bounds writing a proxy-generated error response.
	errorWriteTimeout = time.Second
	// lingerTimeout and lingerMaxBytes bound the drain before closing a
	// connection that may hold unread request bytes. Closing with unread
	// data sends a reset, which can destroy a response the client has not
	// read yet.
	lingerTimeout  = 500 * time.Millisecond
	lingerMaxBytes = 256 << 10
)

// StatusClientClosed is logged (never sent) when the client goes away before
// a response, as nginx does.
const StatusClientClosed = 499

// Proxy routes each request on a client connection by its Host header to a
// pool, picks a backend with the pool's balancer and forwards the request
// over a fresh upstream connection. It implements Handler.
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

	// linger is set when unread request bytes may be left on the
	// connection, so it must be closed with a drain.
	linger bool
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
	cc.br = bufio.NewReaderSize(&cc.lr, clientReadBufferSize)
	cc.dw = httpx.DeadlineWriter{Conn: tc, Timeout: cc.t.ClientIO}
	cc.bw = bufio.NewWriterSize(&cc.dw, clientWriteBufferSize)
	return cc
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
	req, err := http.ReadRequest(cc.br)
	if err != nil {
		// A deadline hit mid-line can surface as a parse error of the
		// partial line rather than a timeout, so the clock decides.
		timedOut := isTimeout(err) || !time.Now().Before(headerDeadline)
		cc.headerError(start, err, timedOut)
		return false
	}
	cc.lr.Unlimit()

	if status, reason := httpx.CheckRequest(req); status != 0 {
		return cc.reject(cc.newRecord(start, req), req, status, reason)
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
	}
	cc.linger = true
	cc.writeError(rec.status, false)
	cc.logAccess(rec)
}

// proxy routes req to a pool, picks a backend, forwards req to it and the
// response back. It reports whether the client connection can carry
// another request.
func (cc *clientConn) proxy(ctx context.Context, snap *config.Snapshot, req *http.Request, start time.Time) bool {
	rec := cc.newRecord(start, req)
	clientKeepAlive := !req.Close && req.ProtoAtLeast(1, 1)
	head := req.Method == http.MethodHead

	if reason := cc.misdirected(snap, req.Host); reason != "" {
		return cc.reject(rec, req, http.StatusMisdirectedRequest, reason)
	}
	pool := snap.Route(req.Host)
	if pool == nil {
		return cc.reject(rec, req, http.StatusNotFound, "no route for host")
	}
	rec.pool = pool.Name
	// The balancer sees the headers as the client sent them, before the
	// proxy adds its own.
	cc.info = lb.RequestInfo{ClientIP: cc.ip, Header: req.Header}
	b := pool.Pick(&cc.info)
	cc.info.Header = nil
	if b == nil {
		return cc.reject(rec, req, http.StatusServiceUnavailable, "no backend available")
	}
	// The count covers the whole exchange, until the response has been
	// written to the client or has failed.
	b.Acquire()
	defer b.Release()
	rec.backend = b.Addr

	httpx.PrepareRequest(req, cc.ip, cc.scheme, b.Conns.Reuse())

	upStart := time.Now()
	uc, err := b.Conns.Get(ctx, cc.t.UpstreamConnect, cc.t.UpstreamResponse)
	var reqBody *httpx.Body
	if req.Body != nil && req.Body != http.NoBody {
		// Bytes already sent upstream are flushed before waiting on the
		// client, so a slow upload streams instead of sitting in a buffer.
		var src io.Reader = req.Body
		if uc != nil {
			src = &flushingReader{r: req.Body, src: cc.br, w: uc.W, wrap: wrapUpstreamErr}
		}
		reqBody = httpx.NewBody(src, cc.c, cc.t.ClientIO)
		req.Body = reqBody
	}
	if err != nil {
		cc.dialError(rec, err, head, reqBody != nil)
		return false
	}
	rec.reused = uc.Reused()
	// The connection goes back to the pool only if the exchange left it
	// clean; every other path closes it. Deferred before the writer's stop,
	// so it runs after the writer is joined.
	reusable := false
	defer func() {
		switch {
		case reusable:
			b.Conns.Put(uc)
		default:
			uc.Close()
		}
	}()

	var w *bodyWriter
	defer func() { w.stop() }() // reached on every path; a no-op once joined
	resp, err := cc.exchange(uc, req, reqBody, &w)
	if err != nil {
		w.stop()
		cc.responseError(rec, reqBody, w.err, err, head)
		return false
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
		src := &flushingReader{r: resp.Body, src: uc.R, w: cc.bw, wrap: wrapClientErr}
		respBody = httpx.NewBody(src, uc, cc.t.UpstreamResponse)
		resp.Body = respBody
	}
	keepAlive := clientKeepAlive
	if w.done() && reqBody != nil && !reqBody.EOF() {
		// The backend stopped taking the body; what is left of it is
		// still on the client connection.
		keepAlive = false
	}
	keepAlive = httpx.PrepareResponse(resp, req, keepAlive)

	cc.dw.Timeout = cc.t.ClientIO
	werr := resp.Write(noReadFrom{cc.bw})
	if werr == nil {
		werr = cc.bw.Flush()
	}
	// The response is complete or failed; either way the backend gets no
	// more of the request body. A writer still running now means the
	// backend answered before taking the whole body, so the connection is
	// not reused.
	writerFinished := w.done()
	w.stop()
	if respBody != nil {
		rec.bytesOut = respBody.N()
	}
	if reqBody != nil {
		rec.bytesIn = reqBody.N()
	}
	if werr != nil {
		// Mid-response the status line is already out, so the only signal
		// left is closing the connection.
		var ce clientError
		if respBody != nil && respBody.Err() != nil && !errors.As(respBody.Err(), &ce) {
			rec.err = fmt.Errorf("upstream body: %w", respBody.Err())
		} else {
			rec.err = fmt.Errorf("client write: %w", werr)
		}
		cc.logAccess(rec)
		return false
	}
	if reqBody != nil && !reqBody.EOF() {
		keepAlive = false
		cc.linger = true
	}
	// Reuse needs the request sent whole, the response read whole, and a
	// backend that framed it and did not ask to close.
	reusable = upstreamKeepAlive && writerFinished && w.err == nil &&
		(reqBody == nil || reqBody.EOF()) &&
		(respBody == nil || respBody.EOF())
	cc.logAccess(rec)
	return keepAlive
}

// exchange sends req on uc and reads the response headers. *w is set to
// the request's writer, which the caller joins.
func (cc *clientConn) exchange(uc *upstream.Conn, req *http.Request, reqBody *httpx.Body, w **bodyWriter) (*http.Response, error) {
	if err := uc.SetReadDeadline(time.Now().Add(cc.t.UpstreamResponse)); err != nil {
		*w = &bodyWriter{}
		return nil, err
	}
	*w = startWriter(uc, req, reqBody)
	return cc.readResponse(uc, req)
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
	for {
		resp, err := http.ReadResponse(uc.R, req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols:
			// Upgrade is stripped from requests, so a 101 is a broken backend.
			return nil, errors.New("unexpected 101 Switching Protocols")
		case resp.StatusCode >= 100 && resp.StatusCode < 200:
			cc.dw.Timeout = cc.t.ClientIO
			if err := httpx.WriteInterim(cc.bw, resp); err != nil {
				return nil, clientError{err}
			}
		default:
			return resp, nil
		}
	}
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
	if f.src.Buffered() == 0 && f.w.Buffered() > 0 {
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

func startWriter(uc *upstream.Conn, req *http.Request, reqBody *httpx.Body) *bodyWriter {
	w := &bodyWriter{uc: uc, reqBody: reqBody}
	if reqBody == nil {
		// No body: nothing can block on the client, so write inline.
		w.err = writeRequest(uc, req)
		return w
	}
	w.ch = make(chan struct{})
	go func() {
		defer close(w.ch)
		w.err = writeRequest(uc, req)
		if w.err != nil && clientBodyFailed(reqBody) {
			// The client failed mid-body. No useful response can follow;
			// closing the upstream wakes the response reader at once.
			uc.Close()
		}
	}()
	return w
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
	if w == nil || w.ch == nil {
		return true
	}
	select {
	case <-w.ch:
		return true
	default:
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

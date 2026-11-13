package conn

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/metrics"
)

// accessRecord collects what the access log line reports for one request.
type accessRecord struct {
	start   time.Time
	method  string
	path    string
	status  int
	host    string
	pool    string
	backend string
	// upstream is the time from starting the dial to having the response
	// headers, or -1 if no response arrived.
	upstream time.Duration
	bytesIn  int64 // request body bytes read from the client
	bytesOut int64 // response body bytes read from the backend
	// reused is set when the response came on a kept-alive connection,
	// retried when a reused connection failed and the request was sent
	// again on a new one.
	reused  bool
	retried bool
	// retries is how many other backends the request went to after the
	// first failed; backend is the last one.
	retries int
	err     error
	// poolStats and backendStats are where the request is counted: the
	// backend's if it reached one, else the pool's, else the proxy's
	// unrouted counts.
	poolStats    *metrics.PoolStats
	backendStats *metrics.BackendStats
}

// newRecord starts the connection's access record afresh for req.
func (cc *clientConn) newRecord(start time.Time, req *http.Request) *accessRecord {
	cc.rec = accessRecord{start: start, method: req.Method, host: req.Host, path: req.URL.Path, upstream: -1}
	return &cc.rec
}

// logAccess writes the access line: client IP, scheme, method, host, path,
// status, pool, backend, upstream time, total time, body bytes in each
// direction, whether the upstream connection was reused (and whether the
// request was retried on a new one), for HTTPS the SNI, TLS version and
// whether the session was resumed, and the error that ended the request, if
// any. It also counts the request in the metrics.
func (cc *clientConn) logAccess(r *accessRecord) {
	total := time.Since(r.start)
	cc.count(r, total)
	attrs := []slog.Attr{
		slog.String("client", cc.ip),
		slog.String("scheme", cc.scheme),
		slog.String("method", r.method),
		slog.String("host", r.host),
		slog.String("path", r.path),
		slog.Int("status", r.status),
		slog.String("pool", r.pool),
		slog.String("backend", r.backend),
		slog.Float64("upstream_ms", ms(r.upstream)),
		slog.Float64("total_ms", ms(total)),
		slog.Int64("bytes_in", r.bytesIn),
		slog.Int64("bytes_out", r.bytesOut),
		slog.Bool("reused", r.reused),
	}
	if r.retried {
		attrs = append(attrs, slog.Bool("retried", true))
	}
	if r.retries > 0 {
		attrs = append(attrs, slog.Int("retries", r.retries))
	}
	if cc.scheme == "https" {
		attrs = append(attrs,
			slog.String("sni", cc.sni),
			slog.String("tls_version", cc.tlsInfo.version),
			slog.Bool("tls_resumed", cc.tlsInfo.resumed))
	}
	if r.err != nil {
		attrs = append(attrs, slog.String("error", r.err.Error()))
	}
	cc.log.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
}

// count adds the request's status to its counts and its total time to its
// pool's latency histogram.
func (cc *clientConn) count(r *accessRecord, total time.Duration) {
	switch {
	case r.backendStats != nil:
		r.backendStats.Codes.Add(r.status)
	case r.poolStats != nil:
		r.poolStats.NoBackend.Add(r.status)
	default:
		if u := cc.p.Metrics.Unrouted(); u != nil {
			u.Add(r.status)
		}
	}
	if r.poolStats != nil && r.poolStats.Latency != nil {
		r.poolStats.Latency.Observe(total.Seconds())
	}
}

// ms converts d to milliseconds with microsecond precision; negative stays -1.
func ms(d time.Duration) float64 {
	if d < 0 {
		return -1
	}
	return float64(d.Microseconds()) / 1000
}

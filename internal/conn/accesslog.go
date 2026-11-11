package conn

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// accessRecord collects what the access log line reports for one request.
type accessRecord struct {
	start   time.Time
	method  string
	path    string
	status  int
	backend string
	// upstream is the time from starting the dial to having the response
	// headers, or -1 if no response arrived.
	upstream time.Duration
	bytesIn  int64 // request body bytes read from the client
	bytesOut int64 // response body bytes read from the backend
	err      error
}

func (cc *clientConn) newRecord(start time.Time, req *http.Request) *accessRecord {
	return &accessRecord{start: start, method: req.Method, path: req.URL.Path, upstream: -1}
}

// logAccess writes the access line: client IP, method, path, status,
// backend, upstream time, total time, body bytes in each direction, and the
// error that ended the request, if any.
func (cc *clientConn) logAccess(r *accessRecord) {
	attrs := []slog.Attr{
		slog.String("client", cc.ip),
		slog.String("method", r.method),
		slog.String("path", r.path),
		slog.Int("status", r.status),
		slog.String("backend", r.backend),
		slog.Float64("upstream_ms", ms(r.upstream)),
		slog.Float64("total_ms", ms(time.Since(r.start))),
		slog.Int64("bytes_in", r.bytesIn),
		slog.Int64("bytes_out", r.bytesOut),
	}
	if r.err != nil {
		attrs = append(attrs, slog.String("error", r.err.Error()))
	}
	cc.log.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
}

// ms converts d to milliseconds with microsecond precision; negative stays -1.
func ms(d time.Duration) float64 {
	if d < 0 {
		return -1
	}
	return float64(d.Microseconds()) / 1000
}

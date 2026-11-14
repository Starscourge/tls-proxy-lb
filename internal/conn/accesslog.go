package conn

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
//
// With Proxy.AccessLog set, the line is encoded straight into the
// connection's buffer and written in one call, byte for byte what slog's
// JSON handler would write (TestAccessLineMatchesSlog). Through slog it
// cost about 1.5 KB of garbage per request ([]slog.Attr and its growth)
// and took the handler's mutex for every line; in Phase 9's profiles that
// was 41% of allocated bytes, 11% of CPU and half or more of all mutex
// wait (47 to 83%).
func (cc *clientConn) logAccess(r *accessRecord) {
	total := time.Since(r.start)
	cc.count(r, total)
	if w := cc.p.AccessLog; w != nil {
		cc.logLine = cc.appendAccess(cc.logLine[:0], time.Now(), r, total)
		_, _ = w.Write(cc.logLine)
		if cap(cc.logLine) > maxKeptLogLine {
			cc.logLine = nil // a huge path or error: do not keep its buffer
		}
		return
	}
	cc.log.LogAttrs(context.Background(), slog.LevelInfo, "access", cc.accessAttrs(r, total)...)
}

// maxKeptLogLine is the largest line buffer a connection keeps between
// requests.
const maxKeptLogLine = 4096

// accessAttrs are the access line's fields as slog attributes, in order.
func (cc *clientConn) accessAttrs(r *accessRecord, total time.Duration) []slog.Attr {
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
	return attrs
}

// appendAccess appends the access line for r, logged at now, as slog's
// JSON handler writes it: the same fields as accessAttrs, in the same
// order, with the same encoding, and a newline.
func (cc *clientConn) appendAccess(b []byte, now time.Time, r *accessRecord, total time.Duration) []byte {
	b = append(b, `{"time":"`...)
	b = now.AppendFormat(b, time.RFC3339Nano)
	b = append(b, `","level":"INFO","msg":"access"`...)
	b = jsonStr(b, "client", cc.ip)
	b = jsonStr(b, "scheme", cc.scheme)
	b = jsonStr(b, "method", r.method)
	b = jsonStr(b, "host", r.host)
	b = jsonStr(b, "path", r.path)
	b = jsonInt(b, "status", int64(r.status))
	b = jsonStr(b, "pool", r.pool)
	b = jsonStr(b, "backend", r.backend)
	b = jsonFloat(b, "upstream_ms", ms(r.upstream))
	b = jsonFloat(b, "total_ms", ms(total))
	b = jsonInt(b, "bytes_in", r.bytesIn)
	b = jsonInt(b, "bytes_out", r.bytesOut)
	b = jsonBool(b, "reused", r.reused)
	if r.retried {
		b = jsonBool(b, "retried", true)
	}
	if r.retries > 0 {
		b = jsonInt(b, "retries", int64(r.retries))
	}
	if cc.scheme == "https" {
		b = jsonStr(b, "sni", cc.sni)
		b = jsonStr(b, "tls_version", cc.tlsInfo.version)
		b = jsonBool(b, "tls_resumed", cc.tlsInfo.resumed)
	}
	if r.err != nil {
		b = jsonStr(b, "error", r.err.Error())
	}
	return append(b, "}\n"...)
}

// jsonKey appends ,"key": for a key that needs no escaping.
func jsonKey(b []byte, key string) []byte {
	b = append(b, ',', '"')
	b = append(b, key...)
	return append(b, '"', ':')
}

func jsonStr(b []byte, key, v string) []byte {
	b = append(jsonKey(b, key), '"')
	b = appendJSONString(b, v)
	return append(b, '"')
}

func jsonInt(b []byte, key string, v int64) []byte {
	return strconv.AppendInt(jsonKey(b, key), v, 10)
}

func jsonBool(b []byte, key string, v bool) []byte {
	return strconv.AppendBool(jsonKey(b, key), v)
}

// jsonFloat appends v as encoding/json does, which is what slog uses for
// floats: 'f' format, or 'e' with a one-digit negative exponent below 1e-6
// and from 1e21. The access line's floats are finite.
func jsonFloat(b []byte, key string, v float64) []byte {
	b = jsonKey(b, key)
	f := byte('f')
	if a := math.Abs(v); a != 0 && (a < 1e-6 || a >= 1e21) {
		f = 'e'
	}
	b = strconv.AppendFloat(b, v, f, -1, 64)
	if n := len(b); f == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b[n-2] = b[n-1]
		b = b[:n-1]
	}
	return b
}

// appendJSONString appends s escaped as slog's JSON handler escapes it
// (log/slog appendEscapedJSONString): \\ and \" escaped, \n \r \t
// named, other control characters as \u00XX, U+2028 and U+2029 as \u2028
// and \u2029, and each byte of invalid UTF-8 as the replacement character,
// written the way this Go's slog writes it (invalidUTF8).
func appendJSONString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case c == utf8.RuneError && size == 1:
			b = append(b, s[start:i]...)
			b = append(b, invalidUTF8...)
		case c == '\u2028' || c == '\u2029':
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', '2', '0', '2', hex[c&0xF])
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	return append(b, s[start:]...)
}

// invalidUTF8 is what slog writes for a byte of invalid UTF-8: the escape
// \ufffd, or, built with the jsonv2 experiment, the character itself. It
// is asked of slog once, so the access line follows the Go it is built
// with.
var invalidUTF8 = func() string {
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
	})).Info("\xff")
	if strings.Contains(buf.String(), `\ufffd`) {
		return `\ufffd`
	}
	return "\ufffd"
}()

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

package conn

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"
)

// slogLine is the access line as Logger's slog JSON handler writes it.
func slogLine(t *testing.T, cc *clientConn, now time.Time, r *accessRecord, total time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	rec := slog.NewRecord(now, slog.LevelInfo, "access", 0)
	rec.AddAttrs(cc.accessAttrs(r, total)...)
	if err := slog.NewJSONHandler(&buf, nil).Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestAccessLineMatchesSlog: the line the proxy encodes itself is byte for
// byte the one slog's JSON handler writes from accessAttrs, so nothing that
// reads the log can tell which wrote it.
func TestAccessLineMatchesSlog(t *testing.T) {
	now := time.Date(2026, 11, 14, 11, 7, 15, 595383399, time.UTC)
	plain := &clientConn{ip: "10.0.0.20", scheme: "http"}
	tlsConn := &clientConn{ip: "2001:db8::1", scheme: "https", sni: "a.test", tlsInfo: tlsInfo{version: "TLS 1.3", resumed: true}}
	odd := "q\"b\\s\n\r\t\x00\x1f\x7f<&>é😀\u2028\u2029\xff\xfe end"
	cases := []struct {
		name  string
		cc    *clientConn
		r     accessRecord
		total time.Duration
	}{
		{"plain", plain, accessRecord{method: "GET", host: "a", path: "/", status: 200, pool: "p", backend: "10.0.0.11:8000",
			upstream: 316 * time.Microsecond, bytesOut: 20, reused: true}, 1080 * time.Microsecond},
		{"tls, retries, error", tlsConn, accessRecord{method: "POST", host: "a.test", path: "/echo", status: 502, pool: "p",
			backend: "b:1", upstream: -1, bytesIn: 5, retried: true, retries: 2, err: errors.New("dial tcp: i/o timeout")}, 1500 * time.Millisecond},
		{"odd strings", tlsConn, accessRecord{method: odd, host: odd, path: odd, status: 400, pool: odd, backend: odd,
			upstream: 0, err: errors.New(odd)}, 0},
		{"long times", plain, accessRecord{method: "GET", path: "/slow", status: 504, upstream: 3 * time.Hour}, 1e18},
		{"no record yet", &clientConn{}, accessRecord{upstream: -1}, time.Nanosecond},
	}
	for _, c := range cases {
		got := string(c.cc.appendAccess(nil, now, &c.r, c.total))
		if want := slogLine(t, c.cc, now, &c.r, c.total); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
	// Floats at the edges of encoding/json's switch to exponents.
	for _, v := range []float64{0, -1, 0.001, 1e-7, 1.5e-9, 1e20, 1e21, 123456789.123, math.MaxFloat64} {
		got := string(jsonFloat(nil, "x", v))
		var buf bytes.Buffer
		rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
		rec.AddAttrs(slog.Float64("x", v))
		if err := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key != "x" {
				return slog.Attr{}
			}
			return a
		}}).Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		if want := "," + string(bytes.TrimSuffix(bytes.TrimPrefix(buf.Bytes(), []byte("{")), []byte("}\n"))); got != want {
			t.Errorf("float %v: got %s want %s", v, got, want)
		}
	}
}

// TestAccessLineAllocatesNothing: with the connection's buffer reused, the
// line costs no allocation (an error's message aside).
func TestAccessLineAllocatesNothing(t *testing.T) {
	cc := &clientConn{ip: "10.0.0.20", scheme: "https", sni: "a.test", tlsInfo: tlsInfo{version: "TLS 1.3"}}
	r := accessRecord{method: "GET", host: "a.test", path: "/1k", status: 200, pool: "p", backend: "10.0.0.11:8000",
		upstream: time.Millisecond, bytesOut: 1024, reused: true}
	now := time.Now()
	buf := cc.appendAccess(nil, now, &r, 2*time.Millisecond)
	if n := testing.AllocsPerRun(100, func() { buf = cc.appendAccess(buf[:0], now, &r, 2*time.Millisecond) }); n != 0 {
		t.Fatalf("%v allocations per line", n)
	}
}

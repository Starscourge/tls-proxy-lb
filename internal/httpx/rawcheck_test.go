package httpx

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckRaw(t *testing.T) {
	lim := RawLimits{MaxHeaderCount: 3, MaxRequestLine: 40}
	cases := []struct {
		name, raw string
		want      Reason
	}{
		{"plain GET", "GET / HTTP/1.1\r\nHost: a\r\n\r\n", ReasonNone},
		{"CL body", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\n\r\nabc", ReasonNone},
		{"chunked", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", ReasonNone},
		{"Chunked with OWS", "POST / HTTP/1.1\r\nHost: a\r\ntransfer-encoding:\t Chunked \r\n\r\n", ReasonNone},
		{"CL leading zeros", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 003\r\n\r\nabc", ReasonNone},
		{"body bytes after the block are ignored", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 4\r\n\r\na\nb\r", ReasonNone},
		{"empty value", "GET / HTTP/1.1\r\nHost: a\r\nX-A:\r\n\r\n", ReasonNone},
		{"CL and TE", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n", ReasonCLAndTE},
		{"TE and CL", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nContent-Length: 6\r\n\r\n", ReasonCLAndTE},
		{"CL twice, same", "POST / HTTP/1.1\r\nContent-Length: 3\r\ncontent-length: 3\r\n\r\n", ReasonDuplicateCL},
		{"TE twice", "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n", ReasonBadTE},
		{"TE gzip", "POST / HTTP/1.1\r\nTransfer-Encoding: gzip\r\n\r\n", ReasonBadTE},
		{"TE list", "POST / HTTP/1.1\r\nTransfer-Encoding: gzip, chunked\r\n\r\n", ReasonBadTE},
		{"TE in HTTP/1.0", "POST / HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n", ReasonTEInHTTP10},
		{"space before colon", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding : chunked\r\n\r\n", ReasonSpaceBeforeColon},
		{"tab before colon", "GET / HTTP/1.1\r\nX-A\t: 1\r\n\r\n", ReasonSpaceBeforeColon},
		{"bare LF in the request line", "GET / HTTP/1.1\nHost: a\r\n\r\n", ReasonBareLF},
		{"bare LF in a field", "GET / HTTP/1.1\r\nHost: a\nX: 1\r\n\r\n", ReasonBareLF},
		{"bare LF ending the block", "GET / HTTP/1.1\r\nHost: a\r\n\n", ReasonBareLF},
		{"bare CR", "GET / HTTP/1.1\r\nX-A: 1\r2\r\n\r\n", ReasonBareCR},
		{"folded with a space", "GET / HTTP/1.1\r\nX-A: 1\r\n 2\r\n\r\n", ReasonFoldedHeader},
		{"folded with a tab", "GET / HTTP/1.1\r\nX-A: 1\r\n\tTransfer-Encoding: chunked\r\n\r\n", ReasonFoldedHeader},
		{"no colon", "GET / HTTP/1.1\r\nXA\r\n\r\n", ReasonMalformed},
		{"unterminated", "GET / HTTP/1.1\r\nHost: a\r\n", ReasonMalformed},
		{"three fields", "GET / HTTP/1.1\r\nA: 1\r\nB: 2\r\nC: 3\r\n\r\n", ReasonNone},
		{"four fields", "GET / HTTP/1.1\r\nA: 1\r\nB: 2\r\nC: 3\r\nD: 4\r\n\r\n", ReasonTooManyHeaders},
		{"request line at the limit", "GET /" + strings.Repeat("a", 26) + " HTTP/1.1\r\n\r\n", ReasonNone},
		{"request line over", "GET /" + strings.Repeat("a", 27) + " HTTP/1.1\r\n\r\n", ReasonRequestLineTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckRaw([]byte(tc.raw), lim); got != tc.want {
				t.Errorf("CheckRaw = %v, want %v", got, tc.want)
			}
		})
	}
	// Zero limits are no limits.
	many := "GET /" + strings.Repeat("a", 1000) + " HTTP/1.1\r\n" + strings.Repeat("A: 1\r\n", 500) + "\r\n"
	if got := CheckRaw([]byte(many), RawLimits{}); got != ReasonNone {
		t.Errorf("no limits: %v", got)
	}
}

func TestCheckRawAllocatesNothing(t *testing.T) {
	raw := []byte("POST /p HTTP/1.1\r\nHost: a.test\r\nContent-Length: 3\r\nX-A: b\r\n\r\nabc")
	if n := testing.AllocsPerRun(100, func() { CheckRaw(raw, RawLimits{MaxHeaderCount: 100}) }); n != 0 {
		t.Fatalf("CheckRaw allocates %v times", n)
	}
}

func TestReasonStatus(t *testing.T) {
	for r, want := range map[Reason]int{
		ReasonNone: 0, ReasonMalformed: 400, ReasonHeaderTooLarge: 431, ReasonTooManyHeaders: 431,
		ReasonRequestLineTooLong: 414, ReasonUnsupportedVersion: 505, ReasonConnect: 405,
		ReasonBodyTooLarge: 413, ReasonCLAndTE: 400, ReasonInvalidHost: 400,
	} {
		if got := r.Status(); got != want {
			t.Errorf("%v: %d, want %d", r, got, want)
		}
	}
	if names := ReasonNames(); len(names) != int(NumReasons)-1 || names[0] != "malformed" || names[len(names)-1] != "body_too_large" {
		t.Fatalf("ReasonNames %v", names)
	}
	for _, n := range ReasonNames() {
		if n == "" {
			t.Fatal("a reason has no name")
		}
	}
}

func TestValidHost(t *testing.T) {
	for host, want := range map[string]bool{
		"a.test": true, "a.test:8443": true, "10.0.0.1": true, "[::1]:443": true, "xn--bcher-kva.example": true,
		"a_b.test": true, "%41.test": true,
		"": false, "a, b": false, "a b": false, "a/b": false, "user@a": false, "a\tb": false, "\xff": false, "a\x00": false,
	} {
		if got := ValidHost(host); got != want {
			t.Errorf("ValidHost(%q) = %v", host, got)
		}
	}
}

// readOne runs ReadRequest on the next request from br, as a connection
// would: limit the header size, then read.
func readOne(br *bufio.Reader, lr *HeaderLimitReader, raw *[]byte, lim RequestLimits) (*http.Request, Reason, error) {
	lr.Limit(64 << 10)
	return ReadRequest(br, lr, raw, lim)
}

func TestReadRequest(t *testing.T) {
	lim := RequestLimits{RawLimits: RawLimits{MaxHeaderCount: 10, MaxRequestLine: 100}, MaxBodyBytes: 5}
	cases := []struct {
		name, raw string
		want      Reason
	}{
		{"ok", "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n", ReasonNone},
		{"HTTP/1.0 without Host", "GET / HTTP/1.0\r\n\r\n", ReasonNone},
		{"body at the cap", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 5\r\n\r\nabcde", ReasonNone},
		{"body over the cap", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\n\r\nabcdef", ReasonBodyTooLarge},
		{"CL and TE", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", ReasonCLAndTE},
		{"invalid Host", "GET / HTTP/1.1\r\nHost: a, b\r\n\r\n", ReasonInvalidHost},
		{"invalid absolute target host", "GET http://a%20b/ HTTP/1.1\r\nHost: a\r\n\r\n", ReasonMalformed},
		{"CONNECT", "CONNECT a:443 HTTP/1.1\r\nHost: a:443\r\n\r\n", ReasonConnect},
		{"HTTP/2.0", "GET / HTTP/2.0\r\nHost: a\r\n\r\n", ReasonUnsupportedVersion},
		{"bare LF", "GET / HTTP/1.1\nHost: a\n\n", ReasonBareLF},
		{"opaque target", "00 A:0 HTTP/1.0\r\n\r\n", ReasonBadTarget},
		{"relative target", "GET a/b HTTP/1.1\r\nHost: a\r\n\r\n", ReasonMalformed}, // net/http refuses it
		{"ftp target", "GET ftp://a/b HTTP/1.1\r\nHost: a\r\n\r\n", ReasonBadTarget},
		{"absolute target, no host", "GET http:///b HTTP/1.1\r\nHost: a\r\n\r\n", ReasonBadTarget},
		{"asterisk for GET", "GET * HTTP/1.1\r\nHost: a\r\n\r\n", ReasonBadTarget},
		{"asterisk for OPTIONS", "OPTIONS * HTTP/1.1\r\nHost: a\r\n\r\n", ReasonNone},
		{"absolute target", "GET https://a.test/x?y HTTP/1.1\r\nHost: b\r\n\r\n", ReasonNone},
		{"double slash path", "GET //x/y HTTP/1.1\r\nHost: a\r\n\r\n", ReasonNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := &HeaderLimitReader{R: strings.NewReader(tc.raw)}
			br := bufio.NewReader(lr)
			var raw []byte
			_, got, err := readOne(br, lr, &raw, lim)
			if err != nil {
				got = ReasonMalformed
			}
			if got != tc.want {
				t.Errorf("ReadRequest = %v (err %v), want %v", got, err, tc.want)
			}
		})
	}
}

// The second of two pipelined requests arrives in the same read as the
// first; its header block must still be checked as it arrived.
func TestReadRequestPipelined(t *testing.T) {
	in := "GET /1 HTTP/1.1\r\nHost: a\r\n\r\n" +
		"POST /2 HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"
	lr := &HeaderLimitReader{R: strings.NewReader(in)}
	br := bufio.NewReader(lr)
	var raw []byte
	req, r, err := readOne(br, lr, &raw, RequestLimits{})
	if err != nil || r != ReasonNone || req.URL.Path != "/1" {
		t.Fatalf("first: %v %v", r, err)
	}
	req, r, err = readOne(br, lr, &raw, RequestLimits{})
	if err != nil || req.URL.Path != "/2" || r != ReasonCLAndTE {
		t.Fatalf("second: %v %v, want cl_and_te", r, err)
	}
	if !bytes.HasPrefix(raw, []byte("POST /2 ")) {
		t.Fatalf("raw holds %q", raw)
	}
}

// Recording stops with the header block; the body is not copied.
func TestReadRequestStopsRecording(t *testing.T) {
	body := strings.Repeat("x", 1<<20)
	in := "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: " + itoa(int64(len(body))) + "\r\n\r\n" + body
	lr := &HeaderLimitReader{R: strings.NewReader(in)}
	br := bufio.NewReaderSize(lr, 4096)
	var raw []byte
	req, r, err := readOne(br, lr, &raw, RequestLimits{})
	if err != nil || r != ReasonNone {
		t.Fatal(r, err)
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 8192 {
		t.Fatalf("recorded %d bytes of a 1 MiB body", len(raw))
	}
}

// A Content-Length trailer is dropped at EOF and never written upstream.
func TestSanitizeTrailerOnWrite(t *testing.T) {
	in := "POST / HTTP/1.1\r\nHost: a\r\nTrailer: X-T\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"3\r\nabc\r\n0\r\nX-T: 1\r\nContent-Length: 99\r\nHost: evil\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	b := NewBody(req.Body, nil, 0)
	b.SetRequest(req)
	req.Body = b
	var out bytes.Buffer
	if err := req.Write(&out); err != nil {
		t.Fatal(err)
	}
	wire := out.String()
	if !strings.Contains(wire, "X-T: 1\r\n") {
		t.Fatalf("allowed trailer dropped:\n%s", wire)
	}
	if strings.Contains(wire, "Content-Length: 99") || strings.Contains(wire, "evil") {
		t.Fatalf("forbidden trailer forwarded:\n%s", wire)
	}
}

func TestBodyCap(t *testing.T) {
	for _, tc := range []struct {
		size    int
		wantErr error
	}{{5, nil}, {6, ErrBodyTooLarge}, {100000, ErrBodyTooLarge}} {
		b := NewBody(strings.NewReader(strings.Repeat("x", tc.size)), nil, 0)
		b.SetLimits(5, 0, time.Now(), 0)
		got, err := io.ReadAll(b)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("size %d: err %v, want %v", tc.size, err, tc.wantErr)
		}
		if len(got) > 5 || b.N() > 5 {
			t.Errorf("size %d: passed on %d bytes (N %d), cap 5", tc.size, len(got), b.N())
		}
		if !errors.Is(b.Err(), tc.wantErr) {
			t.Errorf("size %d: Err %v", tc.size, b.Err())
		}
	}
}

// A body that trickles below the minimum rate is cut off once grace has
// passed, however often it sends a byte; a steady one is not.
func TestBodyMinRate(t *testing.T) {
	run := func(t *testing.T, every time.Duration, total int) error {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		go func() {
			for range total {
				if _, err := client.Write([]byte("x")); err != nil {
					return
				}
				time.Sleep(every)
			}
			client.Close()
		}()
		b := NewBody(server, server, 5*time.Second)
		// 100 bytes/s after 200 ms of grace.
		b.SetLimits(0, 100, time.Now(), 200*time.Millisecond)
		_, err := io.Copy(io.Discard, b)
		return err
	}
	t.Run("trickle", func(t *testing.T) {
		start := time.Now()
		// One byte every 50 ms is 20 bytes/s: due out after about 250 ms.
		err := run(t, 50*time.Millisecond, 100)
		if !errors.Is(err, ErrBodyTooSlow) {
			t.Fatalf("err %v, want ErrBodyTooSlow", err)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("cut off after %v; the stall timeout is 5 s", el)
		}
	})
	t.Run("steady", func(t *testing.T) {
		// One byte every 2 ms is 500 bytes/s, five times the minimum.
		if err := run(t, 2*time.Millisecond, 300); err != nil {
			t.Fatalf("steady body: %v", err)
		}
	})
}

package httpx

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fuzzLimits are the limits the request fuzzer runs with: the defaults,
// with a small body cap so that path is reached.
var fuzzLimits = RequestLimits{RawLimits: RawLimits{MaxHeaderCount: 100, MaxRequestLine: 8192}, MaxBodyBytes: 4096}

// requestSeeds are well-formed requests and the smuggling cases from
// TestNetHTTPReadRequest, as a start for the fuzzer.
var requestSeeds = []string{
	"GET / HTTP/1.1\r\nHost: a.test\r\n\r\n",
	"GET /p?q=1 HTTP/1.0\r\n\r\n",
	"GET http://a.test/x HTTP/1.1\r\nHost: b.test\r\nConnection: close, X-Hop\r\nX-Hop: 1\r\nX-Forwarded-For: 203.0.113.1\r\n\r\n",
	"POST /echo HTTP/1.1\r\nHost: a.test\r\nContent-Length: 5\r\n\r\nhello",
	"POST /echo HTTP/1.1\r\nHost: a.test\r\nTransfer-Encoding: chunked\r\nTrailer: X-T\r\n\r\n5;e=1\r\nhello\r\n0\r\nX-T: 1\r\nContent-Length: 9\r\n\r\n",
	"POST / HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\nabc",
	"POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: a\r\n\r\n",
	"POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding : chunked\r\n\r\n0\r\n\r\n",
	"POST / HTTP/1.0\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
	"GET / HTTP/1.1\nHost: a\n\n",
	"GET / HTTP/1.1\r\nHost: a\r\nX-A: 1\r\n Transfer-Encoding: chunked\r\n\r\n",
	"POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\nContent-Length: 3\r\n\r\nabc",
	"POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\nfffffffffffffffff1\r\nabc\r\n0\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: a, b\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: a\r\n\r\nGET /2 HTTP/1.1\r\nHost: a\r\n\r\n",
}

// FuzzRequestPath sends raw client bytes through what the proxy does to a
// request: ReadRequest (net/http's parser and the proxy's checks), the
// Body wrapper with its cap and trailer cleaning, PrepareRequest, and
// Request.Write. Whatever goes in, if the proxy forwards anything, the
// bytes it sends parse as exactly one request, with the same method,
// target, host and body, and pass CheckRaw themselves.
func FuzzRequestPath(f *testing.F) {
	for _, s := range requestSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		lr := &HeaderLimitReader{R: bytes.NewReader(in)}
		br := bufio.NewReaderSize(lr, BufferSize)
		var raw []byte
		lr.Limit(16 << 10)
		req, reason, err := ReadRequest(br, lr, &raw, fuzzLimits)
		if err != nil || reason != ReasonNone {
			return // refused: nothing is forwarded
		}
		var sent bytes.Buffer // the body as the proxy read it
		if req.Body != nil && req.Body != http.NoBody {
			b := NewBody(io.TeeReader(req.Body, &sent), nil, 0)
			b.SetLimits(fuzzLimits.MaxBodyBytes, 0, timeZero, 0)
			b.SetRequest(req)
			req.Body = b
		}
		method, target, host := req.Method, req.URL.RequestURI(), req.Host
		PrepareRequest(req, Forwarding{ClientIP: "192.0.2.1", Proto: "https"}, true)
		var out bytes.Buffer
		if err := req.Write(&out); err != nil {
			// The body failed mid-stream (a bad chunk, over the cap): the
			// proxy closes the upstream connection, so what was written
			// is never taken as a whole request.
			return
		}

		obr := bufio.NewReader(bytes.NewReader(out.Bytes()))
		got, err := http.ReadRequest(obr)
		if err != nil {
			t.Fatalf("forwarded bytes do not parse: %v\nin:  %q\nout: %q", err, in, out.Bytes())
		}
		body, err := io.ReadAll(got.Body)
		if err != nil {
			t.Fatalf("forwarded body does not parse: %v\nin:  %q\nout: %q", err, in, out.Bytes())
		}
		if rest, _ := io.ReadAll(obr); len(rest) > 0 {
			t.Fatalf("forwarded bytes hold more than one request: %q left\nin:  %q\nout: %q", rest, in, out.Bytes())
		}
		if !bytes.Equal(body, sent.Bytes()) {
			t.Fatalf("body changed: read %q, forwarded %q\nin: %q", sent.Bytes(), body, in)
		}
		if got.Method != method || got.URL.RequestURI() != target || got.Host != host {
			t.Fatalf("request changed: %s %s host %q became %s %s host %q\nin:  %q\nout: %q",
				method, target, host, got.Method, got.URL.RequestURI(), got.Host, in, out.Bytes())
		}
		if r := CheckRaw(out.Bytes(), RawLimits{}); r != ReasonNone {
			t.Fatalf("forwarded bytes fail CheckRaw (%v)\nin:  %q\nout: %q", r, in, out.Bytes())
		}
		if v := got.Header["X-Forwarded-For"]; len(v) != 1 || v[0] != "192.0.2.1" {
			t.Fatalf("X-Forwarded-For %q\nin: %q", v, in)
		}
		for name := range got.Trailer {
			for _, bad := range forbiddenTrailers {
				if strings.EqualFold(name, bad) {
					t.Fatalf("forbidden trailer %q forwarded\nin:  %q\nout: %q", name, in, out.Bytes())
				}
			}
		}
	})
}

// responseSeeds are backend responses, ordinary and broken.
var responseSeeds = []string{
	"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
	"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\nX-T: 1\r\n\r\n",
	"HTTP/1.1 200 OK\r\n\r\nclose-delimited body",
	"HTTP/1.0 200 OK\r\nConnection: keep-alive\r\nContent-Length: 2\r\n\r\nok",
	"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n",
	"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n",
	"HTTP/1.1 304 Not Modified\r\nContent-Length: 10\r\n\r\n",
	"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nHTTP/1.1 200 OK\r\n\r\n",
	"HTTP/1.1 200 OK\r\nContent-Length: 3\r\nContent-Length: 4\r\n\r\nabcd",
	"HTTP/1.1 999 Odd\r\nConnection: Content-Length, X-A\r\nX-A: 1\r\nKeep-Alive: timeout=1\r\nContent-Length: 1\r\n\r\nx",
	"HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip, chunked\r\n\r\n0\r\n\r\n",
}

// FuzzResponse sends a backend's raw bytes, which may be hostile, through
// what the proxy does to a response: ReadResponse with its 1xx and 101
// handling, the Body wrapper, PrepareResponse and Response.Write. method
// picks the request it answers (bit 0: HEAD, bit 1: an HTTP/1.0 client).
// Whatever the backend sends, the client receives the interim responses
// and then exactly one well-framed response with the same status and the
// same body bytes, or, if the backend's body breaks mid-stream, a
// connection the proxy closes.
func FuzzResponse(f *testing.F) {
	for _, s := range responseSeeds {
		for m := range byte(4) {
			f.Add(m, []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, method byte, in []byte) {
		creq := &http.Request{Method: http.MethodGet, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
		if method&1 != 0 {
			creq.Method = http.MethodHead
		}
		if method&2 != 0 {
			creq.Proto, creq.ProtoMinor = "HTTP/1.0", 0
		}
		var out bytes.Buffer
		cw := bufio.NewWriter(&out)
		interim := 0
		resp, err := ReadResponse(bufio.NewReader(bytes.NewReader(in)), creq, func(r *http.Response) error {
			interim++
			return WriteInterim(cw, r)
		})
		if err != nil {
			// No final response: the proxy answers 502 itself.
			if errors.Is(err, ErrUnexpectedSwitch) && !bytes.Contains(in, []byte(" 101")) {
				t.Fatalf("101 refused for a response without one: %q", in)
			}
			return
		}
		status := resp.StatusCode
		var read bytes.Buffer // the body as the proxy read it
		if resp.Body != nil && resp.Body != http.NoBody {
			resp.Body = NewBody(io.TeeReader(resp.Body, &read), nil, 0)
		}
		keepAlive := PrepareResponse(resp, creq, true)
		werr := resp.Write(cw)
		if werr == nil {
			werr = cw.Flush()
		}
		if werr != nil {
			return // the backend's body broke: the client connection is closed
		}

		cbr := bufio.NewReader(bytes.NewReader(out.Bytes()))
		for i := range interim {
			r, err := http.ReadResponse(cbr, creq)
			if err != nil || r.StatusCode < 100 || r.StatusCode > 199 {
				t.Fatalf("interim response %d unreadable: %v\nin:  %q\nout: %q", i, err, in, out.Bytes())
			}
		}
		got, err := http.ReadResponse(cbr, creq)
		if err != nil {
			t.Fatalf("relayed response does not parse: %v\nin:  %q\nout: %q", err, in, out.Bytes())
		}
		body, err := io.ReadAll(got.Body)
		if err != nil {
			t.Fatalf("relayed body does not parse: %v\nin:  %q\nout: %q", err, in, out.Bytes())
		}
		if rest, _ := io.ReadAll(cbr); keepAlive && len(rest) > 0 {
			t.Fatalf("a kept-alive client would read %q as another response\nin:  %q\nout: %q", rest, in, out.Bytes())
		}
		if got.StatusCode != status {
			t.Fatalf("status %d relayed as %d\nin: %q", status, got.StatusCode, in)
		}
		if !bytes.Equal(body, read.Bytes()) {
			t.Fatalf("body changed: read %q, relayed %q\nin: %q", read.Bytes(), body, in)
		}
		if got.Close == keepAlive && creq.ProtoAtLeast(1, 1) {
			t.Fatalf("PrepareResponse said keep-alive %v but the response says close %v\nin:  %q\nout: %q", keepAlive, got.Close, in, out.Bytes())
		}
		for _, h := range hopByHop {
			if _, ok := got.Header[h]; ok && h != "Connection" {
				t.Fatalf("hop-by-hop %s relayed\nin:  %q\nout: %q", h, in, out.Bytes())
			}
		}
	})
}

var timeZero = time.Time{}

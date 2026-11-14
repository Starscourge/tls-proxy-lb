package httpx

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func readReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	return req
}

func TestCheckRequest(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"GET / HTTP/1.1\r\nHost: a.test\r\n\r\n", 0},
		{"GET / HTTP/1.0\r\n\r\n", 0},
		{"GET http://a.test/x HTTP/1.1\r\n\r\n", 0},
		{"GET / HTTP/1.1\r\n\r\n", http.StatusBadRequest},
		{"GET / HTTP/1.1\r\nHost:\r\n\r\n", http.StatusBadRequest},
		{"CONNECT a.test:443 HTTP/1.1\r\nHost: a.test:443\r\n\r\n", http.StatusMethodNotAllowed},
		{"PRI * HTTP/2.0\r\n\r\n", http.StatusHTTPVersionNotSupported},
	}
	for _, tc := range cases {
		got := CheckRequest(readReq(t, tc.raw)).Status()
		if got != tc.want {
			t.Errorf("%q: status %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestPrepareRequestWire serialises a prepared request and checks the exact
// headers the backend would see.
func TestPrepareRequestWire(t *testing.T) {
	req := readReq(t, "POST /p?q=1 HTTP/1.1\r\n"+
		"Host: a.test\r\n"+
		"Connection: keep-alive, X-Hop\r\n"+
		"X-Hop: secret\r\n"+
		"Keep-Alive: timeout=5\r\n"+
		"Upgrade: websocket\r\n"+
		"X-Forwarded-For: 203.0.113.9\r\n"+
		"X-Forwarded-Proto: https\r\n"+
		"Content-Length: 5\r\n\r\nhello")
	PrepareRequest(req, "192.0.2.1", "http", false)
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out, err := http.ReadRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, buf.String())
	}
	body, _ := io.ReadAll(out.Body)
	if out.Method != "POST" || out.URL.RequestURI() != "/p?q=1" || out.Host != "a.test" || string(body) != "hello" {
		t.Fatalf("request line/body changed: %s %s host=%s body=%q", out.Method, out.URL, out.Host, body)
	}
	for _, h := range []string{"X-Hop", "Keep-Alive", "Upgrade", "User-Agent"} {
		if _, ok := out.Header[h]; ok {
			t.Errorf("%s forwarded: %q", h, out.Header[h])
		}
	}
	if got := out.Header.Get("X-Forwarded-For"); got != "203.0.113.9, 192.0.2.1" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := out.Header.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
	if got := out.Header.Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
}

// With keep-alive the upstream request has no Connection header, even when
// the client asked to close its own connection.
func TestPrepareRequestKeepAlive(t *testing.T) {
	for _, raw := range []string{
		"GET / HTTP/1.1\r\nHost: a\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n",
		"GET / HTTP/1.0\r\n\r\n",
	} {
		req := readReq(t, raw)
		PrepareRequest(req, "192.0.2.1", "http", true)
		var buf bytes.Buffer
		if err := req.Write(&buf); err != nil {
			t.Fatal(err)
		}
		if s := buf.String(); strings.Contains(s, "Connection:") || !strings.Contains(s, " HTTP/1.1\r\n") {
			t.Errorf("%q forwarded as\n%s", raw, s)
		}
	}
}

func TestPrepareRequestKeepsUserAgent(t *testing.T) {
	req := readReq(t, "GET / HTTP/1.1\r\nHost: a\r\nUser-Agent: curl/8\r\n\r\n")
	PrepareRequest(req, "192.0.2.1", "http", false)
	var buf bytes.Buffer
	req.Write(&buf)
	if !strings.Contains(buf.String(), "User-Agent: curl/8\r\n") {
		t.Fatalf("User-Agent lost:\n%s", buf.String())
	}
}

func TestPrepareRequestChunkedStaysChunked(t *testing.T) {
	req := readReq(t, "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nTrailer: X-Sum\r\n\r\n"+
		"5\r\nhello\r\n0\r\nX-Sum: 42\r\n\r\n")
	PrepareRequest(req, "192.0.2.1", "http", false)
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out, err := http.ReadRequest(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(out.Body)
	if len(out.TransferEncoding) != 1 || string(body) != "hello" || out.Trailer.Get("X-Sum") != "42" {
		t.Fatalf("TE=%v body=%q trailer=%v\n%s", out.TransferEncoding, body, out.Trailer, buf.String())
	}
}

func readResp(t *testing.T, raw string, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func writeResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	if err := resp.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestPrepareResponse(t *testing.T) {
	get11 := readReq(t, "GET / HTTP/1.1\r\nHost: a\r\n\r\n")
	get10 := readReq(t, "GET / HTTP/1.0\r\n\r\n")
	head11 := readReq(t, "HEAD / HTTP/1.1\r\nHost: a\r\n\r\n")

	t.Run("close-delimited to HTTP/1.1 becomes chunked", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nstream", get11)
		if !PrepareResponse(resp, get11, true) {
			t.Fatal("keep-alive refused")
		}
		out := writeResp(t, resp)
		if !strings.HasPrefix(out, "HTTP/1.1 200 OK\r\n") || !strings.Contains(out, "Transfer-Encoding: chunked\r\n") ||
			strings.Contains(out, "Connection:") || !strings.HasSuffix(out, "6\r\nstream\r\n0\r\n\r\n") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("chunked to HTTP/1.0 becomes close-delimited", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nstream\r\n0\r\n\r\n", get10)
		if PrepareResponse(resp, get10, true) {
			t.Fatal("keep-alive allowed for close-delimited body")
		}
		out := writeResp(t, resp)
		if strings.Contains(out, "chunked") || !strings.Contains(out, "Connection: close\r\n") || !strings.HasSuffix(out, "\r\n\r\nstream") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("backend close does not close client", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok", get11)
		PrepareResponse(resp, get11, true)
		out := writeResp(t, resp)
		if strings.Contains(out, "Connection") || !strings.Contains(out, "Content-Length: 2\r\n") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("headers named in Connection are removed", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 200 OK\r\nConnection: keep-alive, X-Hop\r\nX-Hop: 1\r\nKeep-Alive: timeout=5\r\nContent-Length: 2\r\n\r\nok", get11)
		PrepareResponse(resp, get11, true)
		if out := writeResp(t, resp); strings.Contains(out, "X-Hop") || strings.Contains(out, "Keep-Alive") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("known limitation: ReadResponse drops Connection when it says close", func(t *testing.T) {
		// http.ReadResponse deletes the whole Connection header when it
		// contains "close", so headers it also names cannot be found and
		// are forwarded. Backends are trusted; see docs/DESIGN.md.
		resp := readResp(t, "HTTP/1.1 200 OK\r\nConnection: close, X-Hop\r\nX-Hop: 1\r\nContent-Length: 2\r\n\r\nok", get11)
		if _, ok := resp.Header["Connection"]; ok {
			t.Skip("ReadResponse now keeps Connection; strip named headers and remove this case")
		}
		PrepareResponse(resp, get11, true)
		if out := writeResp(t, resp); !strings.Contains(out, "X-Hop: 1") {
			t.Fatalf("behaviour changed:\n%q", out)
		}
	})
	t.Run("client close is announced", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", get11)
		PrepareResponse(resp, get11, false)
		if out := writeResp(t, resp); !strings.Contains(out, "Connection: close\r\n") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("HEAD keeps Content-Length and sends no body", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 200 OK\r\nContent-Length: 1234\r\n\r\n", head11)
		if !PrepareResponse(resp, head11, true) {
			t.Fatal("keep-alive refused for HEAD")
		}
		out := writeResp(t, resp)
		if !strings.Contains(out, "Content-Length: 1234\r\n") || !strings.HasSuffix(out, "\r\n\r\n") || strings.Contains(out, "chunked") {
			t.Fatalf("got:\n%q", out)
		}
	})
	t.Run("204 stays bodiless", func(t *testing.T) {
		resp := readResp(t, "HTTP/1.1 204 No Content\r\n\r\n", get11)
		if !PrepareResponse(resp, get11, true) {
			t.Fatal("keep-alive refused for 204")
		}
		if out := writeResp(t, resp); strings.Contains(out, "chunked") || strings.Contains(out, "Content-Length") {
			t.Fatalf("got:\n%q", out)
		}
	})
}

func TestWriteInterim(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	resp := &http.Response{StatusCode: 100, Status: "100 Continue", Header: http.Header{"Connection": {"keep-alive"}}}
	if err := WriteInterim(w, resp); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "HTTP/1.1 100 Continue\r\n\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteError(t *testing.T) {
	for _, head := range []bool{false, true} {
		var buf bytes.Buffer
		if err := WriteError(bufio.NewWriter(&buf), http.StatusBadGateway, head); err != nil {
			t.Fatal(err)
		}
		method := "GET"
		if head {
			method = "HEAD"
		}
		resp, err := http.ReadResponse(bufio.NewReader(&buf), &http.Request{Method: method})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 502 || !resp.Close {
			t.Fatalf("status %d close %v", resp.StatusCode, resp.Close)
		}
		if !head && string(body) != "502 Bad Gateway\n" {
			t.Fatalf("body %q", body)
		}
		if head && buf.Len() != 0 {
			t.Fatalf("HEAD error carried %d body bytes", buf.Len())
		}
	}
}

func TestWriteTooManyRequests(t *testing.T) {
	for _, tc := range []struct {
		keepAlive, head bool
	}{{false, false}, {true, false}, {true, true}, {false, true}} {
		var buf bytes.Buffer
		if err := WriteTooManyRequests(bufio.NewWriter(&buf), 17, tc.keepAlive, tc.head); err != nil {
			t.Fatal(err)
		}
		method := "GET"
		if tc.head {
			method = "HEAD"
		}
		br := bufio.NewReader(&buf)
		resp, err := http.ReadResponse(br, &http.Request{Method: method})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "17" || resp.Close == tc.keepAlive {
			t.Fatalf("%+v: status %d Retry-After %q close %v", tc, resp.StatusCode, resp.Header.Get("Retry-After"), resp.Close)
		}
		if resp.ContentLength != int64(len(tooManyBody)) {
			t.Fatalf("Content-Length %d", resp.ContentLength)
		}
		if !tc.head && string(body) != tooManyBody {
			t.Fatalf("body %q", body)
		}
		if br.Buffered() != 0 || buf.Len() != 0 {
			t.Fatalf("%+v: %d bytes after the response", tc, br.Buffered()+buf.Len())
		}
	}
}

func TestWriteTooManyRequestsAllocs(t *testing.T) {
	w := bufio.NewWriter(io.Discard)
	if n := testing.AllocsPerRun(1000, func() { WriteTooManyRequests(w, 3, true, false) }); n != 0 {
		t.Fatalf("allocates %v per call", n)
	}
}

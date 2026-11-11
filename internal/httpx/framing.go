package httpx

import (
	"bufio"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// CheckRequest returns the status to reject req with and a reason, or 0 if
// the proxy can forward it.
func CheckRequest(req *http.Request) (status int, reason string) {
	switch {
	case req.ProtoMajor != 1:
		return http.StatusHTTPVersionNotSupported, "only HTTP/1.x is supported"
	case req.Method == http.MethodConnect:
		return http.StatusMethodNotAllowed, "CONNECT is not supported"
	case req.ProtoAtLeast(1, 1) && req.Host == "":
		return http.StatusBadRequest, "missing Host header"
	}
	return 0, ""
}

// PrepareRequest rewrites a request read from a client for the upstream:
// hop-by-hop headers are removed, X-Forwarded-For and X-Forwarded-Proto set,
// and the connection marked to close after the response.
//
// Host, the request target, the framing (ContentLength, TransferEncoding)
// and trailers are left as parsed; Request.Write re-serialises them.
func PrepareRequest(req *http.Request, clientIP, proto string) {
	RemoveHopByHop(req.Header)
	SetForwarded(req.Header, clientIP, proto)
	// Request.Write adds "User-Agent: Go-http-client/1.1" when the header
	// is absent. An empty value suppresses that without sending a header.
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""}
	}
	// One upstream connection per request until Phase 4 adds reuse. Asking
	// the backend to close makes it the side that holds TIME_WAIT.
	req.Close = true
}

// BodyAllowed reports whether a response to a request with method can carry
// a body, by RFC 9110 section 6.4.1.
func BodyAllowed(method string, status int) bool {
	switch {
	case method == http.MethodHead:
		return false
	case status >= 100 && status < 200, status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	}
	return true
}

// PrepareResponse rewrites an upstream response for the client and returns
// whether the client connection can stay open afterwards.
//
// The response is sent as HTTP/1.1. A body of unknown length (one the
// backend ends by closing) is re-framed as chunked for HTTP/1.1 clients, so
// the client connection survives; HTTP/1.0 clients get it close-delimited.
// Close is set from keepAlive rather than from the backend's choice, which
// only concerns the upstream connection.
func PrepareResponse(resp *http.Response, clientReq *http.Request, keepAlive bool) bool {
	RemoveHopByHop(resp.Header)
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if BodyAllowed(clientReq.Method, resp.StatusCode) && resp.ContentLength < 0 {
		if clientReq.ProtoAtLeast(1, 1) {
			resp.TransferEncoding = []string{"chunked"}
		} else {
			resp.TransferEncoding = nil
			keepAlive = false
		}
	}
	resp.Close = !keepAlive
	return keepAlive
}

// WriteInterim writes a 1xx response (such as 100 Continue) to w and flushes.
// Response.Write is not used because it may add a Content-Length.
func WriteInterim(w *bufio.Writer, resp *http.Response) error {
	RemoveHopByHop(resp.Header)
	reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	if reason == "" {
		reason = http.StatusText(resp.StatusCode)
	}
	fmt.Fprintf(w, "HTTP/1.1 %03d %s\r\n", resp.StatusCode, reason)
	if err := resp.Header.Write(w); err != nil {
		return err
	}
	w.WriteString("\r\n")
	return w.Flush()
}

// WriteError writes a fixed error response that closes the connection, and
// flushes. head omits the body, for HEAD requests.
func WriteError(w *bufio.Writer, status int, head bool) error {
	body := fmt.Sprintf("%d %s\n", status, http.StatusText(status))
	fmt.Fprintf(w, "HTTP/1.1 %03d %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n\r\n",
		status, http.StatusText(status), len(body))
	if !head {
		w.WriteString(body)
	}
	return w.Flush()
}

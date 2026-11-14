package httpx

import (
	"bufio"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// CheckRequest returns why req, as parsed, cannot be forwarded, or
// ReasonNone: not HTTP/1.x, CONNECT, or an HTTP/1.1 request without Host.
func CheckRequest(req *http.Request) Reason {
	switch {
	case req.ProtoMajor != 1:
		return ReasonUnsupportedVersion
	case req.Method == http.MethodConnect:
		return ReasonConnect
	case req.ProtoAtLeast(1, 1) && req.Host == "":
		return ReasonMissingHost
	}
	return ReasonNone
}

// PrepareRequest rewrites a request read from a client for the upstream:
// hop-by-hop headers are removed and X-Forwarded-For and X-Forwarded-Proto
// set (SetForwarded). With keepAlive false the request asks the backend to close the
// connection after the response; with it true the request carries no
// Connection header, which in HTTP/1.1 means keep-alive, whatever the
// client asked of its own connection.
//
// Host, the request target, the framing (ContentLength, TransferEncoding)
// and trailers are left as parsed; Request.Write re-serialises them.
func PrepareRequest(req *http.Request, f Forwarding, keepAlive bool) {
	RemoveHopByHop(req.Header)
	SetForwarded(req.Header, f)
	// Request.Write adds "User-Agent: Go-http-client/1.1" when the header
	// is absent. An empty value suppresses that without sending a header.
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""}
	}
	// Without reuse, asking the backend to close makes it the side that
	// holds TIME_WAIT.
	req.Close = !keepAlive
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
	switch {
	case BodyAllowed(clientReq.Method, resp.StatusCode) && resp.ContentLength < 0:
		if clientReq.ProtoAtLeast(1, 1) {
			resp.TransferEncoding = []string{"chunked"}
		} else {
			resp.TransferEncoding = nil
			keepAlive = false
		}
	case !BodyAllowed(clientReq.Method, resp.StatusCode):
		// No body can follow (HEAD, 1xx, 204, 304), so a missing length
		// needs no close to mark its end. Response.Write would add
		// Connection: close for any HTTP/1.1 response with no length,
		// and the client would drop a connection the proxy keeps.
		// Uncompressed is the one field that check consults, and it is
		// read nowhere else on the write side. FuzzResponse found this.
		resp.Uncompressed = true
		// A 1xx or 204 must not carry Content-Length (RFC 9110 section
		// 8.6); Response.Write would send "Content-Length: 0" for one
		// answering a POST, PUT or PATCH. A HEAD or 304 response keeps
		// the length it was given, which describes the representation.
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode < 200 {
			resp.ContentLength = -1
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

// tooManyBody is the body of a 429 response.
const tooManyBody = "429 Too Many Requests\n"

// WriteTooManyRequests writes a 429 response with a Retry-After of
// retryAfter seconds, and flushes. keepAlive leaves the connection open
// (no Connection: close); head omits the body. It does not allocate, since
// an over-limit client can ask for it as fast as it likes.
func WriteTooManyRequests(w *bufio.Writer, retryAfter int, keepAlive, head bool) error {
	w.WriteString("HTTP/1.1 429 Too Many Requests\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: 22\r\n" + // len(tooManyBody)
		"Retry-After: ")
	w.Write(strconv.AppendInt(w.AvailableBuffer(), int64(retryAfter), 10))
	if keepAlive {
		w.WriteString("\r\n\r\n")
	} else {
		w.WriteString("\r\nConnection: close\r\n\r\n")
	}
	if !head {
		w.WriteString(tooManyBody)
	}
	return w.Flush()
}

// WriteRedirect writes a redirect to location that closes the connection,
// and flushes. head omits the body. location must already be a valid URL;
// it is written as is.
func WriteRedirect(w *bufio.Writer, status int, location string, head bool) error {
	body := fmt.Sprintf("%d %s: %s\n", status, http.StatusText(status), location)
	fmt.Fprintf(w, "HTTP/1.1 %03d %s\r\n"+
		"Location: %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n\r\n",
		status, http.StatusText(status), location, len(body))
	if !head {
		w.WriteString(body)
	}
	return w.Flush()
}

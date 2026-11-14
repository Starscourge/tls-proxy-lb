package httpx

import (
	"bufio"
	"errors"
	"net/http"
)

// RequestLimits bound what ReadRequest accepts. Zero means no limit.
type RequestLimits struct {
	RawLimits
	// MaxBodyBytes refuses a request whose Content-Length is larger. A
	// chunked body is held to it as it streams (Body.SetLimits).
	MaxBodyBytes int64
}

// ReadRequest reads the next request from br and decides whether it may be
// forwarded. br must read from lr, which the caller has already limited to
// the header size (HeaderLimitReader.Limit) and which ReadRequest unlimits
// once the header block is read. After an error lr stays limited, so
// lr.Exhausted still tells a block that was too large. raw is a buffer reused across requests;
// it is left holding the header block as it arrived.
//
// It returns the parsed request and ReasonNone; or the request and why it
// is refused, the caller answering with the reason's Status and closing
// the connection; or a nil request and the error from http.ReadRequest
// (malformed, too large: lr.Exhausted, timed out, or the connection
// closed), which the caller interprets.
//
// The checks, in order: http.ReadRequest's own; CheckRequest; CheckRaw on
// the bytes as they arrived; a valid Host; the Content-Length against
// MaxBodyBytes.
func ReadRequest(br *bufio.Reader, lr *HeaderLimitReader, raw *[]byte, lim RequestLimits) (*http.Request, Reason, error) {
	// What br already holds came through lr before recording started:
	// the start of this request, read ahead with the last one, or the
	// first byte the caller peeked at.
	buffered, _ := br.Peek(br.Buffered())
	*raw = append((*raw)[:0], buffered...)
	lr.Record(raw)
	req, err := http.ReadRequest(br)
	if err != nil {
		lr.Record(nil)
		return nil, ReasonNone, err
	}
	lr.Unlimit()
	if r := CheckRequest(req); r != ReasonNone {
		return req, r, nil
	}
	if r := CheckRaw(*raw, lim.RawLimits); r != ReasonNone {
		return req, r, nil
	}
	if !validTarget(req) {
		return req, ReasonBadTarget, nil
	}
	if req.Host != "" && !ValidHost(req.Host) {
		return req, ReasonInvalidHost, nil
	}
	if lim.MaxBodyBytes > 0 && req.ContentLength > lim.MaxBodyBytes {
		return req, ReasonBodyTooLarge, nil
	}
	return req, ReasonNone, nil
}

// validTarget reports whether req's target is one of the forms a proxy
// forwards (RFC 9112 section 3.2): origin-form, absolute-form with an
// http or https scheme and a host, or asterisk-form for OPTIONS.
// Authority-form is CONNECT's, which is refused anyway.
func validTarget(req *http.Request) bool {
	u := req.URL
	switch {
	case req.RequestURI == "*":
		return req.Method == http.MethodOptions
	case len(req.RequestURI) > 0 && req.RequestURI[0] == '/':
		return u.Scheme == "" && u.Host == "" && u.Opaque == ""
	default:
		return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Opaque == ""
	}
}

// forbiddenTrailers are fields a trailer must not carry (RFC 9110 section
// 6.5.1): framing, routing, and hop-by-hop fields. net/http accepts any
// field in a chunked body's trailer, and Request.Write sends the trailer
// map on, so a backend that merges trailers into the header could be told
// a second Content-Length or Host after the fact.
var forbiddenTrailers = []string{
	"Content-Length", "Transfer-Encoding", "Host", "Trailer",
	"Connection", "Proxy-Connection", "Keep-Alive", "Te", "Upgrade",
	"Content-Encoding", "Content-Type", "Content-Range",
	"Authorization", "Proxy-Authorization", "Proxy-Authenticate", "Expect",
}

// SanitizeTrailer deletes the fields a trailer must not carry. t is
// http.Request.Trailer; nil is fine.
func SanitizeTrailer(t http.Header) {
	if len(t) == 0 {
		return
	}
	for _, name := range forbiddenTrailers {
		delete(t, name)
	}
}

// ErrUnexpectedSwitch is returned by ReadResponse for a 101 Switching
// Protocols: the proxy strips Upgrade from every request, so a backend
// that switches is broken or hostile.
var ErrUnexpectedSwitch = errors.New("httpx: unexpected 101 Switching Protocols")

// ReadResponse reads a backend's response to req from br. Interim (1xx)
// responses before it are passed to interim, which relays them to the
// client; an error from interim ends the read with that error. A 101 is
// refused with ErrUnexpectedSwitch.
func ReadResponse(br *bufio.Reader, req *http.Request, interim func(*http.Response) error) (*http.Response, error) {
	for {
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols:
			return nil, ErrUnexpectedSwitch
		case resp.StatusCode >= 100 && resp.StatusCode < 200:
			if err := interim(resp); err != nil {
				return nil, err
			}
		default:
			return resp, nil
		}
	}
}

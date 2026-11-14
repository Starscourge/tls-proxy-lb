package httpx

import (
	"bytes"
	"net/http"
)

// Reason says why the proxy refused a request it read. Each has a fixed
// name, used in the access log and as the reason label of
// proxy_bad_requests_total.
type Reason uint8

// The reasons. ReasonNone means the request may be forwarded.
const (
	ReasonNone Reason = iota
	// ReasonMalformed: http.ReadRequest could not parse the request.
	ReasonMalformed
	// ReasonHeaderTooLarge: the header block is over max_header_bytes.
	ReasonHeaderTooLarge
	// ReasonTooManyHeaders: more header fields than max_header_count.
	ReasonTooManyHeaders
	// ReasonRequestLineTooLong: the request line is over
	// max_request_line.
	ReasonRequestLineTooLong
	// ReasonUnsupportedVersion: not HTTP/1.x.
	ReasonUnsupportedVersion
	// ReasonConnect: CONNECT, which a reverse proxy does not tunnel.
	ReasonConnect
	// ReasonMissingHost: an HTTP/1.1 request without Host.
	ReasonMissingHost
	// ReasonInvalidHost: a Host (or absolute-form target) with bytes no
	// host may have.
	ReasonInvalidHost
	// ReasonCLAndTE: both Content-Length and Transfer-Encoding.
	ReasonCLAndTE
	// ReasonDuplicateCL: more than one Content-Length field, even with the
	// same value.
	ReasonDuplicateCL
	// ReasonBadTE: a Transfer-Encoding other than one "chunked", or more
	// than one Transfer-Encoding field.
	ReasonBadTE
	// ReasonTEInHTTP10: Transfer-Encoding on an HTTP/1.0 request, which
	// net/http ignores, leaving the chunked body on the connection.
	ReasonTEInHTTP10
	// ReasonSpaceBeforeColon: whitespace between a field name and its
	// colon, which net/http accepts for a name it then does not
	// recognise.
	ReasonSpaceBeforeColon
	// ReasonBareLF: a line ending in LF without CR.
	ReasonBareLF
	// ReasonBareCR: a CR not followed by LF.
	ReasonBareCR
	// ReasonFoldedHeader: a line starting with whitespace, continuing the
	// field before it (obs-fold).
	ReasonFoldedHeader
	// ReasonBodyTooLarge: a Content-Length over max_body_bytes.
	ReasonBodyTooLarge
	NumReasons
)

var reasonNames = [NumReasons]string{
	"", "malformed", "header_too_large", "too_many_headers", "request_line_too_long",
	"unsupported_version", "connect", "missing_host", "invalid_host",
	"cl_and_te", "duplicate_cl", "bad_te", "te_in_http10",
	"space_before_colon", "bare_lf", "bare_cr", "folded_header", "body_too_large",
}

func (r Reason) String() string {
	if r < NumReasons {
		return reasonNames[r]
	}
	return "unknown"
}

// ReasonNames returns the name of every reason but ReasonNone, in order.
func ReasonNames() []string { return append([]string(nil), reasonNames[1:]...) }

// Status returns the response status for a request refused for r.
func (r Reason) Status() int {
	switch r {
	case ReasonNone:
		return 0
	case ReasonHeaderTooLarge, ReasonTooManyHeaders:
		return http.StatusRequestHeaderFieldsTooLarge
	case ReasonRequestLineTooLong:
		return http.StatusRequestURITooLong
	case ReasonUnsupportedVersion:
		return http.StatusHTTPVersionNotSupported
	case ReasonConnect:
		return http.StatusMethodNotAllowed
	case ReasonBodyTooLarge:
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// RawLimits are the limits CheckRaw enforces. Zero means no limit.
type RawLimits struct {
	MaxHeaderCount int
	MaxRequestLine int
}

// CheckRaw checks a request's header block as it arrived, before
// net/http normalised it, for the framing ambiguities that let a proxy and
// a backend disagree about where a request ends (docs/SMUGGLING.md). raw
// starts with the request line and holds at least the whole header block;
// anything after the blank line that ends it is ignored. It is meant for a
// block http.ReadRequest has already accepted, and checks only what that
// leaves open:
//
//   - every line ends in CRLF; no bare LF, no CR elsewhere;
//   - no field line starts with whitespace (obs-fold);
//   - no whitespace between a field name and its colon;
//   - at most one Content-Length, and never both it and
//     Transfer-Encoding;
//   - at most one Transfer-Encoding, whose value is "chunked" (any case,
//     optional whitespace), and none on an HTTP/1.0 request;
//   - the request line and the field count within lim.
//
// It allocates nothing.
func CheckRaw(raw []byte, lim RawLimits) Reason {
	var (
		first          = true
		ended          bool
		http10         bool
		fields, cl, te int
		teOK           = true
	)
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			// No end to the block in what was read: net/http would not
			// have accepted it.
			return ReasonMalformed
		}
		if i == 0 || raw[i-1] != '\r' {
			return ReasonBareLF
		}
		line := raw[:i-1]
		raw = raw[i+1:]
		if bytes.IndexByte(line, '\r') >= 0 {
			return ReasonBareCR
		}
		if first {
			first = false
			if lim.MaxRequestLine > 0 && len(line) > lim.MaxRequestLine {
				return ReasonRequestLineTooLong
			}
			http10 = bytes.HasSuffix(line, []byte(" HTTP/1.0"))
			continue
		}
		if len(line) == 0 {
			ended = true // the blank line that ends the header block
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return ReasonFoldedHeader
		}
		fields++
		if lim.MaxHeaderCount > 0 && fields > lim.MaxHeaderCount {
			return ReasonTooManyHeaders
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return ReasonMalformed
		}
		name := line[:colon]
		if c := name[len(name)-1]; c == ' ' || c == '\t' {
			return ReasonSpaceBeforeColon
		}
		switch {
		case equalFold(name, "Content-Length"):
			cl++
		case equalFold(name, "Transfer-Encoding"):
			te++
			if !equalFold(trimOWS(line[colon+1:]), "chunked") {
				teOK = false
			}
		}
	}
	switch {
	case !ended:
		return ReasonMalformed
	case cl > 0 && te > 0:
		return ReasonCLAndTE
	case cl > 1:
		return ReasonDuplicateCL
	case te > 1 || !teOK:
		return ReasonBadTE
	case te > 0 && http10:
		return ReasonTEInHTTP10
	}
	return ReasonNone
}

// equalFold reports whether b equals the ASCII string s, ignoring case.
func equalFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range len(b) {
		c, d := b[i], s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if 'A' <= d && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}

// trimOWS removes leading and trailing spaces and tabs.
func trimOWS(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// validHostByte is the set of bytes a Host value may hold: a reg-name, an
// IPv4 address, a bracketed IPv6 literal or a percent-encoded name, and a
// port. It is the table net/http's own server uses (httpguts), which
// http.ReadRequest does not apply.
var validHostByte = func() (t [256]bool) {
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
		t[c-'a'+'A'] = true
	}
	for _, c := range "!$%&'()*+,-.:;=[]_~" {
		t[c] = true
	}
	return t
}()

// ValidHost reports whether host may be a request's Host: non-empty (an
// HTTP/1.0 request may have none, which the caller allows separately) and
// made only of bytes a host and port may have. Spaces, controls, '/', '@'
// and non-ASCII bytes are not.
func ValidHost(host string) bool {
	if host == "" {
		return false
	}
	for i := range len(host) {
		if !validHostByte[host[i]] {
			return false
		}
	}
	return true
}

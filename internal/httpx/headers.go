// Package httpx holds the HTTP/1.1 framing and header rules the proxy applies
// around net/http's parsers and serialisers: hop-by-hop header removal,
// forwarding headers, request and response preparation, body wrappers that
// keep deadlines moving and never drain on Close, and fixed error replies.
package httpx

import (
	"net/http"
	"net/textproto"
	"strings"
)

// hopByHop lists the headers that apply to one connection only (RFC 9110
// section 7.6.1, plus the de facto Proxy-Connection and Keep-Alive).
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// RemoveHopByHop deletes the hop-by-hop headers from h, including any header
// named in a Connection header.
func RemoveHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for name := range strings.SplitSeq(v, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		delete(h, name)
	}
}

// SetForwarded appends clientIP to X-Forwarded-For, joining any values the
// client sent into one header, and sets X-Forwarded-Proto to proto.
//
// Appending trusts nothing: the client-supplied part is passed on as is and
// the address the proxy saw is always last. Phase 6 and 8 tighten this for
// untrusted clients.
func SetForwarded(h http.Header, clientIP, proto string) {
	if prior := h["X-Forwarded-For"]; len(prior) > 0 {
		h["X-Forwarded-For"] = []string{strings.Join(prior, ", ") + ", " + clientIP}
	} else {
		h["X-Forwarded-For"] = []string{clientIP}
	}
	h["X-Forwarded-Proto"] = []string{proto}
}

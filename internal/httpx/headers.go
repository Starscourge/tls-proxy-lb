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

// Forwarding is what the proxy tells a backend about the client.
type Forwarding struct {
	// ClientIP is the address the request came from.
	ClientIP string
	// Proto is "http" or "https", the scheme the client used.
	Proto string
	// Trusted is set when the peer is a trusted proxy (trusted_proxies),
	// whose own X-Forwarded-For is passed on.
	Trusted bool
}

// SetForwarded sets X-Forwarded-For and X-Forwarded-Proto. From a trusted
// proxy, ClientIP is appended to the X-Forwarded-For it sent, its header
// lines joined into one. From anyone else, X-Forwarded-For is replaced by
// ClientIP alone, and Forwarded is removed: a client can write any address
// it likes into either, and a backend reading the leftmost entry would
// believe it. X-Forwarded-Proto is always overwritten.
func SetForwarded(h http.Header, f Forwarding) {
	if prior := h["X-Forwarded-For"]; f.Trusted && len(prior) > 0 {
		h["X-Forwarded-For"] = []string{strings.Join(prior, ", ") + ", " + f.ClientIP}
	} else {
		h["X-Forwarded-For"] = []string{f.ClientIP}
	}
	if !f.Trusted {
		delete(h, "Forwarded")
	}
	h["X-Forwarded-Proto"] = []string{f.Proto}
}

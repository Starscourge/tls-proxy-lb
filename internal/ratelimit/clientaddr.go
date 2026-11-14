package ratelimit

import (
	"net/netip"
	"strings"
)

// Trusted is the set of proxies whose X-Forwarded-For is believed, as
// prefixes. A client can write any X-Forwarded-For it likes, so the header
// says nothing unless the connection comes from a proxy that sets it.
type Trusted []netip.Prefix

// Contains reports whether a is inside one of the prefixes. IPv4-mapped
// IPv6 addresses match IPv4 prefixes.
func (t Trusted) Contains(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr returns the address a request is attributed to. It is peer,
// the address the connection came from, unless peer is a trusted proxy.
// Then it is taken from xff, the X-Forwarded-For header lines in order:
// walking from the right, each trusted address is a proxy that forwarded
// the request, and the first untrusted one is the client. If every address
// is trusted, the leftmost is the client. A header that is missing, empty,
// or holds an entry that is not an address before the client is found
// gives peer, so a broken chain never lets the client choose its key.
//
// Entries may carry a port ("192.0.2.1:5678", "[2001:db8::1]:443"), which is
// ignored. ClientAddr does not allocate for IPv4 or zoneless IPv6 entries.
func ClientAddr(peer netip.Addr, xff []string, trusted Trusted) netip.Addr {
	peer = peer.Unmap()
	if len(xff) == 0 || !trusted.Contains(peer) {
		return peer
	}
	var leftmost netip.Addr
	for i := len(xff) - 1; i >= 0; i-- {
		line := xff[i]
		for line != "" {
			var elem string
			if j := strings.LastIndexByte(line, ','); j >= 0 {
				elem, line = line[j+1:], line[:j]
			} else {
				elem, line = line, ""
			}
			elem = strings.Trim(elem, " \t")
			if elem == "" {
				continue // "a,,b" or a trailing comma
			}
			a, ok := parseHop(elem)
			if !ok {
				return peer
			}
			if !trusted.Contains(a) {
				return a
			}
			leftmost = a
		}
	}
	if !leftmost.IsValid() {
		return peer
	}
	return leftmost
}

// parseHop parses one X-Forwarded-For entry: an address, with or without a
// port. The result is unmapped.
func parseHop(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

// ParseTrusted parses a list of addresses and CIDR prefixes. A bare address
// is a single-address prefix. Prefixes are masked, so "10.0.0.1/24" means
// 10.0.0.0/24.
func ParseTrusted(list []string) (Trusted, error) {
	out := make(Trusted, 0, len(list))
	for _, s := range list {
		var p netip.Prefix
		if strings.IndexByte(s, '/') >= 0 {
			pp, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, err
			}
			p = pp.Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, err
			}
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		out = append(out, p)
	}
	return out, nil
}

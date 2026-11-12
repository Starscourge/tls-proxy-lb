package lb

import (
	"cmp"
	"net/textproto"
	"slices"
	"strconv"
)

// DefaultVirtualNodes is the number of points each backend gets on the
// ring. The spread it gives is measured in ring_test.go and recorded in
// docs/DESIGN.md.
const DefaultVirtualNodes = 160

// RingBalancer is a consistent-hash ring. Each backend is placed at
// DefaultVirtualNodes points, derived from its address, and a request goes to
// the first point at or after its key's hash, walking clockwise.
//
// Points depend only on backend addresses, never on their order, so the same
// set of addresses gives the same ring on every instance and after every
// reload. Removing a backend moves only the keys that were on it.
//
// A point whose backend is not in the healthy set is skipped, so those keys
// move to the next backend on the ring and no other key moves. That gives
// the same result as rebuilding the ring without the backend.
type RingBalancer struct {
	hashes []uint64   // sorted
	nodes  []*Backend // nodes[i] owns hashes[i]
	header string     // canonical header name; empty means key on client IP
}

type ringPoint struct {
	hash uint64
	b    *Backend
}

// NewRingBalancer builds a ring over backends with vnodes points each. If
// header is not empty, requests are keyed on that header's first value, and
// on the client IP when the header is missing or empty.
func NewRingBalancer(backends []*Backend, vnodes int, header string) *RingBalancer {
	if vnodes <= 0 {
		vnodes = DefaultVirtualNodes
	}
	pts := make([]ringPoint, 0, len(backends)*vnodes)
	for _, b := range backends {
		for i := range vnodes {
			pts = append(pts, ringPoint{Hash64(b.Addr + "#" + strconv.Itoa(i)), b})
		}
	}
	// A tie between two points (vanishingly rare with 64 bits) is broken by
	// address, so the ring never depends on the order backends were listed.
	slices.SortFunc(pts, func(x, y ringPoint) int {
		return cmp.Or(cmp.Compare(x.hash, y.hash), cmp.Compare(x.b.Addr, y.b.Addr))
	})
	r := &RingBalancer{
		hashes: make([]uint64, len(pts)),
		nodes:  make([]*Backend, len(pts)),
	}
	if header != "" {
		r.header = textproto.CanonicalMIMEHeaderKey(header)
	}
	for i, p := range pts {
		r.hashes[i], r.nodes[i] = p.hash, p.b
	}
	return r
}

// Key returns the string a request is hashed on.
func (r *RingBalancer) Key(req *RequestInfo) string {
	if r.header != "" && req.Header != nil {
		// Indexing the map with the pre-canonicalised name avoids
		// Header.Get, which canonicalises (and may allocate) on every call.
		if v := req.Header[r.header]; len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return req.ClientIP
}

// Pick returns the backend that owns the request's key.
func (r *RingBalancer) Pick(req *RequestInfo, healthy []*Backend) *Backend {
	return r.Lookup(Hash64(r.Key(req)), healthy)
}

// Lookup returns the first backend in healthy at or after hash h on the
// ring, or nil if no ring backend is in healthy.
func (r *RingBalancer) Lookup(h uint64, healthy []*Backend) *Backend {
	n := len(r.hashes)
	if n == 0 || len(healthy) == 0 {
		return nil
	}
	i, _ := slices.BinarySearch(r.hashes, h)
	for j := range n {
		b := r.nodes[(i+j)%n]
		if slices.Contains(healthy, b) {
			return b
		}
	}
	return nil
}

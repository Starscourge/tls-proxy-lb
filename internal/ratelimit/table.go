package ratelimit

import (
	"hash/maphash"
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// NumShards is how many shards a Table has. The count is set by how
	// long a sweep holds one shard's lock, not by request contention: a
	// request holds a lock for well under a microsecond, but the sweep walks
	// a whole shard, and at a million entries a shard of 256 holds about
	// 4,000. It is also the smallest MaxEntries, one entry per shard.
	NumShards = 256
	// DefaultMaxEntries is the table cap used when Options set none. A
	// bucket costs about 126 bytes at the cap (see TestMillionKeysMemory),
	// so this is about 13 MB. Only clients seen within one refill time
	// (burst/rate) hold a bucket, since full ones are swept.
	DefaultMaxEntries = 100_000
	// evictSample is how many entries a full shard looks at to choose the
	// least recently used one to evict.
	evictSample = 8
	// shrinkFactor: a sweep rebuilds a shard's map once its entry count
	// falls below 1/shrinkFactor of its peak, since Go maps never give back
	// memory on delete.
	shrinkFactor = 4
	// shrinkMin is the smallest peak worth rebuilding a map for.
	shrinkMin = 64
)

// Options configure a Table.
type Options struct {
	// MaxEntries caps the number of buckets. It is split evenly across
	// the shards, so it is rounded down to a multiple of NumShards, and at
	// least NumShards. Zero means DefaultMaxEntries.
	MaxEntries int
	// SweepInterval is how often the sweeper drops buckets that have
	// refilled to full. Zero runs no sweeper; tests call Sweep themselves.
	SweepInterval time.Duration
	// Clock tells the time. Nil means SystemClock.
	Clock Clock
}

// key identifies one bucket: a route and a client address, the latter in
// KeyAddr form as 16 bytes.
type key struct {
	route uint32
	addr  [16]byte
}

// shard is one lock and the buckets behind it, padded to a cache line so
// two shards' locks never share one.
type shard struct {
	mu sync.Mutex
	m  map[key]bucket
	// peak is the most entries m has held since it was made.
	peak int
	_    [64 - 8 - 8 - 8]byte
}

// Table holds every client's token buckets, for every route. It is shared by
// all connection goroutines; each shard has its own mutex, and a request
// takes one shard's lock once.
//
// Goroutine ownership: New starts at most one sweeper goroutine, which Close
// stops and waits for.
type Table struct {
	clock    Clock
	epoch    time.Time
	seed     maphash.Seed
	perShard int
	shards   [NumShards]shard

	entries   atomic.Int64
	evictions atomic.Uint64

	mu     sync.Mutex
	routes map[string]uint32
	// caps holds each route's bucket capacity, by route ID, as its latest
	// Limiter set it; the sweeper and eviction read it to tell a full
	// bucket. Copied on write, which happens only when a snapshot is built.
	caps atomic.Pointer[[]int64]

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New returns an empty table and starts its sweeper, if o asks for one.
func New(o Options) *Table {
	if o.Clock == nil {
		o.Clock = SystemClock{}
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	t := &Table{
		clock:    o.Clock,
		epoch:    o.Clock.Now(),
		seed:     maphash.MakeSeed(),
		perShard: max(o.MaxEntries/NumShards, 1),
		routes:   make(map[string]uint32),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for i := range t.shards {
		t.shards[i].m = make(map[key]bucket)
	}
	if o.SweepInterval > 0 {
		go t.sweeper(o.SweepInterval)
	} else {
		close(t.done)
	}
	return t
}

// Close stops the sweeper and waits for it. The table stays usable.
func (t *Table) Close() {
	t.closeOnce.Do(func() { close(t.stop) })
	<-t.done
}

func (t *Table) sweeper(every time.Duration) {
	defer close(t.done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.Sweep()
		case <-t.stop:
			return
		}
	}
}

// MaxEntries returns the most buckets the table holds.
func (t *Table) MaxEntries() int { return t.perShard * NumShards }

// Len returns the number of buckets held.
func (t *Table) Len() int { return int(t.entries.Load()) }

// Evictions returns how many buckets were dropped to make room before they
// had refilled, losing their state.
func (t *Table) Evictions() uint64 { return t.evictions.Load() }

// now returns nanoseconds since the table's epoch.
func (t *Table) now() int64 { return int64(t.clock.Now().Sub(t.epoch)) }

// Limiter returns the limiter for one route with limit l, or nil if l is not
// enabled; a nil *Limiter allows everything. Limiters for the same route
// name share buckets, so a snapshot rebuilt on reload keeps every client's
// state, under the new limit.
func (t *Table) Limiter(route string, l Limit) *Limiter {
	if !l.Enabled() {
		return nil
	}
	p := l.params()
	t.mu.Lock()
	id, ok := t.routes[route]
	if !ok {
		// Route IDs are never reused; there is one per host name ever
		// configured, which reloads keep small.
		id = uint32(len(t.routes))
		t.routes[route] = id
	}
	var caps []int64
	if old := t.caps.Load(); old != nil {
		caps = append(caps, *old...)
	}
	if int(id) >= len(caps) {
		caps = append(caps, make([]int64, int(id)+1-len(caps))...)
	}
	caps[id] = p.capacity
	t.caps.Store(&caps)
	t.mu.Unlock()
	return &Limiter{t: t, route: id, p: p, limit: l}
}

// Limiter applies one route's limit to each client. It is immutable and
// safe for concurrent use.
type Limiter struct {
	t     *Table
	route uint32
	p     params
	limit Limit
}

// Limit returns the limiter's settings.
func (l *Limiter) Limit() Limit { return l.limit }

// Allow spends one request from addr's bucket. If the bucket is empty it
// returns false and how long until it will hold a request. A nil Limiter
// allows everything.
func (l *Limiter) Allow(addr netip.Addr) (ok bool, wait time.Duration) {
	if l == nil {
		return true, 0
	}
	t := l.t
	k := key{route: l.route, addr: KeyAddr(addr).As16()}
	now := t.now()
	s := &t.shards[maphash.Comparable(t.seed, k)%NumShards]
	s.mu.Lock()
	b, found := s.m[k]
	if !found {
		if len(s.m) >= t.perShard {
			t.evictLocked(s, now, *t.caps.Load())
		}
		b = fullBucket(now, l.p)
	}
	ok, wait = b.take(now, l.p)
	s.m[k] = b
	if !found {
		s.peak = max(s.peak, len(s.m))
	}
	s.mu.Unlock()
	if !found {
		t.entries.Add(1)
	}
	return ok, wait
}

// evictLocked removes one bucket from the full shard s: the first of a few
// sampled that has refilled to full, else the least recently used of them.
// Go randomises where map iteration starts, so the sample is spread over the
// shard. s.mu is held.
func (t *Table) evictLocked(s *shard, now int64, caps []int64) {
	var victim key
	oldest := int64(math.MaxInt64)
	full := false
	n := 0
	for k, b := range s.m {
		if b.fullAt(now, caps[k.route]) {
			victim, full = k, true
			break
		}
		if b.last < oldest {
			victim, oldest = k, b.last
		}
		if n++; n == evictSample {
			break
		}
	}
	delete(s.m, victim)
	t.entries.Add(-1)
	if !full {
		t.evictions.Add(1)
	}
}

// Sweep drops every bucket that has refilled to full, and returns how many it
// dropped. It locks one shard at a time.
func (t *Table) Sweep() int {
	cp := t.caps.Load()
	if cp == nil {
		return 0 // no limiter yet, so no buckets
	}
	caps := *cp
	now := t.now()
	removed := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n := 0
		for k, b := range s.m {
			if b.fullAt(now, caps[k.route]) {
				delete(s.m, k)
				n++
			}
		}
		if s.peak >= shrinkMin && len(s.m) < s.peak/shrinkFactor {
			m := make(map[key]bucket, len(s.m))
			for k, b := range s.m {
				m[k] = b
			}
			s.m, s.peak = m, len(m)
		}
		s.mu.Unlock()
		t.entries.Add(int64(-n))
		removed += n
	}
	return removed
}

// KeyAddr returns the address a client is limited by: an IPv4 address as
// is (IPv4-mapped IPv6 addresses unmapped), and an IPv6 address as its /64
// prefix with the zone dropped, since one host typically holds a whole /64
// and could rotate through it.
func KeyAddr(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if a.Is4() || !a.IsValid() {
		return a
	}
	b := a.As16()
	clear(b[8:])
	return netip.AddrFrom16(b)
}

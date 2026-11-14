package ratelimit

import (
	"context"
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
	// bucket costs about 115 bytes at the cap (see TestMillionKeysMemory),
	// so this is about 12 MB. Only clients seen within one refill time
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
//
// The lock guards the map, not the buckets: a request for a client the
// shard already holds takes only the read lock, and spends from the bucket
// with compare-and-swap. The write lock is for adding a bucket, evicting
// one and sweeping, which also keeps a sweep from deleting a bucket while
// a request is spending from it.
type shard struct {
	mu sync.RWMutex
	m  map[key]*bucket
	// peak is the most entries m has held since it was made.
	peak int
	_    [64 - 24 - 8 - 8]byte
}

// Table holds every client's token buckets, for every route. It is shared by
// all connection goroutines. Each shard has its own lock, and a request for
// a known client only reads under it, so one busy client's requests do not
// queue behind each other: they meet only at their bucket's
// compare-and-swap.
//
// Goroutine ownership: New starts at most one sweeper goroutine, which Close
// stops and waits for; NewContext's also stops when its context ends.
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

	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
}

// New returns an empty table and starts its sweeper, if o asks for one.
func New(o Options) *Table { return NewContext(context.Background(), o) }

// NewContext is New with a sweeper that also stops when ctx ends. Close
// must still be called, to wait for it.
func NewContext(ctx context.Context, o Options) *Table {
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
		done:     make(chan struct{}),
	}
	t.ctx, t.stop = context.WithCancel(ctx)
	for i := range t.shards {
		t.shards[i].m = make(map[key]*bucket)
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
	t.stop()
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
		case <-t.ctx.Done():
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
	t.mu.Lock()
	id, ok := t.routes[route]
	if !ok {
		// Route IDs are never reused; there is one per host name ever
		// configured, which reloads keep small.
		id = uint32(len(t.routes))
		t.routes[route] = id
	}
	t.mu.Unlock()
	return &Limiter{t: t, route: id, p: l.params(), limit: l}
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
	s.mu.RLock()
	if b := s.m[k]; b != nil {
		ok, wait = b.take(now, l.p)
		s.mu.RUnlock()
		return ok, wait
	}
	s.mu.RUnlock()

	s.mu.Lock()
	b := s.m[k]
	if b == nil {
		// Still missing now that the lock is exclusive: add it.
		if len(s.m) >= t.perShard {
			t.evictLocked(s, now)
		}
		b = newBucket(now)
		s.m[k] = b
		s.peak = max(s.peak, len(s.m))
		t.entries.Add(1)
	}
	ok, wait = b.take(now, l.p)
	s.mu.Unlock()
	return ok, wait
}

// evictLocked removes one bucket from the full shard s: the first of a few
// sampled that has refilled to full, else the one of them closest to full,
// whose loss forgives the least. Go randomises where map iteration starts,
// so the sample is spread over the shard. s.mu is held for writing.
func (t *Table) evictLocked(s *shard, now int64) {
	var victim key
	nearest := int64(math.MaxInt64)
	full := false
	n := 0
	for k, b := range s.m {
		f := b.full.Load()
		if f <= now {
			victim, full = k, true
			break
		}
		if f < nearest {
			victim, nearest = k, f
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
	now := t.now()
	removed := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n := 0
		for k, b := range s.m {
			if b.fullAt(now) {
				delete(s.m, k)
				n++
			}
		}
		if s.peak >= shrinkMin && len(s.m) < s.peak/shrinkFactor {
			m := make(map[key]*bucket, len(s.m))
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

// Package ratelimit implements per-client token buckets.
//
// A bucket refills lazily when it is next used: there is no timer or
// goroutine per bucket. Its token count and last refill time are folded into
// one integer, the time at which it will be full again, so a request spends
// from it with one compare-and-swap and no lock. Credit is counted in
// nanoseconds, one token being one interval (1e9/rate nanoseconds), so the
// arithmetic is exact.
//
// Buckets live in one Table shared by every connection, split into shards
// with a read-write lock each over the map, and keyed by route and client
// address. IPv6 clients are keyed by their /64, so one host cannot dodge its
// limit by rotating addresses. The table's size is capped, and one sweeper
// goroutine drops buckets that have refilled to full, which carry no
// information.
package ratelimit

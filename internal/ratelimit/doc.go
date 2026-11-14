// Package ratelimit implements per-client token buckets.
//
// A bucket holds its token count and the time it was last refilled, and
// refills lazily when it is next used: there is no timer or goroutine per
// bucket. Token counts are integers in nanoseconds of credit, where one token
// is worth one interval (1e9/rate nanoseconds), so refilling is one addition
// of elapsed time and the arithmetic is exact.
//
// Buckets live in one Table shared by every connection, split into shards
// with a mutex each, and keyed by route and client address. IPv6 clients are
// keyed by their /64, so one host cannot dodge its limit by rotating
// addresses. The table's size is capped, and one sweeper goroutine drops
// buckets that have refilled to full, which carry no information.
package ratelimit

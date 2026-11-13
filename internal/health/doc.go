// Package health implements active and passive backend health checks.
//
// Health belongs to a backend address, not to a pool: a dead server is dead
// for every pool that lists it, so a Checker keeps one Target per address
// and every lb.Backend with that address shares it, as they share the
// address's upstream connection pool.
//
// A Target is in one of three states:
//
//   - Healthy: balancers may pick it.
//   - Unhealthy: active checks failed fall times in a row. rise successes
//     in a row make it healthy again.
//   - Ejected: live traffic failed PassiveFailures times in a row (dial
//     failures, timeouts, resets and 5xx responses). After the Eject
//     backoff it becomes Unhealthy, so active checks decide when it
//     returns; with active checks off it becomes Healthy at once.
//
// The state is an atomic, read by every pick. Every change also bumps the
// Checker's generation, which pools use to know when to rebuild their
// cached healthy sets.
//
// Goroutines: one prober per Target, owned by the Checker and stopped by
// Sync (when its address goes away) or Close, which wait for it to return.
// It runs the active checks, the ejection backoff, and every state change
// except the one into Ejected: that is a compare-and-swap made by the
// request goroutine that saw the last failure, so a failing backend leaves
// rotation within microseconds rather than at the prober's next turn.
package health

# Design decisions

Each entry records a decision, the alternative that was rejected and the
trade-off. Entries are grouped by the phase that made them.

## Phase 0: Setup and lab

### Go version pinned with `go 1.27.4`

The `go` line in `go.mod` names the exact patch release, and CI reads it with
`setup-go`'s `go-version-file`. The rejected alternative was `go 1.27` plus a
`toolchain` line, which pins less precisely for no gain in a single-module
binary project. Trade-off: every Go patch upgrade is an explicit `go.mod` edit.

### The test backend is a library with a thin `main`

`internal/testbackend` holds the server; `cmd/testbackend` only parses flags
and handles signals. Integration tests run the same code in-process, on random
ports, and read its in-memory request log directly instead of scraping
container logs. The rejected alternative was a standalone binary started as a
subprocess, which is slower and harder to clean up. The package is not in the
plan's layout list; it sits under `internal/` so only this module can import
it.

The backend uses `http.Server`. It is not on the proxy's request path, so the
standard-library rule does not apply to it.

### Test backend behaviour flags apply to every endpoint

`--delay-ms` and `--fail-rate` affect `/`, `/echo`, `/slow` and `/healthz`
alike. A backend started with `--fail-rate 1` therefore also fails its health
checks, which is the "accepts TCP but answers 500" case in Phase 5. The
rejected alternative exempted `/healthz`, which would need a second mechanism
to make health checks fail. Trade-off: there is no way yet to have healthy
probes with failing traffic; if Phase 5 needs it, a separate flag will be
added.

### Request log: one JSON line per request, plus an optional in-memory copy

Every request is logged through `slog` (JSON to stdout in the binary, test
output in-process) with method, path, host, forwarded headers, framing
(`Content-Length`, `Transfer-Encoding`), body bytes read and status. With
`Config.Record` the same record is kept in memory for tests. The in-memory log
is unbounded, which is why the binary leaves it off. A request whose client
went away before a response is recorded with status 499 (never sent).

### Test backend shutdown cancels in-flight handlers

`Shutdown` cancels the context every request derives from, so a 60-second
`/slow` returns at once instead of holding test teardown open, then waits for
all handlers to finish so nothing logs after a test ends. The rejected
alternative was a graceful drain. Graceful shutdown is a proxy feature
(Phase 7); for the backend, fast and leak-free teardown matters more.

### Integration harness lives in `test/integration` as a normal Go file

`harness.go` (package `integration`) provides `StartBackends`, `StartBackend`
and `NewClient`, each registering cleanup with `t.Cleanup`. Starting the proxy
in-process with a generated config is added in Phase 1 (proxy) and Phase 2
(config), when those exist; Phase 0 only needs backends. There is no build tag
on integration tests, so `go test -race ./...` runs them; CI runs them in a
separate step so failures are easy to place.

### `goleak` in every package's `TestMain`

The plan asks for it in the integration suite. It is also in every other
package with tests, because a leaked goroutine is cheapest to find in the
package that leaked it. Test HTTP clients use their own `http.Transport` and
close idle connections on cleanup so client-side goroutines do not trip it.

### Certificates: ECDSA P-256, CA kept across runs

`lab/gen-certs.sh` creates a CA and one certificate per hostname (`a.test`,
`b.test` by default) with a DNS SAN and the `serverAuth` EKU. The CA is reused
if present, so anything that already trusts it keeps working; server
certificates are re-issued on every run. ECDSA P-256 was chosen over RSA 2048
for cheaper handshakes; the Phase 9 nginx comparison uses the same certificate
for both proxies, so the choice does not bias it. Keys are written mode 0600.
When the proxy container reads mounted keys in Phase 3, its user must be able
to read them; that is solved then.

### Lab: one Dockerfile, three targets, fixed addresses

`lab/Dockerfile` builds `proxy`, `testbackend` and `loadgen` targets from one
build stage. The proxy runs as an unprivileged user. Backends run as root
because `tc` needs root together with `NET_ADMIN`; the capability is granted
only to backend containers. The load generator is Alpine with `curl` and `wrk`
plus the static `k6` binary copied from the pinned `grafana/k6` image.

The lab network is `10.0.0.0/24` with static addresses (proxy `.10`, backends
`.11`–`.13`, load generator `.20`) to match the plan's config sketch and to
give nginx and the proxy the same fixed upstream addresses in Phase 9. The
trade-off is a possible clash with a host network on that range; none exists on
the development machine, and the subnet is one line to change.

### netem is measured on the response, not the whole request

`netem delay 100ms` on a backend's `eth0` delays every packet the backend
sends. On a new connection that includes the SYN-ACK, so curl's `time_total`
grows by about 200 ms: about 100 ms in `time_connect` and 100 ms between
connect and the first response byte. `lab/netem-check.sh` checks the second
part, `time_starttransfer - time_connect`, against a window of 80–130 ms and
prints the connect delta for reference. Measured on WSL2 (kernel 6.6):
response +100.3 ms, connect +100.2 ms.

This matters for Phase 9: an injected delay of D costs a request on a fresh
upstream connection about 2D, and a request on a reused connection about D.
Results from experiment C depend on upstream connection reuse (Phase 4) and
must say which case they measure.

### CI also runs the lab

Besides build, `go vet`, `staticcheck` (pinned with `go run ...@version`) and
race-enabled unit and integration tests, CI runs `lab/check-lab.sh` on the
runner. That catches a broken Dockerfile or compose file and confirms netem
works on the runner's kernel. The cost is a few minutes of image builds per
push.

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

## Phase 1: Connection handling and plain HTTP proxy

### A helper goroutine writes request bodies

The plan has one goroutine per client connection. A request with a body
breaks that: the connection goroutine starts a writer goroutine that runs
`Request.Write` to the backend while the connection goroutine reads the
response. Writing first and reading after can stall both sides once socket
buffers fill. That happens with a backend that streams its reply while it
reads (`/echo` with 10 MB is close to the loopback buffer limits here), and a
backend that answers early (413) and closes would lose its reply. The writer
is owned by the connection goroutine, which always joins it before reading
the next request. To abort it, the connection goroutine closes the upstream
connection and calls `Body.Abort`, which sets a flag and a past read deadline;
the reader re-checks the flag after each deadline update, so the abort cannot
be missed. Requests without a body are written inline with no goroutine. The
rejected alternative, strictly one goroutine, is simpler but wrong for those
two cases.

### Deadlines: one per header block, moving ones for bodies

| Wait | Deadline |
| --- | --- |
| First request on a new connection | `HeaderRead` |
| Next request on a keep-alive connection | `Idle` |
| Header block | `HeaderRead`, from its first byte, for the whole block |
| Dial | `UpstreamConnect` |
| Request body from client, response to client | `ClientIO`, pushed forward on each read or write |
| Request to backend, response from backend | `UpstreamResponse`, pushed forward on each write or read |
| Response headers | `UpstreamResponse` after the last request byte written |

`ClientIO` (default 60 s, like nginx's `client_body_timeout` and
`send_timeout`) is not in the plan's `[timeouts]` sketch; it was added as
`client_io_ms` for Phase 2. Writes to the backend move the upstream read
deadline too (`DeadlineWriter.ExtendRead`), so a long upload does not count
against the response timeout. The trade-off of moving deadlines is that a
peer trickling one byte per interval keeps a transfer alive indefinitely;
Phase 8 adds a minimum data rate.

`http.ReadRequest` can report a deadline hit mid-line as a parse error of the
partial line, so the proxy treats any header failure after the header
deadline as a timeout. A header timeout closes the connection with no reply
(logged as 408), as nginx does; a slow client gets nothing to read.

### Status codes

| Failure | Status |
| --- | --- |
| Unparseable request, missing `Host` on HTTP/1.1, malformed chunked body | 400 |
| `CONNECT` | 405 |
| Request body stalls past `ClientIO` before a response started | 408 |
| Header block over `MaxHeaderBytes` (64 KiB) | 431 |
| Not HTTP/1.x | 505 |
| Dial refused or failed, upstream closed or sent garbage or a 101 | 502 |
| Dial timeout, no response headers within `UpstreamResponse` | 504 |

A dial timeout is 504 rather than 502, as in nginx: it is a timeout, and it
tells an operator the backend is slow or unreachable rather than refusing.
Statuses 408 for a header timeout and 499 for a client that left are logged,
never sent. Once a response has started, a failure on either side can only be
signalled by closing the client connection; the access log records which side
failed.

### Every proxy-generated error closes the connection

After a 4xx or 5xx from the proxy itself, the client connection is closed, even
when the request was fully read and keep-alive would be safe. This keeps one
rule instead of tracking framing state per error; the cost is a reconnect
after errors. If unread request bytes may remain (an unread body, a rejected
header block), the close lingers: `CloseWrite`, then drain at most 256 KiB for
at most 500 ms. A plain close with unread data sends a reset, which can destroy
the error response before the client reads it.

### One upstream connection per request, closed by the backend

Phase 1 dials a new connection per request and sends `Connection: close`, so
the backend closes first and holds the TIME_WAIT state. If the proxy closed
first, `wrk -c 200` at 15k requests per second would use up the proxy's
ephemeral ports within seconds. Phase 4 replaces this with reuse.

### Bodies: never drained, flushed before blocking

`Request.Write` and `Response.Write` close bodies when done, and the bodies
from `ReadRequest` and `ReadResponse` read to EOF on `Close`. A client that
stops reading a 1 GB response would otherwise cost the proxy the rest of the
download. `httpx.Body` wraps every body with a `Close` that does nothing.

Data is flushed to the other side whenever the proxy is about to block on a
read (`flushingReader`: the source's `bufio.Reader` is empty and the
destination's `bufio.Writer` is not). Without it, response headers and small
pieces of a slow stream sit in a 4 KiB buffer, and a backend that fails
mid-body leaves the client with nothing at all. The cost is one extra write
per socket read on slow streams; fast streams bypass the buffers.

`Request.Write` and `Response.Write` get a writer that hides
`bufio.Writer.ReadFrom` (`noReadFrom`). `ReadFrom` reads straight into the
writer's own buffer, and flushing that writer from inside the read corrupts
it; the first test of an HTTP/1.0 client showed the response headers twice.
Bodies are therefore copied through net/http's own pooled 32 KiB copy buffer,
and writes that size go straight to the socket because the `bufio.Writer` is
empty. Pooling the `bufio` readers and writers is Phase 4.

### Re-framing responses

Responses are always sent as HTTP/1.1, and the proxy, not the backend, decides
`Connection: close` towards the client: every backend response in Phase 1
says close, which concerns only the upstream connection. A body of unknown
length (ended by the backend's close) is re-framed as chunked for HTTP/1.1
clients, so their connection survives. HTTP/1.0 clients get it
close-delimited and are always closed after one response; keep-alive for
HTTP/1.0 was left out as not worth the code.

### Header rules

Hop-by-hop headers (the RFC 9110 list plus `Proxy-Connection` and
`Keep-Alive`) and headers named in `Connection` are removed in both
directions. `Upgrade` is removed, so WebSocket and h2c requests are forwarded
as plain requests; a 101 from a backend is a 502. `Expect: 100-continue` is
forwarded and 1xx responses are relayed, written by hand because
`Response.Write` can add a `Content-Length` to them. `X-Forwarded-For` is
appended to (Phases 6 and 8 decide trust), and `X-Forwarded-Proto` is set to
`http`. `Request.Write` adds `User-Agent: Go-http-client/1.1` when the client
sent none; the proxy sets an empty value, which suppresses it.

Known limitation: `http.ReadResponse` deletes the whole `Connection` header
when it contains `close`, so in a backend response `Connection: close, X-Foo`
the proxy cannot see that `X-Foo` was named, and forwards it. Backends are
trusted (Phase 8 threat model). A test pins the current behaviour and
skips itself if Go starts keeping the header.

### Header size limit below the `bufio.Reader`

`HeaderLimitReader` sits between the connection and its `bufio.Reader` and
fails reads once `MaxHeaderBytes` have been read for the current header block.
Without it, `ReadRequest` buffers a header line of any length. Bytes the
`bufio.Reader` already held (read ahead with the previous request) are not
counted, so the effective limit can be exceeded by up to its 4 KiB size. When
the limit is hit, the proxy answers 431 instead of 400.

### Connection tracker and the cap

The tracker is a set of open connections behind a mutex plus atomic counters,
shared by the accept loop and the connection goroutines. It is counted under
the plan's "in-flight counters" shared state. Over the cap (default 10,000),
connections are accepted and closed at once. The rejected alternative, not
accepting until a slot frees, leaves them in the kernel backlog, where they
time out without the proxy seeing them and make shutdown harder.

Accept errors other than a closed listener pause the loop for 10 ms. Phase 8
replaces this with a backoff for running out of file descriptors. Shutdown in
Phase 1 closes everything at once; Phase 7 adds the drain.

### Test backend changes

`/bytes?n=N[&chunked=1]` streams a fixed pattern that tests regenerate with
`testbackend.Pattern`, so a 1 GB body costs no memory to send or check. `/echo`
reads the first piece of the body before writing its headers, because Go's
server sends `100 Continue` only when a handler reads before replying, and it
flushes each piece so the echo streams during the upload.

### Results

Measured on WSL2 (kernel 6.6, 22 CPUs), Go 1.27.4:

- 1 GB read at about 100 MB/s through a proxy that moves it at 3.4 GB/s
  unthrottled: heap in use grew by under 1 MiB.
- 200 clients × 50 keep-alive requests: goroutines 4 → 207 → 4, fds 9 → 9.
- `wrk -c 200 -d 30s` against a `-race` build: see
  `bench/phase1/results/wrk-race.txt`.

## Phase 2: Config, pools and load balancing

### The config file replaces the command-line settings

The proxy takes `-config` and nothing else besides `-check-config`. The Phase
1 flags (`-listen`, `-backend`, `-max-conns`, `-max-header-bytes`) moved
into the file as `[[listener]]`, `[pool.<name>]` and `[limits]`, so there is
one source of truth for what a running proxy does. The `[timeouts]` table
also has `client_io_ms` (see Phase 1). `cert` and `key` on a route are
parsed and kept but not read until Phase 3.

Exit codes: 0 valid (or a clean stop), 1 a bad config or a failure while
running, 2 bad flags. `-check-config` prints one summary line on stdout when
the file is valid, as `nginx -t` does.

### Strict decoding, every error reported with its field

Unknown keys are errors (`MetaData.Undecoded`), so a typo such as
`algoritm` fails instead of silently falling back to the default. Each
problem is a `FieldError` that names the field's path, such as
`pool.pool_a.backends[1].address`, and validation collects all of them
rather than stopping at the first, so one `-check-config` run lists
everything. Syntax and type errors come from the TOML decoder and carry the
line and the last key read. The rejected alternative, failing on the first
problem, is simpler but makes fixing a file one round trip per mistake.

A missing timeout or limit takes its default; an explicit `0` is an error
(`MetaData.IsDefined` tells them apart), because a zero timeout would mean
"fail at once", which is never what someone writing `0` intends. Numbers
have upper bounds (a day for timeouts, 10 M connections, 16 MiB of headers)
so a slip of the keyboard cannot ask for something absurd.

Listeners may use port 0 (any free port) and may repeat it; tests use this.
Backends may not. Hostnames may contain `_`, which DNS forbids but Docker
service names use.

### Packages: `conn` → `config` → `lb`

`config` builds `lb` pools, and `conn` reads `config` snapshots. `Timeouts`
moved from `conn` to `config` so the dependency runs one way. `lb` knows
nothing about files or routes; `config` knows nothing about connections.

### The snapshot is loaded at a request's first byte

`config.Store` wraps `atomic.Pointer[Snapshot]`. A `Snapshot` holds the
listeners, timeouts, limits, the pools and a map from normalised host to
pool, and is never modified after `NewSnapshot` returns, so readers take no
lock. Each request loads the pointer once, when its first byte arrives, and
uses that one snapshot for its header deadline, its route, its pool and the
rest of the exchange.

The alternative was to load before waiting for the next request on a
keep-alive connection. That would serve a request arriving after a 60-second
idle wait from a config up to 60 seconds old, which matters once Phase 7
reloads remove backends. The cost of loading at the first byte is that the
idle wait and the header-size limit for that request come from the previous
snapshot (or, for a connection's first request, the one current at accept).

Not everything in the snapshot is live. `max_conns` sizes the connection
tracker at startup, and the listener list is opened once; Phase 7 decides
how listener changes apply.

### Routing: exact host match, default pool or 404

The `Host` header is normalised (port removed, IPv6 brackets removed,
trailing dot removed, lower-cased) and looked up exactly. There are no
wildcard routes; they can come with wildcard certificates in Phase 3 if they
are wanted. A host with no route goes to `default_pool` if one is set, and
otherwise gets 404. Like every error the proxy generates, the 404 closes the
connection (Phase 1 rule). Route hosts in the file are normalised the same
way, so `A.Test.` and `a.test` are the same route and listing both is an
error. Routing allocates nothing unless the host has upper-case letters.

### Balancer interface takes the healthy set

`Pick(r *RequestInfo, healthy []*Backend) *Backend` is given the backends
that may be chosen. In Phase 2 that is always the whole pool; Phase 5 passes
only the healthy ones. `RequestInfo` carries the client IP and the request
headers as the client sent them, before the proxy adds `X-Forwarded-*`.

Pools are built per snapshot and never change. The only moving state is
atomic: each backend's in-flight counter and each balancer's rotation
counter. Both count under the plan's "in-flight counters" and "upstream
pools" shared state, and nothing new needs a mutex.

### Backends belong to one pool

The same address in two pools gives two `Backend` values with their own
in-flight counters, as nginx treats a server listed in two `upstream`
blocks. The rejected alternative was one `Backend` per address across all
pools, so that least-connections in one pool would see load sent from
another. It is more accurate when pools overlap, but it couples pools that
are otherwise independent, and the lab is the only place they overlap.
Phase 5's health state will follow the same rule unless that turns out
wrong. To carry counters across reloads, Phase 7 will build the new snapshot
from the old one and reuse `Backend` values by pool and address.

### Round-robin: one atomic counter

Each pick is `healthy[(n.Add(1)-1) % len(healthy)]`. Every `Add` returns a
distinct value, so k·N picks over N backends land exactly k on each under
any concurrency, which the 3,000-request test checks exactly. With a
changing healthy set (Phase 5) the rotation skips or repeats at the moment
the set changes, which does not matter.

### Least-connections: rotation on ties, no lock

The balancer scans from a rotating start position and takes the first
backend with the lowest in-flight count. Without the rotation every tie (all
idle, the common case at low load) would go to the first backend.

Pick and `Acquire` are two steps, so goroutines picking at the same moment
can choose the same backend. The error is at most the number of concurrent
picks and the next pick corrects it. A pool-wide lock around pick and
acquire would remove it but serialise every request in the pool.

The count runs from just after the pick until the response has been written
to the client or has failed, so it includes the client's download time. A
slow client therefore makes its backend look busy. nginx's `least_conn`
counts the same way. Counting only until the response headers arrive would
measure the backend more precisely, but it would undercount streaming
responses.

Measured (`TestLeastConnAvoidsSlowBackend`, `-race`, 3 backends, one with
+200 ms, 50 closed-loop clients for 3 s, fresh upstream connection per
request; `bench/phase2/results/balancers.txt`):

| Algorithm | Requests in 3 s | Fast, fast, slow | Slow share |
| --- | --- | --- | --- |
| round_robin | 2,145 | 715 / 715 / 715 | 33.3% |
| least_conn | 35,112 | 17,478 / 17,400 / 234 | 0.7% |

Round-robin sends the slow backend its third whatever happens, and since
every client waits on it every third request, throughput collapses to the
slow backend's pace. Least-connections parks about 16 of the 50 clients on
the slow backend (234 requests at 200 ms ≈ 15.6 in flight). That is the
level at which the two fast backends' counts catch up with it: with 50 in
flight, 16 on the slow one and 17 on each fast one. The other 34 clients run
at full speed. When all backends are equally fast the two algorithms give
the same split.

### Consistent hashing: 160 virtual nodes, address-derived points

Each backend gets 160 points on a 64-bit ring at `Hash64(addr + "#" + i)`.
A request's key is hashed and goes to the first point at or after it
(`slices.BinarySearch` over a sorted `[]uint64`). Points depend only on the
address, never on list order or the `Backend` value, so every instance and
every reload of the same config builds the same ring. Ties between points
are broken by address for the same reason.

The hash is 64-bit FNV-1a followed by murmur3's `fmix64` finaliser. FNV-1a
alone mixes the last bytes of its input poorly, and the point names differ
only at the end. The loop is written out because `hash/fnv` returns an
interface and allocates. The rejected alternative, `hash/maphash`, is
faster but seeded per process, so two proxies would disagree on every key.

A point whose backend is not in the healthy set is skipped by walking
clockwise. That gives exactly the mapping of a ring rebuilt without that
backend (the unit test checks every key), so Phase 5 can eject a backend
without rebuilding anything and without moving any other key.

The key is the client IP, or with `hash_key = "header:<Name>"` that header's
first value. The name is canonicalised when the config loads and looked up
with `h[name]`, because `Header.Get` canonicalises on every call. A request
without the header (or with an empty value) falls back to the client IP, so
it is still sticky to something rather than spread at random.

**Removal** (`TestRingRemovalRemapsOneNth`, 10,000 keys, each of N backends
removed in turn): the moved fraction ranged from 29.3% to 35.6% for N = 3,
18.5% to 20.7% for N = 5, and 9.0% to 10.6% for N = 10, and no key moved
except those on the removed backend. The test allows ±30% of 1/N.

**Spread** (`TestRingSpread`, 100,000 client IPs; each row averages 20
address sets: one with the lab's numbering, 10.0.0.11 upwards, and 19 seeded
random ones):

| vnodes | N | mean CV | mean max deviation | worst max deviation |
| --- | --- | --- | --- | --- |
| 1 | 3 | 59.9% | 80.9% | 152.8% |
| 10 | 3 | 24.7% | 33.5% | 77.5% |
| 100 | 3 | 7.8% | 10.4% | 20.9% |
| 100 | 10 | 8.5% | 16.3% | 25.8% |
| **160** | **3** | **6.1%** | **8.2%** | **14.5%** |
| **160** | **5** | **6.2%** | **9.9%** | **16.1%** |
| **160** | **10** | **7.4%** | **13.3%** | **22.4%** |
| 200 | 3 | 5.0% | 6.8% | 14.6% |
| 200 | 10 | 6.7% | 12.6% | 21.4% |

"Max deviation" is the most or least loaded backend's distance from a fair
share. The coefficient of variation falls roughly as 1/√vnodes. A single
address set is noisy: a first measurement with one fixed set had 160 nodes
looking worse than 100 for N = 3. Hence the 20 sets.
160 (ketama's choice) was kept over 200: the gain is a point or two, the
ring costs 16 bytes per point (25 KB for ten backends), and building it is a
sort of N·vnodes points per reload. The test holds 160 to 15% mean and 25%
worst max deviation.

### Picks allocate nothing

Each balancer has a benchmark with `-benchmem` and a unit test that asserts
`testing.AllocsPerRun == 0`, so CI enforces it. The proxy keeps the
`RequestInfo` in the connection's state rather than on the stack, because a
pointer passed through the `Balancer` interface escapes and would be
allocated per request. On the development machine (22 CPUs, 10 backends)
a pick costs 8 ns for round-robin and least-connections and 18–22 ns for the
ring, with zero allocations; see `bench/phase2/results/balancers.txt`.

### Lab

`lab/proxy.toml` is mounted into the proxy container. It defines a
round-robin default pool (which the container healthcheck also hits),
`a.test` on least-connections and `b.test` on consistent hashing by
`X-User-Id`, each over all three backends. `check-lab.sh` runs
`-check-config` inside the container and checks each route. Because the
healthcheck shares the default pool, it checks that six requests reach all
three backends rather than exactly two each.

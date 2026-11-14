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

## Phase 3: TLS termination and SNI routing

### The handshake is its own step in the connection goroutine

With `Proxy.TLS` set, `ServeConn` wraps the accepted connection in
`tls.Server` and calls `HandshakeContext` before reading any request. The
handshake has its own timeout, `timeouts.handshake_ms` (default 5 s, like
`header_read_ms`), applied twice: as a deadline on the socket, which
bounds every read and write of the handshake, and as the context's
deadline, which also ends it when the server closes. A failed handshake
writes one `tls handshake failed` log line (client, SNI if one was read,
whether it timed out, the error) and closes; it has no access line
because there was no request.

The first request on a connection then gets `header_read_ms` from the end
of the handshake, so a new HTTPS client has up to handshake plus header
time before it must have sent a request. The rejected alternative, one
budget for both, would make the header timeout depend on handshake cost.

Read timeouts on a `tls.Conn` are not sticky (crypto/tls keeps the
connection usable after a read deadline), so the idle wait and
`Body.Abort` work as they did on TCP. Write timeouts are sticky, which
does not matter because every write error already ends the connection.
`tls.Conn` allows one reader and one writer at once, which is what the
request-body writer goroutine and the connection goroutine do.

### Listener modes, HTTPS by default

Each `[[listener]]` has `mode = "https" | "http" | "redirect"`. A missing
mode means HTTPS, so a config cannot serve plaintext by accident; a plain
listener must say `mode = "http"`, and a file with an HTTPS listener and
no certificates fails `-check-config`. The rejected alternative, plain
HTTP by default as in nginx, keeps old configs working but fails open.
`conn.Server.Serve` takes the handler per listener, so all modes share
one connection tracker and one cap.

### Certificates live in the snapshot

`cert`/`key` on a route and `default_cert`/`default_key` in `[tls]` are
paths resolved against the config file's directory. `NewSnapshot` loads
them into a `tlsx.CertSet` (normalised host to certificate, plus the
default) and rejects, by field, a file that does not load, a key that
does not match, or a certificate whose names do not cover its route's
host. Expired or soon-to-expire certificates (30 days) only warn, at
startup and in `-check-config`: serving an expired certificate with a
warning is better than refusing to start. Loading in `NewSnapshot` means
`-check-config` checks certificates too, and a Phase 7 reload rereads
them, which is how renewed certificates get picked up. The cost is that a
reload reads every certificate file, not just changed ones.

Validation requires a certificate for every route reachable over HTTPS:
either the route has its own or there is a default. Routes are exact host
matches; wildcard routes were left out (a wildcard certificate can still
be a route's or the default certificate).

### One tls.Config for the process; certificates come from the snapshot

`tlsx.ServerConfig` returns one `tls.Config` shared by every HTTPS
listener. Its `GetCertificate` loads the current snapshot's `CertSet` on
each handshake. The alternative, building a `tls.Config` per snapshot
(through `GetConfigForClient`), would give each config its own session
ticket keys, so every reload would invalidate every ticket. With one
config, tickets issued on one port resume on another and survive
reloads. crypto/tls generates the ticket keys and rotates them on its
own (a new key daily, each valid for a week). They are per process: two
proxy instances behind one address do not share tickets, and a restart
invalidates them all. Sharing them would mean managing ticket keys,
which is not worth it here.

After a reload, a connection's certificate may come from one snapshot
and its first request's route from the next. That is harmless: both
were valid configs a moment apart.

### Unknown or missing SNI: the default certificate or unrecognized_name

`Lookup` returns the route host's certificate, else the default, else
nil. `GetCertificate` returning `nil, nil` with `tls.Config.Certificates`
empty makes crypto/tls send `unrecognized_name` (checked in its source
and in tests). The choice is made by configuration alone: setting
`default_cert` means unknown and missing names get it; leaving it out
means their handshakes fail.

crypto/tls refuses a ClientHello whose server name ends in a dot and
keeps its case; the proxy lower-cases it. Some clients (busybox `wget`,
the lab's healthcheck) send an IP literal as SNI, which RFC 6066 forbids;
it is treated as any unknown name.

### SNI and Host must agree, and no SNI means the default pool

On a TLS connection, a request whose normalised `Host` (no port, no
trailing dot, lower case) differs from the normalised SNI gets 421
Misdirected Request and the connection closes, like every response the
proxy generates. The certificate the client verified was chosen for the
SNI name; serving another host on that connection would let a client
reach a pool with a certificate that was never checked for it, and makes
SNI-based filtering in front of the proxy pointless. The check runs
before routing, so the route is in effect chosen by SNI. For an
absolute-form request (`GET https://b.test/`) `http.ReadRequest` takes
the host from the URL, and the check uses that.

A connection without SNI got the default certificate, so it belongs to
the default pool: a `Host` with a route of its own gets 421, and any
other host goes to the default pool (or 404). The rejected alternative,
routing SNI-less requests by `Host` as nginx does, would let a client
that never named `a.test` in the handshake reach `a.test`'s pool. Real
clients always send SNI for host names, so the cost is limited to IP
clients asking for named hosts.

Plain HTTP listeners do no such check.

### TLS settings

| Setting | Value | Why |
| --- | --- | --- |
| Versions | TLS 1.2 and 1.3 | 1.0 and 1.1 are deprecated (RFC 8996) |
| TLS 1.2 suites | ECDHE-ECDSA and ECDHE-RSA with AES-128-GCM, AES-256-GCM, ChaCha20-Poly1305 | ECDHE gives forward secrecy, AEAD removes CBC padding attacks; RSA variants are there for RSA certificates |
| TLS 1.3 suites | crypto/tls's three | Not configurable in Go; all are AEAD, and the key exchange is always ephemeral (crypto/tls resumes only with `psk_dhe_ke`) |
| Key exchange groups | Go's defaults | As testssl.sh sees them: X25519MLKEM768, SecP256r1MLKEM768, SecP384r1MLKEM1024 (hybrid post-quantum), X25519, P-256, P-384, P-521 |
| Suite order | Go's | Prefers AES-GCM with AES hardware, ChaCha20 when the client prefers it |
| ALPN | `http/1.1` | A client offering only `h2` fails with no_application_protocol; `h2,http/1.1` (browsers, curl) gets `http/1.1` |
| Resumption | Session tickets | See above; crypto/tls has no server-side session cache |

Forward secrecy comes from the ephemeral (EC)DHE key exchange: the
certificate key only signs the handshake, so a stolen key does not
decrypt recorded traffic. The suite list pinned for TLS 1.2 is what
guarantees it there, since the static-RSA suites are not in it. The
ticket keys are the remaining exception: anyone holding them can decrypt
resumed sessions made with them, which is one reason crypto/tls rotates
them.

### Closing a TLS connection

`ServeConn` closes the `tls.Conn` on the way out, which sends
`close_notify`, so a client reading a close-delimited body (HTTP/1.0)
can tell the end from a truncation. crypto/tls bounds that write at 5
seconds. The lingering close after an error over TLS sends `close_notify`
first, then the TCP half-close, then drains through the TLS connection,
because only reading the remaining records consumes the client's unread
bytes.

### Redirect listener

`mode = "redirect"` answers every request with 308 Permanent Redirect to
`https://host[:redirect_port]/uri` (the port is left out when it is 443)
and closes. 308 rather than 301 because it keeps the method and body, so
a POST to the HTTP port is not silently turned into a GET. Only hosts the
proxy would serve are redirected (a host with a route, or any host if
there is a default pool); others get 404, so the listener cannot bounce
clients to an arbitrary site through a forged `Host`. The request is
read with the proxy's own deadlines and header limit, reusing the same
connection code with a flag that replaces proxying.

### Access log

Lines carry `scheme`, and on HTTPS also `sni`, `tls_version` and
`tls_resumed`. `X-Forwarded-Proto` is `https` on TLS listeners.

### Tests

`internal/tlsx/tlstest` issues a CA and ECDSA certificates in memory, so
no test needs openssl except the ones about openssl. Every Phase 1 and 2
integration test runs as an `http` and an `https` subtest. The harness's
HTTPS proxy has a `*.test` default certificate, and its client resolves
every host to the proxy, so the host in a URL is both SNI and `Host`.

Two things in the openssl checks would otherwise pass for the wrong
reason:

- OpenSSL 3 at its default security level refuses to offer TLS 1.0 or
  1.1 at all (`no protocols available`), so `s_client -tls1_1` fails
  without ever reaching the proxy. The test passes `-cipher
  DEFAULT@SECLEVEL=0` and requires the proxy's `protocol_version` alert.
- `s_client -reconnect` in OpenSSL 3.0 reconnects before the TLS 1.3
  ticket arrives (it is sent after the handshake), so it never shows
  reuse for TLS 1.3. `-reconnect` is checked for TLS 1.2; TLS 1.3 is
  checked with `-sess_out` after a request and `-sess_in`, and with Go's
  client (`DidResume`).

### Lab

The proxy serves HTTPS on 8443 and redirects from 8080. Certificates
come from `lab/gen-certs.sh` (now also `default.test`, and 397 days, the
most browsers accept) and are mounted read-only. The keys stay mode 0600
and owned by the user, and the proxy container runs as that user
(`LAB_UID`/`LAB_GID`, exported by `make lab-up` and `check-lab.sh`). The
rejected alternatives were group- or world-readable keys, or copying them
into the image. The container healthcheck uses busybox `wget`, which
does verify certificates on Alpine, hence `--no-check-certificate`.

### testssl.sh runs inside the lab network

`bench/phase3/testssl.sh` runs testssl.sh 3.2.5 from its image, pinned by
digest, on the lab network against the proxy container, with the lab CA
trusted. Running it with `--network host` against a proxy on localhost
was tried first and fails under Docker Desktop, whose containers cannot
reach the WSL host's loopback. The script fails if the scan has no
protocol or cipher section (a scan that never connected must not pass)
or rates anything in them LOW or worse.

Result (`docs/testssl-phase3.txt`): TLS 1.2 and 1.3 only, ECDHE with
AEAD only, no vulnerabilities, grade A+. The remaining notes are not
about TLS settings: no OCSP or CRL URL (the lab CA has none), no CAA
record, and no HSTS or security headers in the test backend's responses.
HSTS belongs to the origin; a header-adding option in the proxy would be
a separate feature.

### Results

On the development machine (WSL2, `-race`): the 1 GB slow-reader test
reads at 44 MB/s over HTTPS (92 MB/s plain) with heap growth of 2.3 MiB
(0.3 MiB plain), from TLS record buffers per connection. 200 keep-alive
HTTPS clients return goroutines and fds to baseline (5 → 214 → 5, 9 → 9).

## Phase 4: Upstream connection reuse and concurrency

### One idle pool per backend address, shared across lb pools

`upstream.Registry` holds one `upstream.Pool` per backend address, and
`config.NewSnapshot` gives each `lb.Backend` the pool for its address.
`lb.Backend` objects stay per lb pool (Phase 2: each pool has its own
in-flight counters); only connections are shared, because a TCP
connection to `10.0.0.11:8000` can carry any request for that address,
whichever balancer chose it. In the lab the three pools share their
connections to each backend. The rejected alternative, a pool per
`lb.Backend`, keeps `max_idle_per_backend` exact per lb pool but opens up
to three times the connections to the same server. With pools keyed by
address, Phase 7 keeps idle connections across a reload for free: a new
snapshot asks the registry for the same addresses and gets the same
pools.

The registry is built once in `run` and is read-only on the request path,
because each backend holds its pool pointer directly. Its map lock is
taken only while a snapshot is being built.

This replaces the Phase 1 decision to dial per request with
`Connection: close`. `max_idle_per_backend = 0` brings that behaviour
back, and the bench uses it for the reuse-off run.

### The idle stack is LIFO

`Get` pops the most recently parked connection and `Put` pushes. Under
steady load a few connections stay hot and the rest sink to the bottom,
reach `idle_timeout_ms` and are closed, so the pool shrinks to what the
load needs. A FIFO queue would cycle through every connection, keep each
of them just young enough to survive, and never shrink. If the top
connection has expired, every connection under it is older, so `Get`
closes the whole stack at once. The reaper trims from the bottom.

The mutex guards only the slice and is never held across I/O. Under
`wrk -c 100` on 4 cores it accounts for about 2% of the mutex wait
(`bench/phase4/results/profile.txt`), about 0.01 µs per request. If it
ever showed up, the next step would be to shard each pool into a few
stacks picked by a per-connection index, at the cost of LIFO being only
per shard.

### Stale connections: lazy detection with a non-blocking peek

Backends close idle connections whenever they like. The two options in
the plan were a reader goroutine per idle connection, which notices the
close at once (what `http.Transport` does), or finding out on next use.
I chose the latter, with a cheap check on `Get`: if the connection's
`bufio.Reader` holds buffered bytes, or a `recvfrom(MSG_PEEK|MSG_DONTWAIT)`
on the socket returns anything other than `EAGAIN` (a FIN, a reset,
unsolicited bytes such as a backend's 408), the connection is discarded
and `Get` tries the next one. The cost is one system call per reuse. The
`RawConn` and the callback are built once per connection, so the check
allocates nothing. A reader goroutine per connection would cost a
goroutine and a buffer per idle connection, plus a hand-off and a
deadline change on every `Get`.

What the check cannot see is a close that lands between the check and
the request. The retry covers that. On platforms other than Linux the
check always passes and the retry is the only defence.

### When a connection is reused

A connection is parked only if every one of these holds:

- the request writer finished on its own (not through `stop()`), within
  a short grace if it was still running (below), and the request body
  reached EOF;
- the response body was read to EOF, or there was none;
- the backend did not send `Connection: close` (`resp.Close`, which also
  covers HTTP/1.0 responses);
- the body was not close-delimited: it had a length or was chunked;
- the client write succeeded.

Every other path closes the connection. The connection is parked before
the access line is written, so the next request can have it at once; an
earlier version parked it in a deferred call, and a request on another
client connection could dial in the gap.

The writer gets that grace because "still running once the response is
out" does not mean "the backend answered early". The backend can read
the whole body as soon as the writer's last flush lands, and answer,
and the proxy can relay the reply to the client, before the writer
goroutine runs again to return and report done. An instant check took
that for an early answer and closed a good connection. It showed up as
a flaky `-race` test on a loaded CI runner. Under load in production it
would quietly lower reuse for requests with bodies.

The fix waits up to 50 ms (`writerGrace`) for the writer, and only when
the connection is otherwise fit to reuse: the response went out whole
and the backend did not ask to close. A backend that answers early and
closes, the usual 413, is not waited on, and the upload stops at once
as before. A backend that answers early and keeps the connection must
read the rest of the body anyway (RFC 9112), so up to 50 ms more of it
is forwarded. If the body ends within the grace the connection is
reused, and otherwise it is closed. Waiting on the body's EOF instead
does not work. For a Content-Length body, `Request.Write` reads once
more to find EOF, and the flushing reader sends the last bytes before
that read, so the backend can have the whole request before EOF is set.
The common case pays nothing: the writer has usually finished, and the
timer is built only when it has not. `BenchmarkProxy` allocations are
unchanged.

An upstream `Conn` has two ways to close. `Close` is the socket close
that the request writer goroutine and `bodyWriter.stop` may call
concurrently to abort. `Discard` closes and returns the pooled `bufio`
buffers, and only the owning goroutine calls it, after the writer is
joined. `Pool.Put` and the pool's own closes use `Discard`.

### Retry once, strictly

A request that fails on a reused connection is sent again once on a new
connection to the same backend when all of these hold: no response byte
arrived, the error is EOF, a reset or EPIPE (not a timeout, which means a
slow backend, not a stale connection), the request has no body (a body
streams from the client and cannot be replayed), and either nothing was
written or the method is GET, HEAD, OPTIONS or TRACE. The last condition
is the strict reading chosen over the plan's "any request": a POST that
left the proxy may have been processed by a backend that died before
answering. `http.Transport` makes the same distinction. The access log
marks such requests `"retried": true`. Phase 5 retries on a different
backend; this retry is only about stale keep-alive connections.

### The reaper and the settings

One reaper goroutine per registry wakes every `idle_timeout / 2` (at
least 10 ms) and closes connections past the timeout. `run` owns it and
`Registry.Close` stops it. `Get` already skips expired connections, so
the reaper only bounds how long a socket to an idle backend lingers.

`[upstream]` defaults to `max_idle_per_backend = 100` and
`idle_timeout_ms = 30000`. The timeout is shorter than common backend
keep-alive timeouts (nginx 75 s, the test backend 120 s), so the proxy
usually closes first and the liveness check rarely has to catch a backend
close. There is no limit on connections in use per backend, only on idle
ones, as with nginx's `keepalive`.

### Pools size themselves to each backend's peak

Each backend's pool grows to the most requests that backend has had in
flight at once. Round-robin does not even out in-flight counts, and a
stall (GC, the race detector, a slow backend moment) piles requests onto
one backend, so with 100 clients and three backends the pools grow
towards 100 connections each, 300 in all, not 100. New dials after
warm-up come in short bursts as a pool reaches a new peak, then stop at
the cap. Without `-race` the count is flat within about two seconds. The
integration test therefore bounds new connections as a fraction of
requests (under 0.5%; without reuse it is 100%), and `reuse.sh` records
the per-second curve. Least-connections balancing keeps peaks lower;
round-robin is what the bench uses.

### Buffers from `sync.Pool`, per-request state in the connection

The 4 KiB readers and writers on client and upstream connections come
from `sync.Pool`s in `httpx`. Client buffers go back when the connection
goroutine ends; upstream ones through `Discard`. The access record, both
`httpx.Body` values, both flushing readers and the request writer live in
`clientConn` and are reset per request, so a request without a body
starts no goroutine and allocates none of them. Copy buffers were already
pooled by `net/http` (`Request.Write` and `Response.Write` copy through
its 32 KiB pool), so no copy-buffer pool of our own was needed.

What remains (35 allocations for a GET) is mostly `net/http`'s parsing
and serialising (header maps, the URL, transfer writers) and the
`X-Forwarded-For` string, which only a parser of our own would remove.

### Admin listener: pprof on loopback only

`internal/admin` is an `http.Server` serving the `net/http/pprof`
handlers. `admin.address` must be a loopback IP or `localhost`, checked
by the config parser and again by `admin.Listen`, because profiles
expose memory contents and a CPU profile or trace can be asked to run
for minutes. `WriteTimeout` is 5 minutes so long profiles fit; pprof
refuses longer ones. Mutex and block profiling cost something on every
contended lock or blocking event, so they are off unless
`mutex_profile_fraction` and `block_profile_rate_ns` are set; the bench
scripts set them. Phase 5 adds `/metrics` and `/status` to the same
server.

### The access log is buffered, and Write never does I/O

The first mutex profile under load had one dominant lock: slog's
handler mutex, about 96% of all mutex wait. slog holds it around each
`Write` to its output, and that `Write` was a system call per access
line. Logging off barely changed throughput (system calls on the request
path are the limit on this machine), but every request queued for it.

Fix, chosen with the user: a buffered log, like nginx's
`access_log buffer=64k flush=1s` (`[log] buffer_bytes = 65536`,
`flush_ms = 1000`; `buffer_bytes = 0` is unbuffered). Two attempts did
not help: a buffer flushed by the writer that filled it, and the same
with the write moved outside the buffer's own lock. Either way the
flushing goroutine was still inside slog's `Write`, holding slog's mutex
for the whole system call. The version kept, `internal/logbuf`, has two
buffers and a flusher goroutine. `Write` only copies; a full buffer is
handed to the flusher and the spare takes its place. A writer waits only
if the spare fills too before the flusher finishes, which is backpressure
from a slow stdout. The flusher also writes out a partly full buffer
every interval.

Under `wrk -c 100` on 4 cores, mutex wait on the log path fell from about
3.2 s to about 0.2 s per 8 s profile, the same as with slog writing to
`io.Discard`. The cost is durability: a crash loses up to one flush
interval of log lines. `main` closes the writer before `os.Exit`, so a
failed startup still logs its error, and SIGTERM flushes everything.
Lines from different connections stay whole and in order.

### GOMAXPROCS follows the CPU limit

Go 1.25 and later set GOMAXPROCS from the cgroup CPU limit, so no
dependency like `automaxprocs` is needed. The proxy logs `gomaxprocs` and
`num_cpu` at startup. The lab gives the proxy `cpus: "2"`, and
`check-lab.sh` checks the startup line against `/sys/fs/cgroup/cpu.max`.

### Tests and benches

Unit tests in `internal/upstream` (LIFO order, caps, expiry, the
liveness check against FIN, reset and stray bytes, the reaper, `Close`)
and `internal/conn` (each rule for not reusing, checked on the pool
itself because the liveness check and the retry would hide a connection
parked by mistake; the retry and each case where it must not happen).
A test-only hook, `Proxy.afterRequestWrite`, holds the request writer
back after its last write, so the late-writer case is tested every run
rather than when the scheduler happens to produce it. The connection is
parked before its access line is written, so tests that read access
lines by position wait for each line before sending the next request.
Integration tests cover the steady state with 100 clients and a backend
restarted between requests, over HTTP and HTTPS. The test backend counts
accepted connections, served at `/stats`, so reuse is measured at the
backends rather than by the proxy's own account.

`bench/phase4/` pins the proxy, the backends and wrk to separate CPUs
(`lib.sh`). `scaling.sh` reports the median of three interleaved runs,
because throughput on WSL2 varies by a quarter or more between runs.

### Results

On the development machine (WSL2, Core Ultra 7 155H):

- Reuse on vs off, `wrk -c 100`, 4 cores: 67k vs 27k req/s, p99 8.0 vs
  14.7 ms. New upstream connections: 222 in all with reuse, 24 of them
  after the first 3 s; without reuse, one per request (about 27k/s) and
  TIME_WAIT at the kernel's cap of 32,768.
- GOMAXPROCS 1, 2, 4: 18.7k, 34.3k, 51.3k req/s (1.84×, 2.75×), against
  260k from the backends alone. The CPU profile is about half system
  calls.
- Mutex wait under load: about 0.3–0.4 µs per request; slog's handler
  lock about half of it, the runtime's own locks most of the rest, the
  upstream pools 2%.
- Allocations per request (`BenchmarkProxy`): GET 80 allocs / 24 KB
  before, 41 / 2.9 KB with reuse, 35 / 2.6 KB with pooling; POST 89 →
  50 → 42. With reuse off, pooling takes a GET from 80 to 72 allocations
  and 24 to 15 KB.
- `-race` wrk, GET and POST `/echo` at once for 30 s: no races, no
  non-200 responses, fds back to baseline after reaping.

## Phase 5: Health checks, retries and metrics

### Health belongs to the address

`health.Checker` keeps one `Target` per backend address, and every
`lb.Backend` with that address shares it, as it shares the address's
upstream pool. A server that is down is down for every pool that lists
it, and per-pool probers would check the same server once per pool (three
times per interval in the lab). This reverses the Phase 2 note that health
would follow the per-pool rule; in-flight counters and request counts stay
per pool, as before. The cost: one `[health]` table for the whole proxy, so
two pools cannot check the same address with different paths or
thresholds. Per-pool settings would bring back per-pool targets.

### Three states, one writer each

A target is `Healthy`, `Unhealthy` (active checks failed `fall` times in a
row) or `Ejected` (live traffic failed `passive_failures` times in a row).
The state is an `atomic.Int32` read by every pick.

The target's prober goroutine makes every change except one: the move into
`Ejected` is a compare-and-swap from `Healthy` made by the request
goroutine whose failure completed the run. It then wakes the prober
without blocking (a channel with room for one), and the prober starts the
backoff, records the time for `/status` and closes the address's idle
connections. So a failing backend leaves rotation within microseconds of
the failure that decides it, not at the prober's next turn. In the kill
test it was out of rotation 2.5 ms after `kill -9`. The prober's own
moves use compare-and-swap too, so an ejection that races a check result
wins and the result is dropped.

When the backoff (`eject_ms`) ends the target becomes `Unhealthy`, not
`Healthy`: active checks decide when it returns, after `rise` successes.
A backend that is still broken stays out without first taking another
round of failed requests. With active checks off there is nothing to
decide, so it goes straight back to `Healthy`, as nginx's
`max_fails`/`fail_timeout` does. The backoff is fixed. An exponential one
would treat a backend that keeps failing more gently, but active checks
already keep such a backend out, and one fixed number is easier to reason
about in the Phase 9 demos.

Every state change bumps the checker's generation counter, which is what
pools watch (below).

### Hysteresis: a run must be unbroken

`health.Machine` counts the current run of successes or failures; a result
of the other kind resets it. A healthy backend becomes unhealthy after
`fall` failures in a row, an unhealthy one healthy after `rise` successes
in a row. Results that alternate, or runs one short of a threshold, never
move the verdict, which the table-driven test checks with scripted
sequences. The rejected alternative, failures counted over a sliding
window, ejects a backend that fails one check in three; with runs that
backend stays in, and passive checks catch it if its traffic fails too.

Backends start healthy and are checked at once. Starting unhealthy would
answer 503 for `rise` intervals after every start. A backend that is dead
at startup costs a few requests until passive checks or the first `fall`
checks catch it, and those requests are retried. Later checks are spread
by ±10% jitter, so probers started together do not stay in step.

### Active checks

Each check is `GET <path>` through an `http.Client` (allowed off the
request path) with keep-alive off, bounded by `timeout_ms`. Each check
opens its own connection, so it tests the dial and accept path a new
client would take. This is what makes a SIGSTOPped backend fail its
checks: the kernel still completes the TCP handshake, and the request
times out. A kept-alive probe connection would also time out, but would
not notice a full accept queue. 2xx and 3xx are successes, and redirects
are not followed. The config rejects `timeout_ms` above `interval_ms`:
checks of one backend run one after another, and a longer timeout would
quietly stretch the interval. `interval_ms = 0` turns active checks off.

### Passive checks

These count as a failure in live traffic: a failed dial, a timeout or
reset or close before the response headers, a malformed response, a 5xx
response, and a response body that breaks off on the backend's side. Any
other response is a success and ends the run. These do not count: client
failures (a body that stalls or breaks off, a client that leaves while a
1xx is relayed), and failures while the server is closing, which the
proxy causes.

Counting 5xx follows the plan. The trade-off is that one endpoint
returning 500 on every call could eject a backend whose other endpoints
work. That takes `passive_failures` 5xx in a row with no success in
between, and active checks bring the backend back once the backoff ends.

The run counter is an atomic, so under concurrency "in a row" is
approximate: a success landing between two failures from other
connections resets it. That errs towards keeping a backend in. A success
loads the counter before storing zero, so busy healthy backends write
nothing and the cache line is not bounced between cores.

### Pools pick from a cached healthy set

`Balancer.Pick` has taken the healthy set since Phase 2. Each `lb.Pool`
keeps its healthy backends with the generation they were built at, behind
an `atomic.Pointer`. A pick loads the checker's generation and returns the
cached slice when it matches, so between health changes a pick costs two
atomic loads and allocates nothing (tested for every algorithm). After a
change, the first pick rebuilds the slice. The generation is read before
the states, so a slice is never kept that is older than its generation
says. With every backend healthy the pool's own slice is returned. The
ring already skips backends outside the healthy set, so only an ejected
backend's keys move.

### Retries on another backend

A request goes to another healthy backend it has not tried, up to
`upstream.max_retries` (default 1, at most 2), only if no byte of a
response has reached the client and:

| Failure | Retried |
| --- | --- |
| Dial refused or timed out | Any method, body or not: nothing was sent and no body byte read from the client |
| Reset, close, timeout or malformed response before the headers | Only GET, HEAD, OPTIONS, TRACE without a body |
| A response, any status, 5xx included | Never; it is an answer, and it is relayed |
| A 1xx already relayed | Never: the client is reading this backend's answer |
| Client failures, server closing | Never |

POST is never retried after its bytes may have reached a backend, because
the backend may have acted on it before failing. A body streams from the
client as it is sent, so a request with a body can be retried only before
its first byte is read, which means only after a failed dial.

Timeouts are retried, so a GET to a SIGSTOPped backend takes up to
`upstream_response_ms` and then succeeds elsewhere, instead of failing.
That is nginx's default (`proxy_next_upstream error timeout`). The
trade-off is up to twice the timeout for that request, against an error.
5xx responses are not retried. Retrying them would multiply load on a
pool that is failing because it is overloaded.

The Phase 4 retry (a stale kept-alive connection, retried on a new
connection to the same backend) stays separate and does not count against
`max_retries`, because it says nothing about the backend's health.

What bounds a retry storm is the low cap and ejection: once a backend is
out, its requests stop failing, so they stop being retried. A retry
budget (retries capped as a fraction of requests) is the plan's stretch
goal. `proxy_retries_total` shows the retry rate by reason.

When no backend is healthy the proxy answers 503 at once and dials
nothing. The rejected alternative is Envoy's panic mode, which balances
across every backend when too few are healthy. It helps when health
checks are wrong, at the cost of sending traffic to backends that are
known to be dead.

### Metrics: atomics on the request path, one collector at scrape time

The request path never asks the Prometheus client for a labelled child
(`WithLabelValues` hashes the labels and takes a read lock). Each request
already holds its pool and backend, so the counts are plain atomics on
`metrics.PoolStats` and `metrics.BackendStats`. One unchecked collector
reads them, the upstream pools' dial and reuse counters, and the health
targets at scrape time. The latency histogram is the one Prometheus type
on the hot path. Each pool's child is resolved once per snapshot, and
`Observe` is lock-free. Status codes are counted exactly, not by class,
in a fixed array per backend (100–599, plus "other"). 502, 503 and 504
mean different things here, and the array costs 4 KB per backend.
Requests that reach no pool are counted with empty pool and backend
labels; requests answered without a backend (503 when none is healthy)
with an empty backend.

Stats live in `metrics.Metrics`, keyed by pool name and address, not in
the snapshot, so a snapshot rebuilt by Phase 7's reload gets the same
counters and they never go backwards. The in-flight counter that
least-connections reads moved into `BackendStats` for the same reason.
`BenchmarkProxy`, now wired like the binary, allocates 35 times per GET
and 42 per POST with reuse, the same as Phase 4. Wiring it this way first
showed 36 and 43: an `errors.As` target escaping to the heap on the
success path, now reached only on errors.

Metrics: `proxy_requests_total{pool,backend,code}`,
`proxy_request_duration_seconds{pool}`,
`proxy_requests_in_flight{pool,backend}`,
`proxy_upstream_connections_total{backend,kind=dial|reuse}`,
`proxy_upstream_idle_connections{backend}`,
`proxy_backend_healthy{backend}`, `proxy_backend_state{backend,state}`,
`proxy_backend_transitions_total{backend,to}`,
`proxy_retries_total{pool,reason=stale_conn|dial|error|timeout}`,
`proxy_client_connections`, and the Go and process collectors
(`go_goroutines` is the goroutine count). The redirect listener's 308s
are not counted, because they never reach a pool.

The Go test reads the text exposition rather than importing
`prometheus/client_model`, which would have made that module a direct
dependency. It is still in `go.mod`, as an indirect dependency of
`client_golang`.

### Admin: /metrics, /status, and Prometheus in the proxy's namespace

The admin listener adds `/metrics` and `/status` (JSON: each pool with its
algorithm, healthy count and each backend's state and in-flight count;
each address once with its health, the current runs, the last check,
transitions by state, and its idle, dialled and reused connections). It
still binds only loopback, because pprof exposes memory. In the lab,
Prometheus runs with `network_mode: service:proxy`, so it shares the
proxy container's network namespace and scrapes `127.0.0.1:9901`. The
rejected alternative was a second, public listener serving only
`/metrics`: more config, and a second port to secure.

### Test backend: /healthz can fail on its own

Phase 0 made `-fail-rate` apply to `/healthz` too and said a separate
switch would come if Phase 5 needed one. It did: "accepts TCP but answers
500 on /healthz" must be caught by active checks alone, while traffic
still works. `HealthzFail` / `-healthz-fail` / `SetHealthy` make `/healthz`
answer 500 and leave every other endpoint alone.

### Tests

- `internal/health`: the flapping table and the state machine. Against
  httptest backends: fall and rise, refused, timeout, 3xx, passive
  ejection with active re-admission and without active checks, and a run
  broken by a success. `Sync` removing an address stops its prober, which
  goleak checks against a baseline that includes the other probers.
- `internal/lb`: healthy-set changes, the ring skipping an ejected node
  without moving other keys, exclusion on retry, zero allocations.
- `internal/conn`: every row of the retry table, the retry limit, 503
  without a dial, passive ejection from 5xx and from dial failures, client
  stalls not counting, request counts. A request's outcome reaches health
  after its response reaches the client (the client is not kept waiting
  on bookkeeping) and before its access-log line. So a test that reads
  health state, or needs reports in request order, waits for each
  request's access-log line, not just its response. The 5xx ejection
  test once checked the state as soon as the third 500 arrived and failed
  in CI when the report had not landed yet; a 20 ms sleep before the
  report reproduces that every time.
- `test/integration` runs backends as real processes (`ProcBackend`), so
  `kill -9` and SIGSTOP are the real thing: the kernel resets or silences
  the sockets. The harness turns health checks off unless a test asks for
  them, so earlier tests see no probe requests and no ejections.
- `bench/phase5/failover.sh kill|stop`: wrk against a `-race` build with
  `kill -9` or SIGSTOP on one of three backends, then a restart or
  SIGCONT. Ejection and re-admission are timed from `/metrics`, and
  failures are split by phase from the access log.
- `lab/prometheus-check.sh`, run by `check-lab.sh` and CI: the scrape
  target is up, three backends report healthy, and requests through the
  proxy show up in `proxy_requests_total`.

### Results

On the development machine (WSL2, Core Ultra 7 155H):

- `kill -9` under wrk (32 connections, `-race` build): b2 was out of
  rotation within 30 ms. Polling `/metrics` every 10 ms is the resolution;
  the in-process test measures 2.5 ms. 0 requests failed during detection
  and 0 after ejection, of 256k: 16 requests were retried after failed
  dials and 8 on new connections. After the restart, b2 was back in
  rotation 613 ms later (`rise` 2 at a 500 ms interval).
- SIGSTOP: ejected after 1.03 s (the 1 s upstream response timeout, then
  passive ejection), 0 failed requests, 32 retried after timeouts (one in
  flight per connection). p99 was 741 ms, from the requests that waited
  out the timeout. Back in rotation 534 ms after SIGCONT.
- Allocations per request: unchanged (above).

## Phase 6: Per-client rate limiting

### One bucket per route and client

PLAN.md sets `rate` and `burst` per route, so a bucket is keyed by (route,
client address): a client's traffic to `a.test` and to `b.test` is limited
separately, and a heavy user of one host does not use up its quota on
another. `[rate_limit]` gives the default for routes that set none and for
requests served by `default_pool`; a route's inline
`rate_limit = { rate, burst }` replaces it, and `rate = 0` turns limiting
off for that route. The rejected alternative, one bucket per client
across all routes, cannot express per-route limits at all. The cost is one
bucket per route a client uses, not one per client.

Route identities are interned by host name in the table (`""` for the
default pool), not by position in the config, so a snapshot rebuilt on
reload (Phase 7) finds every client's bucket where it left it, under the
new limit.

### A bucket is one atomic number

PLAN.md says to store the token count and the last refill time and
compute the refill on access. The first version did exactly that: two
`int64`s per bucket, tokens counted in nanoseconds of credit (one token is
one interval, `1e9/rate` ns), so a refill is one addition and the steady
rate is exact with no float drift. Each shard was a `sync.Mutex` over a
`map[key]bucket`.

The mutex profile under load showed the flaw. One client sending ~60k
requests per second over 100 connections puts every request on the same
bucket, so on the same shard lock. Identical runs put the limiter at 3%,
4%, 81% and 91% of all mutex wait. Once a waiter has waited over a
millisecond, Go's mutex switches to starvation mode and hands the lock to
waiters strictly in order, and each handoff then waits for the woken
goroutine to be scheduled: a convoy. Any lock on a per-key state would do
this for one hot key, and one hot key is exactly what an abusive client
is.

So the two fields are folded into one: `full`, the time at which the
bucket will have refilled to capacity. At any time
`tokens = capacity - max(0, full - now)`, and spending one request moves
`full` to `max(full, now) + interval`. This is the same bucket, encoded so
that one compare-and-swap updates it; the form is known as GCRA (generic
cell rate algorithm). The arithmetic is still integer nanoseconds and
exact. A clock that steps back earns nothing. When a limit shrinks on
reload, credit spent under the larger capacity is still owed.

Each shard is now a `sync.RWMutex` over `map[key]*bucket`. A request for a
known client takes the read lock, which only blocks while a writer holds
it, and spends with CAS. The write lock is for adding a bucket, evicting
one and sweeping. The spend happens under the read lock, so a sweep never
deletes a bucket while a request is spending from it. Racing spends on one
bucket retry their CAS; a test races eight goroutines on one bucket at one
instant and gets exactly the burst. After the change the limiter's share
of mutex wait in the single-hot-client run was 0 to 0.03 ms in four runs
(607 ms before), and 0.96% in the full bench. `Allow` costs 88 ns on four
cores for one hot client (132 ns before) and 32 ns for many clients (57
ns), with no allocation for a known client.

The costs: one 8-byte allocation per new client, which the GC must track
through the map's pointers, and a departure from the plan's literal
representation. The alternatives were a per-bucket mutex (keeps two fields
but still convoys on the hot key's own mutex) or keeping the mutex and
documenting the convoy (the done-when check would stay flaky).

### Shards: 256, seeded, padded

A key is `{route uint32, addr [16]byte}`; `hash/maphash` with a per-table
random seed picks the shard, so a client choosing its addresses cannot aim
them all at one shard. Each shard is padded to 64 bytes so two shards'
locks never share a cache line.

The shard count is set by the write-locked work, not by request
contention, since known clients only read-lock. A sweep walks one shard
under its write lock; at a million entries a shard of 256 holds about
4,000, a pause well under 100 µs for the requests that hash there. More
shards would shorten that pause further but cost a cache line each and
make the per-shard cap coarser at small `max_entries`. 256 is also the
smallest `max_entries` the config accepts, one bucket per shard.

### Bounded memory: cap, evict the nearest-to-full, sweep

`max_entries` is split evenly across shards. When a new client arrives at
a full shard, eight entries are sampled; Go randomises where map
iteration starts, so the sample is spread over the shard. A sampled bucket
that has already refilled is dropped, which loses nothing. Otherwise the
one closest to full goes, which forgives the least debt. This fails open
for that one client: if it returns, it gets a fresh, full bucket. The
rejected alternative, refusing clients the table has no room for, fails
closed: an attacker with enough addresses to fill the table would lock
out every new client. Evictions are counted
(`proxy_rate_limit_evictions_total`), so a cap set too low shows.

The sweeper is one goroutine per table, on a ticker (`sweep_ms`, default
10 s), stopped by `Table.Close`. `run` owns it and defers the Close, and
goleak checks it in tests. It drops every bucket whose `full` has passed,
since a full bucket is the same as no bucket. Only clients seen within one
refill time (`burst / rate`) hold memory.

Go maps never give memory back on delete, so a sweep rebuilds a shard's
map once it falls below a quarter of its peak. Measured: with
`max_entries` 100,000, a million distinct keys grow the heap by 11.5 MB,
115 bytes per capped entry. That is a 32-byte map slot plus a control
byte, and the 8-byte bucket; under eviction churn, deleted-slot markers
keep the Swiss-table map at about 2.5 slots per live entry. A second
million keys add 1%. The default cap is 100,000, about 12 MB.

### The client's address

The key is the TCP peer's address, IPv4-mapped addresses unmapped. An
IPv6 client is keyed by its /64, since one host usually holds a whole /64
and could rotate through it. The cost is that hosts sharing a /64 share a
limit.

`X-Forwarded-For` is ignored unless the peer is in `trusted_proxies`
(addresses or CIDR prefixes, top level, since the per-IP connection cap
uses it too and Phase 8's header rewrite will). Anyone can write the
header, so believing it from an arbitrary client would let every request
claim a new identity. From a trusted proxy, the header is read right to
left: trusted addresses are hops, and the first untrusted one is the
client. Everything left of it was written by the client and is never read.
If every address is trusted, the leftmost is the client. An entry that is
not an address, met before the client is found, gives the peer: a broken
chain never lets the client choose its key. Ports on entries are ignored.
The walk allocates nothing.

What the proxy sends upstream in `X-Forwarded-For` is unchanged (it
appends the peer); Phase 8 overwrites it for untrusted clients.

### Where the check runs, and the 429

The check runs after routing and before backend selection: 421 and 404
come first (a request with no route has no limit to apply), and a refused
request costs no balancer pick and no upstream connection. With no
`default_pool`, unknown hosts get an unlimited 404, which is cheap.

The 429 carries `Retry-After`: the time until one request's credit
returns, rounded up to whole seconds, at least 1. A request without a body
keeps its connection, since closing it would make a client that retries
pay for a new TCP and TLS handshake every time: the CPU the limit is meant
to protect. A request with a body closes the connection with the
lingering close other refusals use, because the unread body is still on
it. The writer is fixed text plus one number, built without allocating,
since an over-limit client can ask for it as fast as it likes. The access
log line has status 429 and `error: "rate limited: client <addr>"`,
naming the address the limit applied to, which differs from `client` when
a trusted proxy forwarded the request.

### Per-IP connection cap at accept

`limits.max_conns_per_ip` is enforced in the connection tracker, under the
mutex `Add` already takes for the total cap, as a `map[netip.Addr]int`
keyed like the buckets (IPv6 per /64). So it adds no new shared state:
the tracker was already shared between the accept loop and the connection
goroutines. A connection past the cap is closed at accept, before the TLS
handshake, which is the CPU this protects. Trusted proxies are exempt,
since they carry many clients' connections. Refusals are counted by reason
(`proxy_client_connections_rejected_total{reason="max_conns"|"per_ip"}`).
Per-IP refusals are logged at debug level only, because one abusive
client at its cap would otherwise flood the log. Like `max_conns`, the cap
applies from startup and is not reloaded. Both are off by default (0);
the lab sets 2,048, enough for Phase 9's runs at concurrency 1,000.

### Metrics

- `proxy_rate_limited_requests_total{route}`: 429s by route host, `""`
  for the default pool. A plain atomic on the request's target, read at
  scrape time like the Phase 5 counters.
- `proxy_rate_limit_buckets` and `proxy_rate_limit_buckets_max`: table size
  and cap.
- `proxy_rate_limit_evictions_total`.
- `proxy_client_connections_rejected_total{reason}`.

The 429s are also in `proxy_requests_total` under their pool with an empty
backend, like 503.

### Tests and benches

- `internal/ratelimit`:
  - with an injected clock: burst then exactly the rate, each acceptance
    on the 200 ms mark; refill capped at the burst; partial waits; a clock
    going backwards; a shrinking limit; the token count derived from
    `full`.
  - concurrency: eight goroutines on one bucket get exactly the burst.
  - table behaviour: clients and routes separate; state kept across
    limiters for one route; IPv6 /64 keying; sweep, map shrink, cap and
    eviction (preferring full buckets).
  - memory: a million keys against a 100,000 cap, then a second million.
  - zero allocations; the `X-Forwarded-For` walk, table-driven.
- `internal/conn`:
  - 429 with `Retry-After` on a kept connection, and HEAD.
  - unlimited routes, the default pool's own bucket.
  - closing on a body, `Connection: close` or HTTP/1.0.
  - `X-Forwarded-For` ignored from untrusted peers and used from trusted
    ones.
  - the per-IP cap with 127.0.0.2 as a second address.
- `test/integration/phase6_test.go`, over HTTP and HTTPS:
  - an open-loop client at twice the limit (100/s, burst 10) for 3 s,
    with a second client from 127.0.0.2 at half: 309 accepted against
    310 expected, the second client never limited, `/metrics` matching.
  - per-route limits.
  - the per-IP cap at accept, before the handshake.
- `bench/phase6/k6.sh` (lab): k6 constant-arrival-rate at 400/s from
  `loadgen` and 100/s from `loadgen2` against `limited.test` (200/s,
  burst 20) for 60 s.
- `bench/phase6/race-profile.sh`: two wrk runs at once, first against a
  `-race` build, then against a normal build with a 10 s mutex and CPU
  profile.
  - one client (100 connections, one bucket) on the default pool, limited
    to 10^6/s so nothing is refused.
  - a random `X-Forwarded-For` per request from a trusted 127.0.0.1 to
    `many.test` (1/s, burst 10), so each new client's bucket stays short
    of full and the 4,096-entry table evicts the whole time, with the
    sweeper running every 100 ms.

WSL2 steps the wall clock, by about 2.5 s every half minute; measured
against `time.monotonic`. Three tools time with the wall clock and were
fooled until handled:
- k6's reported rates. `k6.sh` divides by the scheduled duration and
  checks that k6 sent every scheduled iteration.
- wrk's timeouts. A backward step makes the request in flight on each
  connection look timed out: exactly one per connection. `race-profile.sh`
  measures the step, tolerates at most one "timeout" per connection when
  there was one, and requires every line of the proxy's access log, which
  times with the monotonic clock, to be a 200.
- `UnixNano` in a test. The integration test measures its own span with
  `time.Since`.

The proxy itself only uses monotonic time for deadlines and buckets.

### Results

On the development machine (WSL2, Core Ultra 7 155H):

- k6 in the lab, 400/s against a 200/s limit for 60 s: 12,019 accepted,
  200.32/s (+0.16%), 199.98/s after the burst. 11,982 got 429, matching
  `proxy_rate_limited_requests_total`. The second client: 6,001 of 6,001
  accepted. p99 1.0 ms and 1.2 ms.
- `-race` build, 30 s: 0 data races, 390,825 requests all 200 in the
  access log, the table at most 3,942 of 4,096 buckets with 63,448
  evictions. wrk counted 97 and 50 "timeouts", one per connection, from a
  2.48 s clock step during the run.
- Mutex profile, normal build, 35.8k req/s for one client plus 17.7k
  req/s for random clients: 169.5 ms of mutex wait in 10 s, 1.62 ms
  (0.96%) of it in the rate limiter. The largest site is the access log's
  `slog` handler (83%), as in Phase 4.

## Phase 7: Hot reload and graceful shutdown

### `internal/app` owns the process; `main` owns the signals

Everything `cmd/proxy`'s `run` used to build (registry, checker,
rate-limit table, metrics, connection server, snapshot store, listeners,
admin server) moved into `app.App`, with `New`, `Start`, `Reload` and
`Shutdown`. `main` is left with flags and one loop on its own goroutine
that reads a `signal.Notify` channel: `SIGHUP` calls `Reload`, `SIGTERM`
and `SIGINT` call `Shutdown`, and a listener that fails on its own (on
`App.Errors`) shuts down too and exits 1. The reason is testing: the
reload and drain logic is driven in-process by `internal/app` and the
Phase 7 integration tests, and only the signal wiring needs a subprocess
(`cmd/proxy/signal_test.go`). `Reload` and `Shutdown` take one mutex, so
two reloads, or a reload and a shutdown, never interleave. The channel
holds four signals, so a `SIGTERM` that arrives during a reload is not
lost.

The integration harness from earlier phases still wires the proxy by
hand; moving it onto `App` would touch every earlier test for no gain in
coverage.

### Reload: build everything, then one pointer swap

Each step that can fail runs before anything changes:

1. `config.Load`: parse and validate. A broken file fails here.
2. `KeepStartup` (below), then `NewSnapshot`: pools, balancers, rings,
   limiters, and every certificate read from disk. A missing or
   mismatched certificate fails here.
3. Open sockets for the listeners the new file adds. A busy port fails
   here.

Then the swap, which cannot fail: `Store.Store(snap)`, the tracker's new
limits, new handlers for kept listeners, accept loops for added ones,
`CloseListener` for removed ones. A request loads the snapshot pointer
once, at its first byte, and keeps that snapshot to its end, so a request
in flight finishes on the old config and the next one on any connection
uses the new. Readers take no lock: the pointer is one atomic load, the
snapshot is never written after `NewSnapshot`, and the garbage collector
keeps the old snapshot alive for exactly as long as a request's stack
still points at it.

Last, whatever happened, `retain` makes the process-wide state match the
snapshot in force: `Checker.Sync` stops the probers of removed addresses,
`Registry.Retain` closes their idle connections and drops their pools,
and `Metrics.Retain` drops their series. Running this after a failed
reload matters because step 2 already created health targets and pools
for the new file's addresses; without it a broken config naming new
backends would leave probers running for them.

A failure is logged at error level with the reason, counted in
`proxy_config_reloads_total{result="failure"}`, and sets
`proxy_config_last_reload_successful` to 0 until the next good reload,
the same pair Prometheus exposes for its own config.

### What carries across a reload, and why it is free

Earlier phases put the state that should outlive a snapshot outside it,
keyed by name, so a rebuilt snapshot finds it again:

| State | Kept in | Key |
| --- | --- | --- |
| Health state and its prober | `health.Checker` | backend address |
| Idle upstream connections | `upstream.Registry` | backend address |
| In-flight count, request counts | `metrics.Metrics` | pool name, backend address |
| Token buckets | `ratelimit.Table` | route host, client address |
| TLS session ticket keys | the one `tls.Config` | process |

PLAN.md's Phase 2 note expected the new snapshot to be built from the old
one, reusing `lb.Backend` values. That was not needed: the in-flight
counter lives in the metrics stats, which `NewSnapshot` already fetches by
pool and address, so a backend in both configs gets the same counter and
least-connections sees the requests still running on the old snapshot.
`TestReloadCarriesBackendState` checks every one of these by identity.

A backend removed and added again (the alternating test does this every
second) starts fresh: a new prober, which treats it as healthy and checks
at once, an empty idle pool, and counters from zero.

### Removed backends: dropped, not kept

A removed address's prober stops, its idle connections close and its
series leave `/metrics`, so `/metrics` and `/status` show only what is
configured. The cost: a request still in flight on the old snapshot
counts into stats nothing exports, and a backend that comes back has its
counters reset, which Prometheus reads as a counter reset (`rate()`
handles it). Keeping removed backends' series forever was the alternative;
it makes `/metrics` grow with every address ever configured.

Two details keep a request on an old snapshot from undoing the removal. A
connection put back to a closed pool is closed instead of parked. A
passive ejection reported for a removed address used to call
`reg.Pool(addr).Flush()`, which would have created the pool again; it now
uses `Registry.Lookup`, which never creates one.

### Startup-only settings: warn and keep

Some settings size things made once: the admin listener (`[admin]`), the
log buffer (`[log]`), the probers (`[health]`), the upstream pools
(`max_idle_per_backend`, `idle_timeout_ms`) and the bucket table
(`max_entries`, `sweep_ms`). Changing them on reload would mean rebuilding
those objects and losing what they hold. `Config.KeepStartup` copies the
running values into the new config before its snapshot is built and
returns the keys that differed. The reload goes ahead and logs one
warning naming them. The snapshot then holds the values actually in force,
so `/status` never shows a setting that is not applied. Rejecting the
whole reload was the alternative; it would block an unrelated certificate
renewal because someone also edited `[health]`.

`max_conns`, `max_conns_per_ip` and `trusted_proxies` did not need to be
on that list. The tracker's limits are now an atomic pointer that a
reload replaces, read once per accept. A lowered cap closes nothing;
connections over it end on their own.

### Listeners: matched by configured address

A listener's identity is its address as written in the file, plus its
position among listeners with the same address (only port 0 may repeat).
A reload keeps the socket of every listener whose identity is still
present. If only the mode or redirect port changed, it swaps the handler
through an atomic pointer that each new connection reads once at accept,
so a mode change never closes the socket and never refuses a connection.
Added listeners are opened before the swap, so a busy port fails the
reload with the old config intact. Removed ones are closed after it by
`Server.CloseListener`, whose accept loop then returns
`ErrServerClosed`; connections it already accepted carry on.

The cost of matching by string: `localhost:8443` to `127.0.0.1:8443` is a
remove plus an add, and the add fails because the old socket still holds
the port. Opening new sockets first is what makes a failed reload
harmless, so closing first is not an option. `SO_REUSEPORT` would allow
both at once but changes how the kernel spreads connections, which is not
worth it for a rename.

### Shutdown: close, wake, drain, cut

`conn.Server.Shutdown(ctx)`:

1. Close every listener and wait for the accept loops, so new connections
   are refused and nothing more is added to the tracker.
2. `Tracker.Drain`: set the draining flag, then set the read deadline of
   every idle keep-alive connection to now. A goroutine blocked in `Read`
   wakes with a timeout, sees the connection is done, and the server
   closes it. This is how to wake a goroutine blocked in `Read` without
   closing the socket under it.
3. Every response written while draining gets `Connection: close`, and
   the request loop stops after it.
4. Wait for the tracker's WaitGroup until `ctx` ends, then fall back to
   `Close`: close every client connection and every request's upstream
   connection.

The race in step 2 is a connection going idle as the drain starts. The
connection sets its state to idle, then its idle deadline, then checks
the flag. `Drain` sets the flag, then scans the states. Either the
connection sees the flag and returns, or `Drain` sees it idle and its
deadline of now is set after the connection's own, so it wins.
`TestShutdownIdleRace` runs the race fifty times.

A connection that has not sent its first request yet is left alone: the
client may already have sent it, and a request on a fresh connection that
gets reset cannot be safely retried. It is served with `Connection:
close`, bounded by the handshake and header timeouts and by
`shutdown_ms`. Idle keep-alive connections do have the race where the
client sends just as the proxy closes, as with any idle timeout, and HTTP
clients retry idempotent requests on a reused connection for that reason.

`shutdown_ms` (`[timeouts]`, default 8 s, 0 for no drain) is read from the
current snapshot when the signal arrives, so it reloads. 8 s leaves the
drain and the teardown inside `docker stop`'s 10 s grace period. Past it
the process still exits 0: it did what it was configured to do.

After the drain, the admin server stops (Prometheus can scrape during the
drain), the root context is cancelled, and each background goroutine's
`Close` waits for it.

### The hard close also cuts off the backend

Until now `Server.Close` closed only the client sockets. A request waiting
on a slow backend reads from the upstream connection, not the client, so
it kept running until `upstream_response_ms`. The drain deadline exposed
this: a 300 ms deadline took 5 s. Each `TrackedConn` now records the
upstream connection its request is using (an atomic pointer, set after
`Get` and cleared before the connection is parked or discarded), and
`CloseAll` closes it too. If `CloseAll` closes a connection as it is being
parked, the pool's liveness check discards it on the next `Get`. Nothing
on the request path allocates for it.

### One root context

`NewRegistryContext`, `NewCheckerContext` and `ratelimit.NewContext` take
a parent context. `App` derives all three from one root, so cancelling it
stops the reaper, every prober and the sweeper, and each `Close` then
waits for its goroutines. The old constructors remain, with
`context.Background()`. The root is cancelled only after the drain, so
health checks keep running while requests finish.

| Goroutine | Owner | Stopped by |
| --- | --- | --- |
| signal loop | `main` | `Shutdown` returning |
| accept loop per listener | `App` | `CloseListener` on reload, or `Shutdown` |
| one per client connection | `conn.Server` | its handler returning, the drain, or `Close` at the deadline |
| request-body writer | the connection | its request ending |
| prober per backend address | `health.Checker` | `Sync` removing it, or the root context |
| upstream reaper, rate-limit sweeper | `upstream.Registry`, `ratelimit.Table` | the root context |
| admin `Serve` | `App` | `admin.Server.Shutdown` after the drain |
| log flusher | `main` | its close function before exit |

### nginx does it with processes

On `SIGHUP`, nginx's master process starts new worker processes with the
new config and tells the old ones to stop accepting and exit once their
connections finish. Old and new configs run side by side in different
processes, and per-worker state (upstream keep-alive pools, health of
passive checks, `limit_req` zones outside shared memory) does not carry
over. Here old and new configs run side by side in one process, and the
state above does carry over, because it lives outside the snapshot and is
keyed by name. The cost is that a bug in a reload is a bug in the one
process serving traffic: a reload that panicked would take the proxy
down, where nginx would lose only the new workers. Phase 8's per-
connection `recover` does not cover the reload goroutine; a reload runs on
the main goroutine and only calls code that also runs at startup.

### Tests

- `internal/conn`: draining an in-flight request (completes with
  `Connection: close`, new dials refused), idle connections closed at
  once, the idle race fifty times, the deadline cutting a request waiting
  on a backend, a fresh connection's first request served while draining,
  `CloseListener`, and reloaded tracker limits and exemptions.
- `internal/app`: a reload swapping the pool on a kept-alive connection;
  state carried by identity; a request in flight across a reload; five
  broken configs (TOML syntax, validation, missing certificate, busy
  port, missing file) each leaving the old config serving, logged,
  counted, with no prober or pool left for the backends they named;
  listeners added, removed and switched to redirect; startup-only
  warnings; shutdown draining and timing out; a reloaded `shutdown_ms`.
- `cmd/proxy/signal_test.go`: the real binary and real signals. `SIGTERM`
  during a 5 s response (200 with `Connection: close`, dial refused at
  once, exit 0 within 500 ms of the response); `shutdown_ms = 1000`
  against the same response (exit 0 between 1.0 and 1.5 s); `SIGHUP` with
  a broken then a good file, read back from `/metrics`. A `-race` binary
  sleeps a second at exit by default, so the subprocess runs with
  `GORACE=atexit_sleep_ms=0`, or every exit would look a second late.
- `test/integration/phase7_test.go`: 60 reloads every 50 ms alternating
  pool membership under eight keep-alive clients over HTTPS with active
  checks every 50 ms, no failed request; certificate rotation (a new
  handshake gets the new serial, an open connection keeps working); 1,100
  reloads with heap and goroutines compared after the first 100;
  `goleak.VerifyNone` after shutdown with idle connections, a request in
  flight, probers, reloads and the admin listener behind it.
- `bench/phase7/reload-wrk.sh`: the 60 s wrk run with a `SIGHUP` a second.
  `RELOADS=off` is the same run without signals, as a control.
- `lab/stop-check.sh`: `docker compose stop proxy` with an idle and a
  busy connection, in CI after the lab check.

The first heap measurement over 1,000 reloads grew by 0.9 MB. A heap
profile put it all in the in-process test backends' request log, filled
by health probes, not in the proxy, so the test turns that log off
(`BackendOptions.NoRecord`). The remaining difference, 3 to 170 KB
between runs, is probes and client connections in flight when the heap is
read.

### Results

On the development machine (WSL2, Core Ultra 7 155H):

- `reload-wrk.sh`, `-race` build, 60 s, 32 connections: 59 reloads
  applied, 0 failed, reload time 0.7 / 1.6 / 27 ms (min / median / max).
  0 non-2xx responses; 613,796 access-log lines, every one a 200; all
  three backends served. wrk counted 64 "timeouts" and no other socket
  error: the wall clock stepped twice (−5.0 s), one in-flight request per
  connection per step. The control run without reloads counted the same
  64 for two steps, and 32 in a run with one step. The script counts
  steps once a second and allows one timeout per connection per step, as
  `race-profile.sh` does in Phase 6.
- 1,000 reloads: heap +3 KB to +170 KB across runs, goroutines 10 before
  and 10 after.
- `docker stop` in the lab, with a 3 s request 1 s in: 2.35 s, exit code
  0, the request answered 200 with `Connection: close`, the drain
  2.0 s.

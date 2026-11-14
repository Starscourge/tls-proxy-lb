# tls-proxy-lb

An HTTP/1.1 reverse proxy and load balancer in Go that terminates TLS, routes
by SNI and balances requests across backend pools.

The request path is built on `net` and `crypto/tls` directly. From `net/http`
it uses only the parsers and serialisers (`ReadRequest`, `ReadResponse`,
`Request.Write`, `Response.Write`, `Header`). `http.Server`, `http.Transport`,
`http.Client` and `httputil.ReverseProxy` are not used on the request path.

**Status:** Phase 6 (HTTPS termination with TLS 1.2 and 1.3; SNI picks the
certificate and the pool on one port; round-robin, least-connections or
consistent hashing across each pool, from a TOML config file; upstream
connections kept alive and reused through the proxy's own pool; active and
passive health checks that eject failing backends and re-admit them;
retries on another backend; per-client token-bucket rate limits per route
with 429 and `Retry-After`, and a per-IP connection cap; Prometheus
`/metrics`, a JSON `/status` and pprof on a loopback admin listener). Hot
reload and graceful shutdown arrive in Phase 7.

## Layout

```text
cmd/proxy/         the proxy
cmd/testbackend/   test backend for tests and the lab
internal/          proxy packages (conn, httpx, tlsx, lb, upstream, health,
                   metrics, ratelimit, config, admin, logbuf) and the test
                   backend library
test/integration/  end-to-end tests and their helpers
bench/             benchmark and demo scripts, raw results
lab/               Docker Compose lab, certificate script
docs/              design decisions, testssl.sh report
```

## Requirements

Linux (WSL2 works), Go as pinned in `go.mod`, Docker with Compose v2,
`openssl`, and `staticcheck` for `make lint`.

## Commands

```sh
make build              # go build ./... and binaries in bin/
make test               # go test -race ./...
make lint               # gofmt, go vet, staticcheck
make certs              # local CA and certificates for a.test, b.test, default.test in lab/certs
make check-config       # validate lab/proxy.toml (or CONFIG=path), like nginx -t
make bench-lb           # balancer benchmarks; fails if a pick allocates
make testssl            # testssl.sh against the lab proxy; report in docs/testssl-phase3.txt
make lab-up             # proxy, three backends and a load generator in Docker
make lab-check          # bring the lab up and check backends, proxy routes, Prometheus and netem
make prom-check         # Prometheus in the lab scrapes the proxy's /metrics (lab must be up)
make lab-down
bench/phase1/wrk-race.sh   # wrk -c 200 -d 30s against a -race proxy build
make bench-allocs       # allocations per request (bench/phase4/results/allocs-*.txt)
make bench-reuse        # wrk with upstream reuse off and on, new connections per second
make wrk-race-reuse     # wrk GET and POST /echo against a -race build, reuse on
make bench-scaling      # throughput at GOMAXPROCS 1, 2 and 4
make bench-profile      # mutex, block and CPU profiles under load
make bench-kill         # kill -9 one of three backends under wrk (-race build), then restart it
make bench-stop         # SIGSTOP one of three backends under wrk (-race build), then SIGCONT
make bench-ratelimit-k6   # k6 at twice the rate limit plus a second client, in the lab (lab must be up)
make bench-ratelimit-race # -race run and mutex profile with the rate-limit table under load
```

## Running the proxy

```sh
make certs                                    # lab/certs: ca.crt, a.test.{crt,key}, ...
go run ./cmd/testbackend -listen 127.0.0.1 -port 8000 -name b1 &
go run ./cmd/testbackend -listen 127.0.0.1 -port 8001 -name b2 &
cat > lab/certs/proxy.toml <<'TOML'
default_pool = "web"

[[listener]]                    # HTTPS
address = "127.0.0.1:8443"

[[listener]]                    # plain HTTP redirects to it
address       = "127.0.0.1:8080"
mode          = "redirect"
redirect_port = 8443

[[route]]
host = "a.test"
pool = "web"
cert = "a.test.crt"             # relative to this file
key  = "a.test.key"

[pool.web]
algorithm = "round_robin"
backends  = [{ address = "127.0.0.1:8000" }, { address = "127.0.0.1:8001" }]
TOML
go run ./cmd/proxy -config lab/certs/proxy.toml -check-config
go run ./cmd/proxy -config lab/certs/proxy.toml
curl -si --cacert lab/certs/ca.crt --resolve a.test:8443:127.0.0.1 https://a.test:8443/ | grep X-Backend
curl -si -H 'Host: a.test' http://127.0.0.1:8080/x   # 308, Location: https://a.test:8443/x
```

Flags: `-config` (required) and `-check-config`, which validates the file,
loads its certificates and exits 0, or 1 with every problem listed by
field (`pool.web.backends[1].address: "127.0.0.1" is not host:port:
missing port in address`, `route[0].cert: certificate is not valid for
"b.test" (names [a.test])`). Certificates expiring within 30 days are
reported as warnings. One JSON access-log line per request goes to
stdout, through a buffer flushed every second and on exit, with the host, pool, backend and, for HTTPS, the SNI, TLS version
and whether the session was resumed.

### Config file

```toml
default_pool = "web"            # optional: hosts with no route; else 404
trusted_proxies = ["10.0.0.0/24"]  # optional: X-Forwarded-For is believed only from these

[[listener]]                    # one or more
address = "0.0.0.0:8443"
mode    = "https"               # https (default) | http | redirect
# redirect_port = 443           # redirect only: port in the https:// URL (default 443)

[[route]]                       # exact host match, case and port ignored
host = "a.test"
pool = "web"
cert = "certs/a.test.crt"       # PEM chain and key, relative to this file;
key  = "certs/a.test.key"       # needed on HTTPS unless [tls] has a default
rate_limit = { rate = 20, burst = 10 }  # optional: replaces [rate_limit]'s; rate = 0 turns it off

[tls]                           # optional
default_cert = "certs/default.crt"  # for unknown or missing SNI; without it
default_key  = "certs/default.key"  # those handshakes fail (unrecognized_name)

[pool.web]
algorithm = "consistent_hash"   # round_robin (default) | least_conn | consistent_hash
hash_key  = "header:X-User-Id"  # consistent_hash only: client_ip (default) | header:<Name>
backends  = [
  { address = "10.0.0.11:8000" },
  { address = "10.0.0.12:8000" },
]

[timeouts]                      # all optional; defaults shown
handshake_ms         = 5000     # TLS handshake on a new connection
header_read_ms       = 5000
idle_ms              = 60000
client_io_ms         = 60000    # longest stall on a client body or response
upstream_connect_ms  = 1000
upstream_response_ms = 10000
shutdown_ms          = 8000     # SIGTERM drain before closing the rest; 0 = none

[limits]                        # optional; defaults shown
max_conns        = 10000
max_header_bytes = 65536
max_conns_per_ip = 0            # per client address (IPv6: per /64), at accept; 0 = no cap

[rate_limit]                    # optional; defaults shown (limiting off)
rate        = 0                 # requests per second per client, for routes that set none
                                # and the default pool; 0 = off; fractions allowed
# burst     = 50                # required when rate is set
max_entries = 100000            # buckets held across all routes (about 115 bytes each)
sweep_ms    = 10000             # how often buckets that have refilled are dropped

[upstream]                      # optional; defaults shown
max_idle_per_backend = 100      # idle keep-alive connections per backend address; 0 = no reuse
idle_timeout_ms      = 30000    # idle connections older than this are closed
max_retries          = 1        # other backends a failed request may go to, 0 to 2

[health]                        # optional; defaults shown; applies to every backend address
path             = "/healthz"   # active checks: GET this; 2xx or 3xx is a success
interval_ms      = 2000         # between checks of one backend; 0 turns active checks off
timeout_ms       = 1000         # per check; at most interval_ms
rise             = 2            # successes in a row to become healthy
fall             = 3            # failures in a row to become unhealthy
passive_failures = 3            # failures in a row in live traffic that eject; 0 = off
eject_ms         = 10000        # an ejected backend stays out at least this long

[admin]                         # optional; no admin listener without it
address                = "127.0.0.1:9090"  # loopback only; /metrics, /status, /debug/pprof/
mutex_profile_fraction = 0      # >0 turns the mutex profile on (runtime.SetMutexProfileFraction)
block_profile_rate_ns  = 0      # >0 turns the block profile on (runtime.SetBlockProfileRate)

[log]                           # optional; defaults shown
buffer_bytes = 65536            # log lines are buffered; 0 writes each line at once
flush_ms     = 1000             # the most a line waits, and the most a crash can lose
```

Unknown keys are errors. TLS: versions 1.2 and 1.3 only; TLS 1.2 limited
to ECDHE with AES-GCM or ChaCha20-Poly1305; ALPN `http/1.1`; session
tickets for resumption. On HTTPS a request's `Host` must match its SNI,
or it gets 421; a connection without SNI is served the default
certificate and the default pool. testssl.sh finds no weak protocols or
ciphers (`docs/testssl-phase3.txt`).

What the proxy does so far: keep-alive to clients,
streaming bodies in both directions (`Content-Length` and chunked) with
backpressure, hop-by-hop header removal, `X-Forwarded-For` and
`X-Forwarded-Proto` (`http` or `https`), `100 Continue`, and 400, 404,
408, 421, 429, 431, 502, 503 and 504 on the failures they name. Consistent hashing uses 160 virtual nodes per
backend; requests without the hash header are keyed on the client IP. Each
backend address has a LIFO pool of idle keep-alive connections, shared by
every pool that lists the address. A connection is reused only after a
clean exchange; one the backend closed while idle is found by a
non-blocking peek before use, and a request that meets a connection dying
under it is retried once on a new one, if it has no body and is
idempotent or was never sent. The access log reports `reused` (and
`retried`). Reuse doubles throughput under `wrk -c 100` and brings new
upstream connections to about zero after warm-up
(`bench/phase4/results/`).

Health: each backend address is checked with `GET /healthz` every
`interval_ms` and leaves rotation after `fall` failures in a row, returning
after `rise` successes. Live traffic ejects it sooner: `passive_failures`
dial failures, timeouts, resets or 5xx responses in a row take it out at
once, for at least `eject_ms`, after which active checks decide. With no
healthy backend the proxy answers 503 without dialling. A request that
fails before any response byte reaches the client goes to another backend:
after a failed dial for any method, after a later failure only for GET,
HEAD, OPTIONS or TRACE without a body; the access log reports `retries`.
`kill -9` on one of three backends under wrk fails no requests, and the
backend is out of rotation within milliseconds; a SIGSTOPped one is out
after one response timeout (`bench/phase5/results/`).

Rate limiting: each route holds each client address to a token bucket of
`rate` per second and `burst` at once; an IPv6 client is one /64. The
check runs after routing and before a backend is chosen. A request over
the limit gets 429 with `Retry-After` in seconds, and the connection stays
open unless the request had a body. The client is the TCP peer;
`X-Forwarded-For` counts only when the peer is in `trusted_proxies`, and
then the client is its rightmost address that is not a trusted proxy.
Buckets live in one sharded table capped at `max_entries`; a full one is
dropped by the sweeper, and when the table is full the bucket closest to
full is evicted. `max_conns_per_ip` closes a client's connections past the
cap at accept, before the TLS handshake; trusted proxies are exempt. k6
at twice the limit from one client is accepted at 200.3/s against 200/s
while a second client is never limited (`bench/phase6/results/`).

Admin listener: `/metrics` (Prometheus: requests by pool, backend and
status, latency histogram by pool, in-flight requests, upstream dials and
reuses, backend health and transitions, retries by reason, client
connections and connections refused at accept by reason, rate-limited
requests by route, rate-limit table size and evictions, Go runtime), `/status` (JSON: pools and backends with their
health, last check and connection counts) and `/debug/pprof/`.

## Test backend

```sh
go run ./cmd/testbackend -port 8000 -name b1 -delay-ms 0 -fail-rate 0 [-healthz-fail]
```

Every response carries `X-Backend: <name>`. Endpoints: `/` (catch-all),
`/healthz`, `/echo` (streams the request body back), `/slow?ms=N`
(default 5000), `/bytes?n=N[&chunked=1]` (N bytes of a fixed pattern) and
`/stats` (connections accepted and open, and requests served, as JSON).
`-delay-ms` and `-fail-rate` apply to every endpoint except `/stats`;
`-healthz-fail` makes `/healthz` alone answer 500.
Each request other than `/stats` is logged as one JSON line.

## Lab

`lab/docker-compose.yml` puts the proxy (`10.0.0.10`, HTTPS on 8443 and a
redirect on 8080, configured by `lab/proxy.toml`: a round-robin default
pool with the `default.test` certificate, `a.test` on least-connections
and `b.test` on consistent hashing by `X-User-Id`, each with its own
certificate), `backend1`–`backend3` (`10.0.0.11`–`.13`, port 8000) and a
load generator with curl, wrk and k6 (`10.0.0.20`, the lab CA at
`/work/ca.crt`), and a second one (`loadgen2`, `10.0.0.21`) for checks
that need two client addresses, on one network. `limited.test` is
rate limited to 200 requests per second per client with a burst of 20,
and each client address may hold 2,048 connections. Prometheus runs in the proxy container's
network namespace, scrapes its loopback admin listener every 2 s, and
serves its UI on `10.0.0.10:9090` inside the lab network. `make lab-up` creates the certificates if
needed and runs the proxy container as you, so it can read the keys in
`lab/certs`. Backends have `NET_ADMIN`, so traffic shaping works inside
them:

```sh
docker compose -f lab/docker-compose.yml exec backend1 tc qdisc add dev eth0 root netem delay 100ms
docker compose -f lab/docker-compose.yml exec loadgen curl -w '%{time_total}\n' -o /dev/null -s http://10.0.0.11:8000/
docker compose -f lab/docker-compose.yml exec loadgen curl -s -o /dev/null -D - --cacert /work/ca.crt \
  --resolve a.test:8443:10.0.0.10 https://a.test:8443/
docker compose -f lab/docker-compose.yml exec backend1 tc qdisc del dev eth0 root
```

netem delays every packet the backend sends, so a new connection pays the delay
twice (SYN-ACK and response). See `docs/DESIGN.md`.

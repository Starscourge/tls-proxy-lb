# tls-proxy-lb

An HTTP/1.1 reverse proxy and load balancer in Go that terminates TLS, routes
by SNI and balances requests across backend pools.

The request path is built on `net` and `crypto/tls` directly. From `net/http`
it uses only the parsers and serialisers (`ReadRequest`, `ReadResponse`,
`Request.Write`, `Response.Write`, `Header`). `http.Server`, `http.Transport`,
`http.Client` and `httputil.ReverseProxy` are not used on the request path.

**Status:** Phase 3 (HTTPS termination with TLS 1.2 and 1.3; SNI picks the
certificate and the pool on one port; round-robin, least-connections or
consistent hashing across each pool, from a TOML config file). Upstream
connection reuse arrives in Phase 4.

## Layout

```text
cmd/proxy/         the proxy
cmd/testbackend/   test backend for tests and the lab
internal/          proxy packages (conn, httpx, tlsx, lb, upstream, health,
                   ratelimit, config, admin) and the test backend library
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
make lab-check          # bring the lab up and check backends, proxy routes and netem
make lab-down
bench/phase1/wrk-race.sh   # wrk -c 200 -d 30s against a -race proxy build
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
stdout, with the host, pool, backend and, for HTTPS, the SNI, TLS version
and whether the session was resumed.

### Config file

```toml
default_pool = "web"            # optional: hosts with no route; else 404

[[listener]]                    # one or more
address = "0.0.0.0:8443"
mode    = "https"               # https (default) | http | redirect
# redirect_port = 443           # redirect only: port in the https:// URL (default 443)

[[route]]                       # exact host match, case and port ignored
host = "a.test"
pool = "web"
cert = "certs/a.test.crt"       # PEM chain and key, relative to this file;
key  = "certs/a.test.key"       # needed on HTTPS unless [tls] has a default

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

[limits]                        # optional; defaults shown
max_conns        = 10000
max_header_bytes = 65536
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
408, 421, 431, 502 and 504 on the failures they name. Consistent hashing uses 160 virtual nodes per
backend; requests without the hash header are keyed on the client IP. Each
request opens a new upstream connection; reuse is Phase 4.

## Test backend

```sh
go run ./cmd/testbackend -port 8000 -name b1 -delay-ms 0 -fail-rate 0
```

Every response carries `X-Backend: <name>`. Endpoints: `/` (catch-all),
`/healthz`, `/echo` (streams the request body back), `/slow?ms=N`
(default 5000) and `/bytes?n=N[&chunked=1]` (N bytes of a fixed pattern).
`-delay-ms` and `-fail-rate` apply to every endpoint. Each request is logged
as one JSON line.

## Lab

`lab/docker-compose.yml` puts the proxy (`10.0.0.10`, HTTPS on 8443 and a
redirect on 8080, configured by `lab/proxy.toml`: a round-robin default
pool with the `default.test` certificate, `a.test` on least-connections
and `b.test` on consistent hashing by `X-User-Id`, each with its own
certificate), `backend1`–`backend3` (`10.0.0.11`–`.13`, port 8000) and a
load generator with curl, wrk and k6 (`10.0.0.20`, the lab CA at
`/work/ca.crt`) on one network. `make lab-up` creates the certificates if
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

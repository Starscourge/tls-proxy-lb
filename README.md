# tls-proxy-lb

An HTTP/1.1 reverse proxy and load balancer in Go that terminates TLS, routes
by SNI and balances requests across backend pools.

The request path is built on `net` and `crypto/tls` directly. From `net/http`
it uses only the parsers and serialisers (`ReadRequest`, `ReadResponse`,
`Request.Write`, `Response.Write`, `Header`). `http.Server`, `http.Transport`,
`http.Client` and `httputil.ReverseProxy` are not used on the request path.

**Status:** Phase 1 (plain HTTP proxy to one backend). TLS, pools and the
config file arrive in Phases 2 and 3.

## Layout

```text
cmd/proxy/         the proxy
cmd/testbackend/   test backend for tests and the lab
internal/          proxy packages (conn, httpx, tlsx, lb, upstream, health,
                   ratelimit, config, admin) and the test backend library
test/integration/  end-to-end tests and their helpers
bench/             benchmark and demo scripts, raw results
lab/               Docker Compose lab, certificate script
docs/              design decisions
```

## Requirements

Linux (WSL2 works), Go as pinned in `go.mod`, Docker with Compose v2,
`openssl`, and `staticcheck` for `make lint`.

## Commands

```sh
make build              # go build ./... and binaries in bin/
make test               # go test -race ./...
make lint               # gofmt, go vet, staticcheck
make certs              # local CA and certificates for a.test, b.test in lab/certs
make lab-up             # proxy, three backends and a load generator in Docker
make lab-check          # bring the lab up and check backends, proxy and netem
make lab-down
bench/phase1/wrk-race.sh   # wrk -c 200 -d 30s against a -race proxy build
```

## Running the proxy

```sh
go run ./cmd/testbackend -listen 127.0.0.1 -port 8000 -name b1 &
go run ./cmd/proxy -listen 127.0.0.1:8080 -backend 127.0.0.1:8000
curl -i http://127.0.0.1:8080/
```

Flags: `-listen`, `-backend` (required), `-max-conns` (default 10000),
`-max-header-bytes` (default 64 KiB). Timeouts use their defaults until the
config file arrives: header read 5 s, idle keep-alive 60 s, client body and
response stalls 60 s, upstream connect 1 s, upstream response 10 s. One JSON
access-log line per request goes to stdout.

What Phase 1 does: keep-alive to clients, streaming bodies in both directions
(`Content-Length` and chunked) with backpressure, hop-by-hop header removal,
`X-Forwarded-For` and `X-Forwarded-Proto`, `100 Continue`, and 400, 408, 431,
502 and 504 on the failures they name. Each request opens a new upstream
connection; reuse is Phase 4.

## Test backend

```sh
go run ./cmd/testbackend -port 8000 -name b1 -delay-ms 0 -fail-rate 0
```

Every response carries `X-Backend: <name>`. Endpoints: `/` (catch-all),
`/healthz`, `/echo` (streams the request body back), `/slow?ms=N`
(default 5000) and `/bytes?n=N[&chunked=1]` (N bytes of a fixed pattern). `-delay-ms` and `-fail-rate` apply to every endpoint. Each
request is logged as one JSON line.

## Lab

`lab/docker-compose.yml` puts the proxy (`10.0.0.10:8080`, forwarding to
`backend1` for now), `backend1`–`backend3`
(`10.0.0.11`–`.13`, port 8000) and a load generator with curl, wrk and k6
(`10.0.0.20`) on one network. Backends have `NET_ADMIN`, so traffic shaping
works inside them:

```sh
docker compose -f lab/docker-compose.yml exec backend1 tc qdisc add dev eth0 root netem delay 100ms
docker compose -f lab/docker-compose.yml exec loadgen curl -w '%{time_total}\n' -o /dev/null -s http://10.0.0.11:8000/
docker compose -f lab/docker-compose.yml exec backend1 tc qdisc del dev eth0 root
```

netem delays every packet the backend sends, so a new connection pays the delay
twice (SYN-ACK and response). See `docs/DESIGN.md`.

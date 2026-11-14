# syntax=docker/dockerfile:1
# Images for the lab. Build context is the repo root:
#   docker build -f lab/Dockerfile --target proxy .
# Targets: proxy, testbackend, loadgen, slowhttptest.

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.22
ARG K6_VERSION=1.3.0
ARG DEBIAN_VERSION=bookworm-slim
ARG SLOWHTTPTEST_VERSION=1.8.2-1+b1

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/proxy ./cmd/proxy \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/testbackend ./cmd/testbackend

# The proxy runs as an unprivileged user.
FROM alpine:${ALPINE_VERSION} AS proxy
RUN adduser -D -H -u 10001 proxy
COPY --from=build /out/proxy /usr/local/bin/proxy
USER proxy
ENTRYPOINT ["/usr/local/bin/proxy"]

# Backends run as root so `tc` works with the NET_ADMIN capability granted in
# docker-compose.yml. iproute2 provides tc; curl is for debugging.
FROM alpine:${ALPINE_VERSION} AS testbackend
RUN apk add --no-cache iproute2 curl
COPY --from=build /out/testbackend /usr/local/bin/testbackend
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/testbackend"]

# Slow-client attacks for bench/phase8/slowhttptest.sh: slowloris (slow
# headers), slow body and slow read. Debian packages slowhttptest.
FROM debian:${DEBIAN_VERSION} AS slowhttptest
ARG SLOWHTTPTEST_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends slowhttptest=${SLOWHTTPTEST_VERSION} \
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["slowhttptest"]

FROM grafana/k6:${K6_VERSION} AS k6

# Load generator: curl for latency checks, wrk for throughput, k6 for
# constant-arrival-rate runs. k6 is a static binary.
FROM alpine:${ALPINE_VERSION} AS loadgen
RUN apk add --no-cache bash curl wrk
COPY --from=k6 /usr/bin/k6 /usr/local/bin/k6
WORKDIR /work
CMD ["sleep", "infinity"]

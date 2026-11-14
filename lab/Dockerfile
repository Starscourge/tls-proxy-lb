# syntax=docker/dockerfile:1
# Images for the lab. Build context is the repo root:
#   docker build -f lab/Dockerfile --target proxy .
# Targets: proxy, testbackend, loadgen, slowhttptest.

ARG GO_VERSION=1.27.4
ARG ALPINE_VERSION=3.22
ARG K6_VERSION=1.3.0
ARG DEBIAN_VERSION=bookworm-slim
ARG SLOWHTTPTEST_VERSION=1.8.2-1+b1
ARG WRK2_COMMIT=44a94c17d8e6a0bac8559b53da76848e430cb7a7
ARG WRK_VERSION=4.2.0

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

# wrk and wrk2 for Phase 9, both patched never to resume a TLS session.
# Both keep a closed connection's session (SSL_clear) and offer it on the
# next connect, so a new connection per request would measure resumed
# handshakes; the patch drops the session after SSL_clear, and every new
# connection makes a full handshake. Keep-alive runs never reconnect and
# are unaffected. The packaged wrk stays for the earlier phases' scripts.
#
# wrk2 (constant-rate wrk with coordinated-omission-corrected latency) is
# unmaintained since 2019 and not packaged; its bundled LuaJIT 2.0 writes a
# bytecode object today's linker rejects ("string table is corrupt"), so
# the one Makefile line that makes it has LuaJIT write C instead, which the
# system compiler builds.
FROM alpine:${ALPINE_VERSION} AS wrk2
ARG WRK2_COMMIT
RUN apk add --no-cache build-base openssl-dev zlib-dev git linux-headers
RUN git clone https://github.com/giltene/wrk2.git /src && git -C /src checkout -q ${WRK2_COMMIT}
WORKDIR /src
RUN sed -i 's|^    SSL_clear(c->ssl);$|    SSL_clear(c->ssl);\n    SSL_set_session(c->ssl, NULL);|' src/ssl.c \
 && grep -q 'SSL_set_session(c->ssl, NULL)' src/ssl.c
RUN sed -i 's|cd $(LDIR) \&\& ./luajit -b $(CURDIR)/$< $(CURDIR)/$@|cd $(LDIR) \&\& ./luajit -b -t c -n wrk $(CURDIR)/$< $(CURDIR)/$(ODIR)/bytecode.c \&\& $(CC) -c -o $(CURDIR)/$@ $(CURDIR)/$(ODIR)/bytecode.c|' Makefile \
 && grep -q 'luajit -b -t c' Makefile \
 && make -j"$(nproc)" >/tmp/build.log 2>&1 || { tail -40 /tmp/build.log; exit 1; }

FROM alpine:${ALPINE_VERSION} AS wrk
ARG WRK_VERSION
RUN apk add --no-cache build-base openssl-dev zlib-dev git linux-headers luajit-dev luajit
RUN git clone --depth 1 --branch ${WRK_VERSION} https://github.com/wg/wrk.git /src
WORKDIR /src
RUN sed -i 's|^    SSL_clear(c->ssl);$|    SSL_clear(c->ssl);\n    SSL_set_session(c->ssl, NULL);|' src/ssl.c \
 && grep -q 'SSL_set_session(c->ssl, NULL)' src/ssl.c \
 && C_INCLUDE_PATH=/usr/include/luajit-2.1 make -j"$(nproc)" WITH_OPENSSL=/usr WITH_LUAJIT=/usr >/tmp/build.log 2>&1 || { tail -40 /tmp/build.log; exit 1; }

# Load generator: curl for latency checks, wrk for throughput (wrk-noresume
# in Phase 9), wrk2 for constant-rate latency, k6 for constant-arrival-rate
# runs, and openssl s_client to check what a handshake negotiates. k6 is a
# static binary.
FROM alpine:${ALPINE_VERSION} AS loadgen
RUN apk add --no-cache bash curl wrk openssl luajit
COPY --from=k6 /usr/bin/k6 /usr/local/bin/k6
COPY --from=wrk2 /src/wrk /usr/local/bin/wrk2
COPY --from=wrk /src/wrk /usr/local/bin/wrk-noresume
WORKDIR /work
CMD ["sleep", "infinity"]

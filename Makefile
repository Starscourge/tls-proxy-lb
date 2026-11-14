# Common tasks. See README.md for the lab.
GO        ?= go
BIN       := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE   := docker compose -f lab/docker-compose.yml
UNIT_PKGS  = $(shell $(GO) list ./... | grep -v /test/integration)

.PHONY: all build test test-unit test-integration lint vuln fmt certs \
        check-config lab-up lab-down lab-check netem-check prom-check wrk-race bench-lb testssl \
        bench-allocs bench-reuse wrk-race-reuse bench-scaling bench-profile \
        bench-kill bench-stop bench-ratelimit-k6 bench-ratelimit-race \
        bench-reload bench-reload-control stop-check fuzz bench-slowloris \
        demo-kill demo-slow clean

all: build test lint

build:
	$(GO) build ./...
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN)/proxy ./cmd/proxy
	$(GO) build -trimpath -o $(BIN)/testbackend ./cmd/testbackend

# Everything, race detector on.
test:
	$(GO) test -race ./...

test-unit:
	$(GO) test -race $(UNIT_PKGS)

test-integration:
	$(GO) test -race ./test/integration/...

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	staticcheck ./...

vuln:
	govulncheck ./...

fmt:
	gofmt -w .

certs:
	lab/gen-certs.sh lab/certs

# Validate a config file like nginx -t (default: the lab's).
CONFIG ?= lab/proxy.toml
check-config:
	$(GO) run ./cmd/proxy -config $(CONFIG) -check-config

# The proxy container runs as you, so it can read the 0600 keys in lab/certs.
LAB_ENV := LAB_UID=$$(id -u) LAB_GID=$$(id -g)

lab-up:
	test -s lab/certs/default.test.crt || lab/gen-certs.sh lab/certs
	$(LAB_ENV) $(COMPOSE) up -d --build --wait

lab-down:
	$(LAB_ENV) $(COMPOSE) down

# Phase 0 "Done when": lab comes up, backends answer, netem adds ~100 ms.
lab-check:
	lab/check-lab.sh

netem-check:
	lab/netem-check.sh

# Phase 5 "Done when": Prometheus in the lab scrapes the proxy's /metrics.
prom-check:
	lab/prometheus-check.sh

# Phase 1 "Done when": wrk -c 200 -d 30s against a -race build.
wrk-race:
	bench/phase1/wrk-race.sh

# Phase 2 "Done when": balancer picks allocate nothing.
bench-lb:
	bench/phase2/balancers.sh

# Phase 3 "Done when": testssl.sh against the lab proxy, report in docs/.
REPORT ?= docs/testssl-phase8.txt
testssl:
	bench/phase3/testssl.sh $(REPORT)

# Phase 4 "Done when" items; results in bench/phase4/results/.
bench-allocs:            # allocations per request (LABEL=after by default)
	bench/phase4/allocs.sh $(or $(LABEL),after)
bench-reuse:             # wrk with reuse off and on, new upstream connections per second
	bench/phase4/reuse.sh
wrk-race-reuse:          # wrk GET and POST /echo against a -race build, reuse on
	bench/phase4/wrk-race.sh
bench-scaling:           # throughput at GOMAXPROCS 1, 2 and 4
	bench/phase4/scaling.sh
bench-profile:           # mutex, block and CPU profiles under load
	bench/phase4/profile.sh

# Phase 5 "Done when" items; results in bench/phase5/results/.
bench-kill:              # kill -9 one of three backends under wrk, then restart it
	bench/phase5/failover.sh kill
bench-stop:              # SIGSTOP one of three backends under wrk, then SIGCONT
	bench/phase5/failover.sh stop

# Phase 6 "Done when" items; results in bench/phase6/results/.
bench-ratelimit-k6:      # k6 at twice the limit plus a second client, in the lab (lab must be up)
	bench/phase6/k6.sh
bench-ratelimit-race:    # -race run and mutex profile with the rate-limit table under load
	bench/phase6/race-profile.sh

# Phase 7 "Done when" items; results in bench/phase7/results/.
bench-reload:            # SIGHUP every second, alternating pool membership, during 60 s of wrk
	bench/phase7/reload-wrk.sh
bench-reload-control:    # the same wrk run with no SIGHUP
	RELOADS=off bench/phase7/reload-wrk.sh
stop-check:              # docker stop drains and exits 0 inside the grace period (lab must be up)
	lab/stop-check.sh --no-up

# Phase 8 "Done when" items; results in bench/phase8/results/.
fuzz:                    # each fuzz target for at least a CPU-hour; corpora into testdata/fuzz
	bench/phase8/fuzz.sh
bench-slowloris:         # slowhttptest: slow headers, body and read at 5,000 connections, in the lab
	bench/phase8/slowhttptest.sh

# Phase 9 experiments; results and graphs in bench/phase9/results/.
demo-kill:               # docker kill one of three backends under k6 at 1,000 req/s, then start it
	bench/phase9/demo-kill.sh
demo-slow:               # netem delay, then loss, on one backend under three proxy configs
	bench/phase9/demo-slow.sh

clean:
	rm -rf $(BIN)

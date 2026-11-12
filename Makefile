# Common tasks. See README.md for the lab.
GO        ?= go
BIN       := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE   := docker compose -f lab/docker-compose.yml
UNIT_PKGS  = $(shell $(GO) list ./... | grep -v /test/integration)

.PHONY: all build test test-unit test-integration lint vuln fmt certs \
        check-config lab-up lab-down lab-check netem-check wrk-race bench-lb testssl clean

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

# Phase 1 "Done when": wrk -c 200 -d 30s against a -race build.
wrk-race:
	bench/phase1/wrk-race.sh

# Phase 2 "Done when": balancer picks allocate nothing.
bench-lb:
	bench/phase2/balancers.sh

# Phase 3 "Done when": testssl.sh against the lab proxy, report in docs/.
testssl:
	bench/phase3/testssl.sh

clean:
	rm -rf $(BIN)

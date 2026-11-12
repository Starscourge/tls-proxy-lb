#!/usr/bin/env bash
# Phase 2 "Done when": balancer benchmarks show zero allocations per pick.
# Also writes the consistent-hash spread table and the least-connections
# split from the tests that measure them.
#
# Usage: bench/phase2/balancers.sh
# Writes bench/phase2/results/balancers.txt and exits non-zero if any pick
# allocates or a test fails.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="$root/bench/phase2/results/balancers.txt"
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase2/results' || echo '-dirty')"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cd "$root"
go test -run '^$' -bench . -benchmem -count 1 ./internal/lb | tee "$tmp/bench.txt"
go test -run 'TestRingSpread$' -v -count 1 ./internal/lb | grep 'ring_test.go' | sed 's/^ *ring_test.go:[0-9]*: //' | tee "$tmp/spread.txt"
go test -race -run 'TestLeastConnAvoidsSlowBackend$' -v -count 1 ./test/integration | grep 'phase2_test.go' | sed 's/^ *phase2_test.go:[0-9]*: //' | tee "$tmp/split.txt"

allocs=$(awk '/^Benchmark/ && $(NF-1) != 0 {print $1}' "$tmp/bench.txt")
{
  echo "# Phase 2: balancer benchmarks"
  echo "date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:  $rev"
  echo "go:      $(go version | awk '{print $3, $4}')"
  echo "kernel:  $(uname -sr)"
  echo "cpus:    $(nproc)"
  echo
  echo "## go test -bench . -benchmem ./internal/lb (10 backends)"
  grep -E '^(Benchmark|goos|goarch|cpu)' "$tmp/bench.txt"
  echo
  echo "## Consistent-hash spread: 100,000 client IPs, 20 address sets per row"
  cat "$tmp/spread.txt"
  echo
  echo "## Least-connections vs round-robin: 3 backends, one +200 ms, 50 clients, -race"
  cat "$tmp/split.txt"
  echo
  if [[ -z "$allocs" ]]; then echo "RESULT: PASS (0 allocs/op everywhere)"; else echo "RESULT: FAIL: allocating: $allocs"; fi
} > "$out"
echo; tail -1 "$out"
[[ -z "$allocs" ]]

#!/usr/bin/env bash
# Phase 4 "Done when": allocations per request, measured before and after
# pooling. Runs BenchmarkProxy (internal/conn/bench_test.go), whose client
# and backend allocate nothing per request, so allocs/op is the proxy's.
#
# Usage: bench/phase4/allocs.sh LABEL [COUNT]
# Writes bench/phase4/results/allocs-LABEL.txt.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
label="${1:?usage: allocs.sh LABEL [COUNT]}"
count="${2:-5}"
out="$root/bench/phase4/results/allocs-$label.txt"
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase4/results' || echo '-dirty')"

{
  echo "# Phase 4: allocations per request ($label)"
  echo "date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:  $rev"
  echo "go:      $(go version | awk '{print $3, $4}')"
  echo "command: go test -run '^\$' -bench BenchmarkProxy -benchmem -count $count ./internal/conn"
  echo
  (cd "$root" && go test -run '^$' -bench BenchmarkProxy -benchmem -count "$count" ./internal/conn)
} | tee "$out"

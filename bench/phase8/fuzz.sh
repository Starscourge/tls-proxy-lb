#!/usr/bin/env bash
# Phase 8 "Done when": each fuzz target has run for at least one CPU-hour
# with no open crashes, and its corpus is committed.
#
# Runs the three targets at once, each with PARALLEL workers for FUZZTIME,
# and measures the CPU time each used (user plus system, workers
# included) with /usr/bin/time. Then it copies each target's corpus, the
# inputs the fuzzer kept because they reached new coverage, from the Go
# cache into the package's testdata/fuzz/<Target>/, where `go test` runs
# every one of them as a regression test. Inputs that made a target fail
# are written there by `go test` itself.
#
# Passes when every target used at least 3,600 CPU-seconds and none
# failed.
#
# Usage: [FUZZTIME=12m] [PARALLEL=7] bench/phase8/fuzz.sh
# Writes bench/phase8/results/fuzz.txt.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="$root/bench/phase8/results/fuzz.txt"
fuzztime="${FUZZTIME:-12m}"
parallel="${PARALLEL:-7}"
min_cpu=3600
module="$(cd "$root" && go list -m)"
cache="$(go env GOCACHE)/fuzz"
targets=("FuzzRequestPath internal/httpx" "FuzzResponse internal/httpx" "FuzzConfig internal/config")
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase8/results' ':!*/testdata/fuzz' || echo '-dirty')"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "== fuzzing ${#targets[@]} targets at once, $parallel workers each, for $fuzztime"
for t in "${targets[@]}"; do
  read -r name pkg <<<"$t"
  (
    cd "$root"
    rc=0
    /usr/bin/time -f "%U %S" -o "$tmp/$name.time" \
      go test -run '^$' -fuzz="^$name\$" -fuzztime="$fuzztime" -parallel "$parallel" "./$pkg" \
      >"$tmp/$name.log" 2>&1 || rc=$?
    echo "$rc" >"$tmp/$name.rc"
  ) &
done
wait

fail=()
{
  echo "# Phase 8: fuzzing, $fuzztime per target, $parallel workers each, all three at once"
  echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:   $rev"
  echo "go:       $(go version | awk '{print $3, $4}')"
  echo "cpu:      $(lscpu | sed -n 's/^Model name: *//p'), $(nproc) logical CPUs"
  echo
  printf '%-16s %10s %14s %10s %8s %10s  %s\n' target cpu_s execs exec/s corpus crashers result
  for t in "${targets[@]}"; do
    read -r name pkg <<<"$t"
    # The last line: time prints "Command exited with non-zero status"
    # before it when the command failed.
    read -r user sys < <(tail -1 "$tmp/$name.time")
    cpu=$(awk -v u="$user" -v s="$sys" 'BEGIN { printf "%.0f", u + s }')
    rc=$(cat "$tmp/$name.rc")
    last=$(grep '^fuzz: elapsed' "$tmp/$name.log" | tail -1)
    execs=$(echo "$last" | sed -n 's/.*execs: \([0-9]*\).*/\1/p')
    secs=$(echo "$last" | sed -n 's/.*elapsed: \([0-9hms]*\),.*/\1/p')
    rate=$(awk -v e="${execs:-0}" -v t="$secs" 'BEGIN {
      s = 0; n = t
      if (match(n, /[0-9]+h/)) { s += substr(n, RSTART, RLENGTH - 1) * 3600 }
      if (match(n, /[0-9]+m/)) { s += substr(n, RSTART, RLENGTH - 1) * 60 }
      if (match(n, /[0-9]+s/)) { s += substr(n, RSTART, RLENGTH - 1) }
      printf "%.0f", (s > 0 ? e / s : 0) }')
    dst="$root/$pkg/testdata/fuzz/$name"
    mkdir -p "$dst"
    src="$cache/$module/$pkg/$name"
    [[ -d $src ]] && cp -n "$src"/* "$dst"/ 2>/dev/null || true
    corpus=$(find "$dst" -type f | wc -l)
    # A failing input is reported as "Failing input written to ...".
    crashers=$(grep -c 'Failing input written to' "$tmp/$name.log" || true)
    result=PASS
    if [[ $rc != 0 ]]; then result=FAIL; fail+=("$name exited $rc"); fi
    if ((cpu < min_cpu)); then result=FAIL; fail+=("$name used $cpu CPU-seconds, under $min_cpu"); fi
    printf '%-16s %10s %14s %10s %8s %10s  %s\n' "$name" "$cpu" "${execs:-?}" "$rate" "$corpus" "$crashers" "$result"
  done
  echo
  echo "cpu_s: user plus system CPU-seconds, workers included; execs and exec/s from the last"
  echo "progress line; corpus: files now in testdata/fuzz/<target> (seeds in the code are extra)."
  for t in "${targets[@]}"; do
    read -r name _ <<<"$t"
    echo
    echo "## $name: last lines"
    tail -4 "$tmp/$name.log"
  done
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} >"$out"

cat "$out"
[[ ${#fail[@]} -eq 0 ]]

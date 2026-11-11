#!/usr/bin/env bash
# Phase 1 "Done when": `wrk -c 200 -d 30s` against a -race build of the
# proxy gives no race reports and no non-2xx responses, and the proxy's open
# fds return to their baseline afterwards.
#
# Usage: bench/phase1/wrk-race.sh [DURATION] [CONNECTIONS]
# Env:   PROXY_PORT (18080), BACKEND_PORT (18000), WRK_THREADS (4)
# Writes bench/phase1/results/wrk-race.txt and exits non-zero on failure.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
duration="${1:-30s}"
conns="${2:-200}"
threads="${WRK_THREADS:-4}"
pport="${PROXY_PORT:-18080}"
bport="${BACKEND_PORT:-18000}"
out="$root/bench/phase1/results/wrk-race.txt"

# Recorded before anything is written, so the results file itself does not
# make the tree look dirty.
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase1/results' || echo '-dirty')"

command -v wrk >/dev/null || { echo "wrk not installed" >&2; exit 1; }
for port in "$pport" "$bport"; do
  if ss -Htln "sport = :$port" | grep -q .; then
    echo "port $port is in use" >&2; exit 1
  fi
done

tmp="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

echo "== building (proxy with -race)"
(cd "$root" && go build -race -o "$tmp/proxy" ./cmd/proxy && go build -o "$tmp/testbackend" ./cmd/testbackend)

"$tmp/testbackend" -listen 127.0.0.1 -port "$bport" -name b1 >"$tmp/backend.log" 2>&1 &
pids+=($!)
GORACE="log_path=$tmp/race exitcode=66" \
  "$tmp/proxy" -listen "127.0.0.1:$pport" -backend "127.0.0.1:$bport" >"$tmp/access.log" 2>"$tmp/proxy.err" &
ppid=$!
pids+=($ppid)

for _ in $(seq 50); do
  curl -fs -o /dev/null "http://127.0.0.1:$pport/healthz" && break
  sleep 0.1
done
curl -fs -o /dev/null "http://127.0.0.1:$pport/healthz" || { echo "proxy did not come up" >&2; cat "$tmp/proxy.err" >&2; exit 1; }

fds() { find "/proc/$ppid/fd" -mindepth 1 -maxdepth 1 | wc -l; }
threads_of() { awk '/^Threads:/ {print $2}' "/proc/$ppid/status"; }
rss_kb() { awk '/^VmRSS:/ {print $2}' "/proc/$ppid/status"; }
fd_base=$(fds)
rss_base=$(rss_kb)

echo "== wrk -t$threads -c$conns -d$duration"
wrk -t"$threads" -c"$conns" -d"$duration" --latency "http://127.0.0.1:$pport/" | tee "$tmp/wrk.txt"
rss_peak_note=$(rss_kb)

# wrk closes its connections when it exits; give the proxy a moment.
fd_after=$(fds)
for _ in $(seq 50); do
  fd_after=$(fds)
  [[ $fd_after -le $fd_base ]] && break
  sleep 0.1
done
threads_after=$(threads_of)

kill -TERM "$ppid"
set +e
wait "$ppid"
proxy_exit=$?
set -e
pids=("${pids[@]:0:1}")

fail=()
grep -q "Non-2xx or 3xx responses" "$tmp/wrk.txt" && fail+=("non-2xx responses")
grep -q "Socket errors" "$tmp/wrk.txt" && fail+=("socket errors")
races=$(cat "$tmp"/race.* 2>/dev/null | grep -c "WARNING: DATA RACE" || true)
races=$((races + $(grep -c "WARNING: DATA RACE" "$tmp/proxy.err" || true)))
[[ $races -eq 0 ]] || fail+=("$races data race reports")
[[ $proxy_exit -eq 0 ]] || fail+=("proxy exit code $proxy_exit")
[[ $fd_after -le $fd_base ]] || fail+=("fds $fd_after after load, baseline $fd_base")
access_lines=$(grep -c '"msg":"access"' "$tmp/access.log" || true)
non200=$(grep '"msg":"access"' "$tmp/access.log" | grep -vc '"status":200' || true)
[[ $non200 -eq 0 ]] || fail+=("$non200 non-200 access log lines")

{
  echo "# Phase 1: wrk against a -race build"
  echo "date:       $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:     $rev"
  echo "go:         $(go version | awk '{print $3, $4}')"
  echo "kernel:     $(uname -sr)"
  echo "cpus:       $(nproc)"
  echo "command:    wrk -t$threads -c$conns -d$duration --latency http://127.0.0.1:$pport/"
  echo "backend:    testbackend on 127.0.0.1:$bport, GET / (14-byte body)"
  echo
  cat "$tmp/wrk.txt"
  echo
  echo "access log lines:     $access_lines (non-200: $non200)"
  echo "data race reports:    $races"
  echo "proxy exit code:      $proxy_exit (after SIGTERM)"
  echo "proxy fds:            baseline $fd_base, after load $fd_after"
  echo "proxy threads after:  $threads_after"
  echo "proxy RSS:            baseline ${rss_base} kB, at end of load ${rss_peak_note} kB (race detector shadow memory included)"
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} > "$out"

echo
tail -9 "$out"
[[ ${#fail[@]} -eq 0 ]]

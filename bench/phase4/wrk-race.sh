#!/usr/bin/env bash
# Phase 4 "Done when": a wrk run against a -race build is clean, with
# upstream connections reused.
#
# Two wrk runs at once against a -race build of the proxy in front of three
# backends: GET / and POST /echo with a 4 KiB body (post-echo.lua), which
# exercises the request-writer goroutine on reused connections. Passes with
# no race reports, no non-2xx responses or socket errors, a clean exit, and
# the proxy's fds back to their baseline once the idle upstream connections
# have been reaped (idle_timeout_ms = 1000 here).
#
# Usage: bench/phase4/wrk-race.sh [DURATION] [GET_CONNS] [POST_CONNS]
# Writes bench/phase4/results/wrk-race.txt and exits non-zero on failure.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

duration="${1:-30s}"
gconns="${2:-100}"
pconns="${3:-50}"
out="$root/bench/phase4/results/wrk-race.txt"
procs=4

build -race
start_backends 3
write_config "$tmp/proxy.toml" 128
# Short idle timeout so the pool empties soon after the load stops.
sed -i 's/^max_idle_per_backend = 128$/max_idle_per_backend = 128\nidle_timeout_ms = 1000/' "$tmp/proxy.toml"
start_proxy "$tmp/proxy.toml" "$procs" GORACE="log_path=$tmp/race exitcode=66"

fds() { find "/proc/$proxy_pid/fd" -mindepth 1 -maxdepth 1 | wc -l; }
fd_base=$(fds)
a0=$(accepted)

echo "== wrk GET -c$gconns and POST /echo -c$pconns for $duration against a -race build"
wrk_run -t3 -c"$gconns" -d"$duration" --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/get.wrk" &
g=$!
wrk_run -t2 -c"$pconns" -d"$duration" --latency -s "$root/bench/phase4/post-echo.lua" "http://127.0.0.1:$PROXY_PORT/" >"$tmp/post.wrk" &
p=$!
wait "$g" "$p"
a1=$(accepted)
fd_load=$(fds)

# wrk has closed its connections; the reaper closes the idle upstream ones
# within about 1.5 idle timeouts.
fd_after=$(fds)
for _ in $(seq 100); do
  fd_after=$(fds)
  [[ $fd_after -le $fd_base ]] && break
  sleep 0.1
done
stop_proxy

fail=()
for f in get post; do
  grep -q "Non-2xx or 3xx responses" "$tmp/$f.wrk" && fail+=("$f: non-2xx responses")
  grep -q "Socket errors" "$tmp/$f.wrk" && fail+=("$f: socket errors")
done
races=$(cat "$tmp"/race.* 2>/dev/null | grep -c "WARNING: DATA RACE" || true)
races=$((races + $(grep -c "WARNING: DATA RACE" "$tmp/proxy.err" || true)))
[[ $races -eq 0 ]] || fail+=("$races data race reports")
[[ $proxy_exit -eq 0 ]] || fail+=("proxy exit code $proxy_exit")
[[ $fd_after -le $fd_base ]] || fail+=("fds $fd_after after load, baseline $fd_base")
access=$(grep -c '"msg":"access"' "$tmp/access.log" || true)
non200=$(grep '"msg":"access"' "$tmp/access.log" | grep -vc '"status":200' || true)
fresh=$(grep '"msg":"access"' "$tmp/access.log" | grep -c '"reused":false' || true)
retried=$(grep -c '"retried":true' "$tmp/access.log" || true)
[[ $non200 -eq 0 ]] || fail+=("$non200 non-200 access log lines")

{
  echo "# Phase 4: wrk against a -race build, upstream reuse on"
  header
  echo "proxy:    -race build, GOMAXPROCS $procs, 3 test backends, round-robin,"
  echo "          max_idle_per_backend 128, idle_timeout_ms 1000"
  echo
  echo "## GET /"
  echo "command: wrk -t3 -c$gconns -d$duration --latency http://127.0.0.1:$PROXY_PORT/"
  cat "$tmp/get.wrk"
  echo
  echo "## POST /echo, 4 KiB body"
  echo "command: wrk -t2 -c$pconns -d$duration --latency -s bench/phase4/post-echo.lua http://127.0.0.1:$PROXY_PORT/"
  cat "$tmp/post.wrk"
  echo
  echo "access log lines:          $access (non-200: $non200)"
  echo "requests on new upstream connections: $fresh (retried: $retried)"
  echo "upstream connections accepted by the backends during the run: $((a1 - a0))"
  echo "data race reports:         $races"
  echo "proxy exit code:           $proxy_exit (after SIGTERM)"
  echo "proxy fds:                 baseline $fd_base, under load $fd_load, after reaping $fd_after"
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} >"$out"

echo
tail -8 "$out"
[[ ${#fail[@]} -eq 0 ]]

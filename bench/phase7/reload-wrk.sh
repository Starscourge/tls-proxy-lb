#!/usr/bin/env bash
# Phase 7 "Done when": a SIGHUP every second, alternating pool membership,
# during a 60-second wrk run gives zero non-2xx responses and zero socket
# errors.
#
# A -race build of the proxy serves plain HTTP from a round-robin pool over
# three test backends, with active health checks on. Every second the
# script renames a new config into place (b1+b2, then b2+b3, and so on) and
# sends SIGHUP, so each reload removes one backend and adds another: its
# prober stops or starts, its idle connections close, and requests in
# flight to it finish on the old config. It passes with no non-2xx
# response or socket error in wrk's report, no failed request in the
# access log, every reload applied and none failed, no data race, and
# exit code 0 after SIGTERM.
#
# wrk times requests with the wall clock, which WSL2 steps by seconds every
# half minute or so; a backward step makes the request in flight on each
# connection look timed out. As in bench/phase6/race-profile.sh, the script
# measures the wall clock against the monotonic one every second, counts
# the steps, tolerates at most one wrk "timeout" per connection per step,
# and the proxy's own access log (monotonic timings) decides either way.
#
# RELOADS=off runs the same load with no SIGHUP, as a control: the wrk
# timeouts it shows come from the clock alone.
#
# Usage: [RELOADS=off] bench/phase7/reload-wrk.sh [DURATION_S]
# Writes bench/phase7/results/reload-wrk.txt (reload-wrk-control.txt with
# RELOADS=off) and exits non-zero on failure.
set -euo pipefail
source "$(dirname "$0")/../phase4/lib.sh"

duration="${1:-60}"
reloads="${RELOADS:-on}"
case "$reloads" in on) name=reload-wrk ;; off) name=reload-wrk-control ;; *) echo "RELOADS must be on or off" >&2; exit 2 ;; esac
out="$root/bench/phase7/results/$name.txt"
procs=4
conns=32

build -race
start_backends 3

# write_set FILE N: the config whose pool is backends N+1 and N+2 (b1+b2 for
# N = 0, b2+b3 for N = 1), written beside FILE and renamed over it, so a
# reload never reads half a file.
write_set() {
  local file=$1 n=$2
  cat >"$file.new" <<TOML
default_pool = "default"

[[listener]]
address = "127.0.0.1:$PROXY_PORT"
mode    = "http"

[pool.default]
algorithm = "round_robin"
backends  = [ { address = "${backend_addrs[$n]}" }, { address = "${backend_addrs[$((n + 1))]}" } ]

[upstream]
max_idle_per_backend = 128

[health]
interval_ms = 500
timeout_ms  = 250

[admin]
address = "127.0.0.1:$ADMIN_PORT"
TOML
  mv "$file.new" "$file"
}

write_set "$tmp/proxy.toml" 0
start_proxy "$tmp/proxy.toml" "$procs" GORACE="log_path=$tmp/race exitcode=66"

# clock_offset: wall clock minus monotonic clock, in seconds.
clock_offset() { python3 -I -c 'import time; print(time.time() - time.monotonic())'; }

echo "== wrk -c$conns for ${duration}s; reloads $reloads"
offset0=$(clock_offset)
prev=$offset0
steps=0
# count_step: one more step if the offset moved by over 0.1 s since the
# last look.
count_step() {
  local now
  now=$(clock_offset)
  if awk -v a="$prev" -v b="$now" 'BEGIN { exit !((b - a) > 0.1 || (b - a) < -0.1) }'; then
    steps=$((steps + 1))
  fi
  prev=$now
}
wrk_run -t2 -c"$conns" -d"${duration}s" --timeout 5s --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/wrk.txt" &
w=$!
sent=0
# One reload a second until a second before wrk ends, so the last one has
# settled when the run stops.
for ((i = 1; i < duration; i++)); do
  sleep 1
  if [[ $reloads == on ]]; then
    write_set "$tmp/proxy.toml" $((i % 2))
    kill -HUP "$proxy_pid"
    sent=$((sent + 1))
  fi
  count_step
done
wait "$w"
count_step
step=$(awk -v a="$offset0" -v b="$prev" 'BEGIN { printf "%.3f", b - a }')
metrics="$(curl -fs "http://127.0.0.1:$ADMIN_PORT/metrics")"
stop_proxy

metric() { echo "$metrics" | sed -n "s/^$1 //p"; }
applied=$(metric 'proxy_config_reloads_total{result="success"}')
failed=$(metric 'proxy_config_reloads_total{result="failure"}')
non2xx=$(sed -n 's/^ *Non-2xx or 3xx responses: *//p' "$tmp/wrk.txt")
socket_errors=$(sed -n 's/^ *Socket errors: *//p' "$tmp/wrk.txt")
access=$(grep -c '"msg":"access"' "$tmp/access.log" || true)
bad=$(grep '"msg":"access"' "$tmp/access.log" | grep -vc '"status":200' || true)
races=$(cat "$tmp"/race.* 2>/dev/null | grep -c "WARNING: DATA RACE" || true)
races=$((races + $(grep -c "WARNING: DATA RACE" "$tmp/proxy.err" || true)))
reload_ms=$(grep '"msg":"config reloaded"' "$tmp/access.log" | sed -n 's/.*"reload_ms":\([0-9.]*\).*/\1/p' | sort -n || true)
by_backend=$(grep '"msg":"access"' "$tmp/access.log" | sed -n 's/.*"backend":"\([^"]*\)".*/\1/p' | sort | uniq -c)

fail=()
[[ -z $non2xx ]] || fail+=("wrk: $non2xx non-2xx responses")
read -r e_connect e_read e_write e_timeout < <(echo "${socket_errors:-connect 0, read 0, write 0, timeout 0}" |
  sed -n 's/connect \([0-9]*\), read \([0-9]*\), write \([0-9]*\), timeout \([0-9]*\)/\1 \2 \3 \4/p')
((e_connect + e_read + e_write == 0)) || fail+=("wrk: socket errors: $socket_errors")
if ((e_timeout > steps * conns)); then
  fail+=("wrk: $e_timeout timeouts")
fi
[[ $bad -eq 0 ]] || fail+=("$bad failed requests in the access log")
[[ ${applied:-0} == "$sent" ]] || fail+=("$applied of $sent reloads applied")
[[ $failed == 0 ]] || fail+=("$failed reloads failed")
[[ $races -eq 0 ]] || fail+=("$races data race reports")
[[ $proxy_exit -eq 0 ]] || fail+=("proxy exit code $proxy_exit")
if [[ $reloads == on ]]; then
  [[ $(echo "$by_backend" | wc -l) -eq 3 ]] || fail+=("not every backend served requests")
fi

{
  if [[ $reloads == on ]]; then
    echo "# Phase 7: SIGHUP every second, alternating pool membership, under wrk"
  else
    echo "# Phase 7 control: the same wrk run with no SIGHUP (b1+b2 throughout)"
  fi
  header
  echo "proxy:    -race build, GOMAXPROCS $procs, round-robin over 2 of 3 test backends,"
  echo "          max_idle_per_backend 128; health: interval 500 ms, timeout 250 ms"
  echo "command:  wrk -t2 -c$conns -d${duration}s --timeout 5s --latency http://127.0.0.1:$PROXY_PORT/"
  echo "reloads:  config renamed into place, then SIGHUP, every second: b1+b2, b2+b3, ..."
  echo
  cat "$tmp/wrk.txt"
  echo
  echo "## Reloads"
  echo "SIGHUPs sent:                  $sent"
  echo "applied (metrics):             $applied"
  echo "failed (metrics):              $failed"
  echo "reload time ms (min/median/max): $(echo "$reload_ms" | head -1) / $(echo "$reload_ms" | awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}') / $(echo "$reload_ms" | tail -1)"
  echo
  echo "## Requests"
  echo "non-2xx responses (wrk):       ${non2xx:-0}"
  echo "socket errors (wrk):           ${socket_errors:-none}"
  printf "wall clock against monotonic:  %+.3f s over the run, in %d steps\n" "$step" "$steps"
  if ((e_timeout > 0)); then
    echo "                               wrk timeouts are clock-step artefacts: at most one per connection"
    echo "                               per step ($((steps * conns)) allowed), and the access log, timed"
    echo "                               monotonically, has no failure"
  fi
  echo "access log lines:              $access"
  echo "failed in the access log:      $bad"
  echo "by backend:"
  echo "$by_backend"
  echo
  echo "data race reports:             $races"
  echo "proxy exit code:               $proxy_exit (after SIGTERM)"
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} >"$out"

echo
sed -n '/## Reloads/,$p' "$out"
[[ ${#fail[@]} -eq 0 ]]

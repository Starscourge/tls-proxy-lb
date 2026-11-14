#!/usr/bin/env bash
# Phase 9 experiment B: kill a backend under load.
#
# In the lab, with the lab's own proxy.toml (three backends, round-robin
# default pool, active checks every second, passive ejection after three
# failures in a row), k6 sends GETs at a constant RATE for DURATION
# seconds. A third of the way in, `docker kill` takes backend2 down (its
# network namespace goes with it, so nothing resets the proxy's
# connections); two thirds of the way in, `docker start` brings it back on
# the same address.
#
# The kill and start times come from this script, the ejection and the
# re-admission from the proxy's own health log, and everything else from
# k6's per-request lines. bench/phase9/plot turns them into kill.svg
# (successes and errors per second, each backend's share, latency) and
# summary.txt: failed requests during detection and after ejection, time to
# eject and to re-admit. It fails the run if a request failed after the
# ejection, or if k6 could not keep the rate.
#
# Usage: bench/phase9/demo-kill.sh [RATE] [DURATION_S]
# Writes bench/phase9/results/kill/.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

rate="${1:-1000}"
duration="${2:-60}"
victim=backend2
victim_addr=10.0.0.12:8000
out="$root/bench/phase9/results/kill"
mkdir -p "$out"

echo "== lab up (proxy recreated with lab/proxy.toml)"
lab_up
clock_start
since="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"

echo "== k6 at $rate/s for ${duration}s; $victim killed at $((duration / 3))s, started at $((duration * 2 / 3))s"
k6_run "$rate" "$duration" kill &
k6pid=$!
# k6 takes a moment to start; time the fault from its first request.
t0=$(first_request_ms kill)
sleep_until $((t0 + duration * 1000 / 3))
t_kill=$(now_ms)
"${compose[@]}" kill -s KILL "$victim" >/dev/null 2>&1
echo "killed $victim"
sleep_until $((t0 + duration * 2000 / 3))
t_start=$(now_ms)
"${compose[@]}" start "$victim" >/dev/null 2>&1
echo "started $victim"
wait "$k6pid"
clock_end

proxy_health_log "$since" >"$out/proxy-health.log"
{
  echo "# events (epoch ms): first request, docker kill, docker start"
  echo "$t0 load"
  echo "$t_kill kill"
  echo "$t_start start"
} >"$out/events.txt"
gzip -9n <"$tmp/kill.csv" >"$out/requests.csv.gz"
cp "$tmp/kill.summary" "$out/k6-summary.txt"

{
  echo "# bench/phase9/demo-kill.sh $rate $duration at $rev, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# lab/proxy.toml; k6 $("${compose[@]}" exec -T loadgen k6 version 2>/dev/null | head -1)"
  plot kill -requests "$out/requests.csv.gz" -events "$out/events.txt" \
    -health "$out/proxy-health.log" -backend "$victim_addr" -name "$victim" \
    -svg "$out/kill.svg"
} | tee "$out/summary.txt"
exit "${PIPESTATUS[0]}"

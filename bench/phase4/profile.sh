#!/usr/bin/env bash
# Phase 4 "Done when": a mutex profile under load is saved and shows no
# single lock dominating.
#
# The proxy runs on 4 cores with mutex and block profiling on, under wrk
# -c 100 against three backends with reuse on. Midway through, 10-second
# delta mutex and block profiles and a CPU profile are fetched from the
# admin listener. The raw profiles and their top entries go to results/.
#
# Usage: bench/phase4/profile.sh [SECONDS]
# Writes bench/phase4/results/{mutex,block,cpu}.pprof and profile.txt.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

secs="${1:-25}"
window=10
procs=4
res="$root/bench/phase4/results"
out="$res/profile.txt"

build
start_backends 3
write_config "$tmp/proxy.toml" 128 "mutex_profile_fraction = 10
block_profile_rate_ns  = 10000"
start_proxy "$tmp/proxy.toml" "$procs"

echo "== wrk -t6 -c100 -d${secs}s with profiles over ${window}s from t=5s"
wrk_run -t6 -c100 -d"${secs}s" --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/wrk.txt" &
w=$!
sleep 5
admin="http://127.0.0.1:$ADMIN_PORT/debug/pprof"
curl -fs -o "$res/mutex.pprof" "$admin/mutex?seconds=$window" &
c1=$!
curl -fs -o "$res/block.pprof" "$admin/block?seconds=$window" &
c2=$!
curl -fs -o "$res/cpu.pprof" "$admin/profile?seconds=$window" &
c3=$!
wait "$c1" "$c2" "$c3" "$w"
stop_proxy

top() { go tool pprof -top -nodecount="$2" "$1" 2>/dev/null | sed -n '/^Showing\|flat/,$p'; }
{
  echo "# Phase 4: mutex, block and CPU profiles under load"
  header
  echo "proxy:    GOMAXPROCS $procs, 3 test backends, round-robin, upstream reuse on,"
  echo "          mutex_profile_fraction 10, block_profile_rate_ns 10000"
  echo "load:     wrk -t6 -c100 -d${secs}s --latency http://127.0.0.1:$PROXY_PORT/"
  echo "profiles: ${window}s windows from t=5s: mutex.pprof, block.pprof, cpu.pprof"
  echo "          (go tool pprof -top <file>; -peek <regexp> shows callers)"
  echo
  grep -E "Requests/sec|Latency  |99%" "$tmp/wrk.txt"
  echo
  echo "## mutex contention (delay), top 15"
  top "$res/mutex.pprof" 15
  echo
  echo "## mutex contention by caller of Lock, cumulative, top 25"
  go tool pprof -top -cum -nodecount=25 "$res/mutex.pprof" 2>/dev/null | sed -n '/flat/,$p'
  echo
  echo "## blocking (delay), top 15"
  top "$res/block.pprof" 15
  echo
  echo "## CPU, top 30"
  top "$res/cpu.pprof" 30
  echo
  echo "proxy exit code: $proxy_exit"
} >"$out"
sed -n '/## mutex contention (delay)/,/## mutex contention by/p' "$out"

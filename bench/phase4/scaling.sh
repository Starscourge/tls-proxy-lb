#!/usr/bin/env bash
# Phase 4 "Done when": throughput at GOMAXPROCS 1, 2 and 4, with backends
# that are not the bottleneck.
#
# The proxy runs on N dedicated cores (one logical CPU each) with
# GOMAXPROCS=N, in front of three backends on CPUs of their own, with
# upstream reuse on. First wrk runs straight at the three backends at once,
# with the same pinning, to show what they and wrk can do without the
# proxy: every proxy run must stay below that.
#
# Usage: bench/phase4/scaling.sh [SECONDS] [CONNECTIONS]
# Writes bench/phase4/results/scaling.txt.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

secs="${1:-15}"
conns="${2:-100}"
threads=6
out="$root/bench/phase4/results/scaling.txt"

build
start_backends 3

echo "== backends alone: three wrk at once, one per backend"
per=$((conns / 3))
i=0
wpids=()
for a in "${backend_addrs[@]}"; do
  wrk_run -t2 -c"$per" -d"${secs}s" --latency "http://$a/" >"$tmp/direct$i.wrk" &
  wpids+=($!)
  i=$((i + 1))
done
wait "${wpids[@]}"
direct=$(cat "$tmp"/direct*.wrk | awk '/^Requests\/sec:/ {s += $2} END {printf "%.0f", s}')
echo "backends alone: $direct req/s"

results=()
for n in 1 2 4; do
  write_config "$tmp/proxy$n.toml" 128
  start_proxy "$tmp/proxy$n.toml" "$n"
  # Warm the pools, then measure.
  wrk_run -t"$threads" -c"$conns" -d2s "http://127.0.0.1:$PROXY_PORT/" >/dev/null
  echo "== GOMAXPROCS $n: wrk -t$threads -c$conns -d${secs}s"
  wrk_run -t"$threads" -c"$conns" -d"${secs}s" --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/p$n.wrk"
  gm=$(grep -o '"gomaxprocs":[0-9]*' "$tmp/access.log" | head -1 | cut -d: -f2)
  stop_proxy
  r=$(rps "$tmp/p$n.wrk")
  results+=("$n $r $(awk '/ 50%/ {print $2}' "$tmp/p$n.wrk") $(awk '/ 99%/ {print $2}' "$tmp/p$n.wrk") $gm")
  echo "GOMAXPROCS $n (proxy logged $gm): $r req/s"
done

{
  echo "# Phase 4: proxy throughput by GOMAXPROCS"
  header
  echo "command:  wrk -t$threads -c$conns -d${secs}s --latency http://127.0.0.1:$PROXY_PORT/ (after a 2 s warm-up)"
  echo "proxy:    3 test backends, round-robin, GET / (14-byte body), upstream reuse on"
  echo
  echo "backends alone (three wrk -t2 -c$per at once, same pinning): $direct req/s"
  echo
  printf "%-11s %12s %10s %10s %8s %16s\n" GOMAXPROCS req/s p50 p99 speedup "% of backends"
  base=""
  for row in "${results[@]}"; do
    read -r n r p50 p99 gm <<<"$row"
    [[ -n $base ]] || base=$r
    printf "%-11s %12s %10s %10s %7.2fx %15.0f%%\n" "$n" "$r" "$p50" "$p99" \
      "$(echo "$r / $base" | bc -l)" "$(echo "100 * $r / $direct" | bc -l)"
  done
  echo
  for n in 1 2 4; do
    echo "## GOMAXPROCS $n"
    cat "$tmp/p$n.wrk"
    echo
  done
  echo "## backends alone"
  for f in "$tmp"/direct*.wrk; do cat "$f"; echo; done
} >"$out"
echo
sed -n '/^backends alone (/,/^$/p;/^GOMAXPROCS /,/^$/p' "$out" | head -8

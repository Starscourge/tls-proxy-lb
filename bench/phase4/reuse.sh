#!/usr/bin/env bash
# Phase 4 "Done when":
#   - under steady load with 100 client connections, new upstream
#     connections per second fall to about zero after warm-up;
#   - a wrk comparison with reuse off and on.
#
# Runs wrk -c 100 against the proxy in front of three backends, once with
# reuse on and then with max_idle_per_backend = 0 (a new connection per
# request, Connection: close), sampling the backends' accepted-connection
# counts every second (less the sampler's own connections) and the
# TIME_WAIT sockets at the end. Reuse on goes first, so the second run's
# TIME_WAIT sockets do not show up in its count.
#
# Usage: bench/phase4/reuse.sh [SECONDS] [CONNECTIONS]
# Writes bench/phase4/results/reuse.txt.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

secs="${1:-15}"
conns="${2:-100}"
threads=4
procs=4
warmup=3
out="$root/bench/phase4/results/reuse.txt"

build
start_backends 3
# TIME_WAIT lasts 60 seconds; an earlier run's sockets would be counted.
echo "== waiting for TIME_WAIT sockets on the backend ports to clear"
for _ in $(seq 70); do [[ $(time_wait) -lt 10 ]] && break; sleep 1; done

# run LABEL MAX_IDLE: one wrk run with the per-second connection counts.
run() {
  local label=$1 max_idle=$2
  write_config "$tmp/$label.toml" "$max_idle"
  start_proxy "$tmp/$label.toml" "$procs"
  local a0 prev now i tw0
  tw0=$(time_wait)
  a0=$(accepted)
  prev=$a0
  wrk_run -t"$threads" -c"$conns" -d"${secs}s" --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/$label.wrk" &
  local wrk_pid=$!
  : >"$tmp/$label.persec"
  local nb=${#backend_addrs[@]}
  for ((i = 1; i <= secs; i++)); do
    sleep 1
    now=$(accepted)
    echo "$i $((now - prev - nb))" >>"$tmp/$label.persec"
    prev=$now
  done
  wait "$wrk_pid"
  local tw
  tw=$(time_wait)
  stop_proxy
  local total
  total=$(awk '{s += $2} END {print s + 0}' "$tmp/$label.persec")
  local after
  after=$(awk -v w="$warmup" '$1 > w {s += $2} END {print s + 0}' "$tmp/$label.persec")
  {
    echo "## reuse $label (max_idle_per_backend = $max_idle)"
    echo
    cat "$tmp/$label.wrk"
    echo
    echo "new upstream connections per second (summed over the backends):"
    awk '{printf "  t=%2ds %6d\n", $1, $2}' "$tmp/$label.persec"
    echo "new upstream connections: $total in all, $after after the first ${warmup}s"
    echo "TIME_WAIT sockets on backend ports: $tw0 at the start, $tw at the end (net.ipv4.tcp_max_tw_buckets = $(sysctl -n net.ipv4.tcp_max_tw_buckets 2>/dev/null || echo '?'))"
    echo "proxy exit code: $proxy_exit"
    echo
  } >"$tmp/$label.section"
  echo "$label: $(rps "$tmp/$label.wrk") req/s, $total new connections ($after after warm-up), TIME_WAIT $tw"
}

echo "== wrk -t$threads -c$conns -d${secs}s, reuse on"
run on 128
echo "== wrk -t$threads -c$conns -d${secs}s, reuse off"
run off 0

{
  echo "# Phase 4: upstream connection reuse off and on"
  header
  echo "command:  wrk -t$threads -c$conns -d${secs}s --latency http://127.0.0.1:$PROXY_PORT/"
  echo "proxy:    GOMAXPROCS $procs; 3 test backends, round-robin, GET / (14-byte body)"
  echo
  cat "$tmp/on.section" "$tmp/off.section"
  echo "summary:"
  echo "  reuse on:  $(rps "$tmp/on.wrk") req/s, p99 $(awk '/ 99%/ {print $2}' "$tmp/on.wrk")"
  echo "  reuse off: $(rps "$tmp/off.wrk") req/s, p99 $(awk '/ 99%/ {print $2}' "$tmp/off.wrk")"
} >"$out"
echo
tail -3 "$out"

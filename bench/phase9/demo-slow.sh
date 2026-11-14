#!/usr/bin/env bash
# Phase 9 experiment C: slow a backend with netem.
#
# In the lab, k6 sends GETs at a constant RATE for DURATION seconds to the
# proxy's one pool of three backends. A third of the way in, netem starts
# on backend2's eth0 (every packet it sends is delayed, or some are
# dropped); two thirds of the way in, it is removed. That runs once per
# proxy config in bench/phase9/configs/ and per fault:
#
#   configs   rr        round-robin, no passive ejection
#             lc        least-connections, no passive ejection
#             lc-eject  least-connections, passive ejection on 100 ms timeouts
#   faults    delay     netem delay 200ms 20ms
#             loss      netem loss 5%
#
# Per run, bench/phase9/plot slow reads k6's per-request lines, the fault
# times and the proxy's health log; per fault it draws slow-FAULT.svg (p99
# per second for the three configs, and each config's per-backend share)
# and prints each run's share, latency and failures with the fault on and
# off. The script fails if k6 could not keep the rate or the clock was
# stepped; it makes no claim about the results themselves.
#
# Usage: bench/phase9/demo-slow.sh [RATE] [DURATION_S] [FAULTS] [CONFIGS]
#   e.g. bench/phase9/demo-slow.sh 500 60 "delay loss" "rr lc lc-eject"
# PLOT_ONLY=1 skips the runs and redraws the graphs and summaries from the
# raw results already in bench/phase9/results/slow/.
# Writes bench/phase9/results/slow/.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

rate="${1:-500}"
duration="${2:-60}"
faults="${3:-delay loss}"
configs="${4:-rr lc lc-eject}"
victim=backend2
victim_addr=10.0.0.12:8000
out="$root/bench/phase9/results/slow"

netem_args() {
  case "$1" in
  delay) echo "delay 200ms 20ms" ;;
  loss) echo "loss 5%" ;;
  *) echo "unknown fault $1" >&2; return 1 ;;
  esac
}

for fault in $faults; do
  [[ -n "${PLOT_ONLY:-}" ]] && break
  args="$(netem_args "$fault")"
  for cfg in $configs; do
    [[ -f "$root/bench/phase9/configs/slow-$cfg.toml" ]] || { echo "no config slow-$cfg.toml" >&2; exit 1; }
    dir="$out/$fault/$cfg"
    mkdir -p "$dir"
    echo "== $fault, $cfg: lab up with bench/phase9/configs/slow-$cfg.toml"
    lab_up "../bench/phase9/configs/slow-$cfg.toml"
    clock_start
    since="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"

    echo "   k6 at $rate/s for ${duration}s; netem $args on $victim from $((duration / 3))s to $((duration * 2 / 3))s"
    k6_run "$rate" "$duration" run &
    k6pid=$!
    t0=$(first_request_ms run)
    sleep_until $((t0 + duration * 1000 / 3))
    "${compose[@]}" exec -T "$victim" tc qdisc add dev eth0 root netem $args
    t_fault=$(now_ms)
    sleep_until $((t0 + duration * 2000 / 3))
    "${compose[@]}" exec -T "$victim" tc qdisc del dev eth0 root
    t_clear=$(now_ms)
    wait "$k6pid"
    clock_end

    proxy_health_log "$since" >"$dir/proxy-health.log"
    {
      echo "# events (epoch ms): first request, netem $args added, removed"
      echo "$t0 load"
      echo "$t_fault fault"
      echo "$t_clear clear"
    } >"$dir/events.txt"
    gzip -9n <"$tmp/run.csv" >"$dir/requests.csv.gz"
    cp "$tmp/run.summary" "$dir/k6-summary.txt"
  done
done

for fault in $faults; do
  runs=()
  for cfg in $configs; do runs+=("$cfg=$out/$fault/$cfg"); done
  # A redraw keeps the header of the run that made the raw results; read
  # it before tee truncates the file.
  header="# bench/phase9/demo-slow.sh $rate $duration at $rev, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  [[ -n "${PLOT_ONLY:-}" ]] && header="$(head -1 "$out/summary-$fault.txt")"
  {
    echo "$header"
    echo "# netem $(netem_args "$fault") on $victim; configs in bench/phase9/configs/; k6 $("${compose[@]}" exec -T loadgen k6 version 2>/dev/null | head -1)"
    plot slow -fault "netem $(netem_args "$fault")" -backend "$victim_addr" -name "$victim" \
      -svg "$out/slow-$fault.svg" "${runs[@]}"
  } | tee "$out/summary-$fault.txt"
done

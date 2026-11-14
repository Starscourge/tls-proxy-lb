#!/usr/bin/env bash
# Phase 9 experiment A: the proxy against nginx.
#
# bench/phase9/bench/compose.yml puts the device under test, the proxy or
# nginx, on the same four CPUs, the same address and the same certificate,
# in front of two nginx origins serving fixed files, with wrk and wrk2 on
# their own CPUs. Before anything is measured both devices are checked to
# negotiate the same TLS version, cipher suite and key-exchange group and
# to run the same number of workers.
#
# Scenarios: ka-1k (1 KB over keep-alive connections), new-1k (1 KB, a new
# TLS connection per request), ka-100k (100 KB over keep-alive), each at
# 50, 200 and 1,000 connections. Per scenario, concurrency and device:
#
#   throughput  wrk at full speed for TPUT_S seconds, RUNS times
#   latency     wrk2 at 70% of that device's median throughput for LAT_S
#               seconds (30 s recorded after wrk2's 10 s calibration),
#               RUNS times
#
# Each cell (scenario, concurrency, device) starts on a freshly created
# device under test and a WARM_S-second wrk run at full speed, thrown
# away: on a laptop the first seconds of load run at turbo clocks the CPU
# cannot sustain, and the runs should all see the sustained clock. Each
# run then follows a GAP_S-second top-up. Each run records the device's
# CPU time and the cell's peak memory so far (its cgroup), and for the
# proxy the garbage collector's CPU (runtime metrics on /metrics). bench/phase9/plot bench
# turns the raw results into summary.txt and the graphs.
#
# Usage: bench/phase9/bench.sh
#   RUNS=3 TPUT_S=15 LAT_S=40 WARM_S=20 GAP_S=5   the defaults
#   QUICK=1                                 one run each, shorter: a smoke test
#   SCENARIOS="ka-1k" CONNS="200" DUTS="proxy nginx"   a subset
#   PLOT_ONLY=1                             redraw from the raw results
# Writes bench/phase9/results/bench/ (OUT= elsewhere). About 70 minutes.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
source "$(dirname "$0")/benchlib.sh"

if [[ -n "${QUICK:-}" ]]; then
  : "${RUNS:=1}" "${TPUT_S:=8}" "${LAT_S:=20}" "${WARM_S:=5}" "${GAP_S:=2}"
fi
export RUNS="${RUNS:-3}" TPUT_S="${TPUT_S:-15}" LAT_S="${LAT_S:-40}" WARM_S="${WARM_S:-20}" GAP_S="${GAP_S:-5}"
scenarios="${SCENARIOS:-ka-1k new-1k ka-100k}"
conns_list="${CONNS:-50 200 1000}"
duts="${DUTS:-proxy nginx}"
out="${OUT:-$root/bench/phase9/results/bench}"

if [[ -z "${PLOT_ONLY:-}" ]]; then
  ((LAT_S > 10)) || { echo "LAT_S must be over 10: wrk2 records nothing in its first 10 s" >&2; exit 1; }
  rm -rf "$out/raw"
  mkdir -p "$out/raw"
  export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
  trap 'bench_down; rm -rf "$tmp"' EXIT
  clock_start

  echo "== images and origins"
  bench_up
  echo "== TLS and workers"
  check_duts | tee "$tmp/duts.txt"
  { env_info; echo "runs:        per cell ${WARM_S}s warm-up, then $RUNS x ${TPUT_S}s throughput and $RUNS x ${LAT_S}s latency, each after ${GAP_S}s at full speed"; cat "$tmp/duts.txt"; } >"$out/env.txt"

  for sc in $scenarios; do
    for c in $conns_list; do
      for d in $duts; do
        dir="$out/raw/$d/$sc-c$c"
        mkdir -p "$dir"
        dut_up "$d"
        warm "$d" "$c" "$sc" "$WARM_S"
        for i in $(seq "$RUNS"); do
          warm "$d" "$c" "$sc" "$GAP_S"
          tput "$d" "$c" "$sc" "$dir/tput-$i"
          echo "   $sc c$c $d throughput run $i: $(rps "$dir/tput-$i.txt") req/s"
        done
        max=$(for i in $(seq "$RUNS"); do rps "$dir/tput-$i.txt"; done | median)
        rate=$((max * 7 / 10))
        for i in $(seq "$RUNS"); do
          warm "$d" "$c" "$sc" "$GAP_S"
          lat "$d" "$c" "$sc" "$rate" "$dir/lat-$i"
          echo "   $sc c$c $d latency run $i at $rate req/s: $(awk '/^ 99.000%/ { print $2 }' "$dir/lat-$i.txt") p99"
        done
      done
    done
  done
  clock_end
fi

plot bench -dir "$out" | tee "$out/summary.txt"

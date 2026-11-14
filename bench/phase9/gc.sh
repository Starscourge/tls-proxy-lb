#!/usr/bin/env bash
# Phase 9: the garbage collector's cost and what tuning it buys.
#
# The proxy alone, in bench.sh's set-up, on the scenario that allocates
# most per second: 1 KB over 200 keep-alive connections. Per GOGC and
# GOMEMLIMIT setting, RUNS throughput runs and RUNS latency runs, each
# setting on a fresh proxy after bench.sh's warm-up. Every latency run is
# at the same rate, 70% of the default setting's median throughput, so the
# p99s differ only by the collector. Reports the garbage collector's share
# of the proxy's CPU (runtime/metrics), throughput, p99 and peak memory;
# bench/phase9/plot variants draws gc.svg.
#
# Usage: bench/phase9/gc.sh
#   SETTINGS="gogc-100 gogc-200"   a subset (names below); RUNS, TPUT_S,
#   LAT_S, WARM_S, GAP_S as for bench.sh; PLOT_ONLY=1 redraws.
# Writes bench/phase9/results/gc/. About 20 minutes.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
source "$(dirname "$0")/benchlib.sh"

export RUNS="${RUNS:-3}" TPUT_S="${TPUT_S:-15}" LAT_S="${LAT_S:-40}" WARM_S="${WARM_S:-20}" GAP_S="${GAP_S:-5}"
settings="${SETTINGS:-gogc-100 gogc-50 gogc-200 gogc-400 gogc-off-limit-256mib}"
sc=ka-1k
c=200
out="${OUT:-$root/bench/phase9/results/gc}"

# setting_env NAME: the environment a setting runs with.
setting_env() {
  case "$1" in
  gogc-off-limit-256mib) echo "GOGC=off GOMEMLIMIT=256MiB" ;;
  gogc-*) echo "GOGC=${1#gogc-}" ;;
  *) echo "unknown setting $1" >&2; return 1 ;;
  esac
}

if [[ -z "${PLOT_ONLY:-}" ]]; then
  rm -rf "$out/raw"
  mkdir -p "$out/raw"
  export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
  trap 'bench_down; rm -rf "$tmp"' EXIT
  clock_start
  echo "== images and origins"
  bench_up
  { env_info; echo "scenario:    $sc at $c connections; per setting ${WARM_S}s warm-up, $RUNS x ${TPUT_S}s throughput, $RUNS x ${LAT_S}s latency at one rate"; } >"$out/env.txt"

  rate=""
  for s in $settings; do
    env=($(setting_env "$s"))
    dir="$out/raw/proxy/$s-c$c"
    mkdir -p "$dir"
    echo "== $s (${env[*]})"
    dut_up proxy "${env[@]}"
    dut_exec proxy sh -c 'tr "\0" "\n" </proc/1/environ | grep -E "^GO(GC|MEMLIMIT)="' >"$dir/env.txt" || true
    warm proxy "$c" "$sc" "$WARM_S"
    for i in $(seq "$RUNS"); do
      warm proxy "$c" "$sc" "$GAP_S"
      tput proxy "$c" "$sc" "$dir/tput-$i"
      echo "   throughput run $i: $(rps "$dir/tput-$i.txt") req/s"
    done
    # The first setting, the default, fixes the rate for all of them.
    if [[ -z $rate ]]; then
      max=$(for i in $(seq "$RUNS"); do rps "$dir/tput-$i.txt"; done | median)
      rate=$((max * 7 / 10))
    fi
    for i in $(seq "$RUNS"); do
      warm proxy "$c" "$sc" "$GAP_S"
      lat proxy "$c" "$sc" "$rate" "$dir/lat-$i"
      echo "   latency run $i at $rate req/s: $(awk '/^ 99.000%/ { print $2 }' "$dir/lat-$i.txt") p99"
    done
  done
  clock_end
fi

plot variants -dir "$out" -title "Tuning the garbage collector" -svg gc.svg -order "$(echo $settings | tr ' ' ',')" |
  tee "$out/summary.txt"

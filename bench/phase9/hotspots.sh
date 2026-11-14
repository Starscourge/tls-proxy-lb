#!/usr/bin/env bash
# Phase 9: profile the proxy under load, and measure a change against the
# commit before it.
#
# Builds the proxy image of BEFORE (a commit) beside this tree's, then for
# each, in bench.sh's set-up on 1 KB over 200 keep-alive connections:
#
#   profiles  with proxy-profile.toml (the mutex profile on), during 30 s
#             of wrk at full speed: CPU, allocations and mutex contention
#             from /debug/pprof, and their top entries as text
#   measured  with proxy.toml: RUNS throughput runs, then RUNS latency
#             runs at one rate for both (70% of BEFORE's median
#             throughput), each on bench.sh's warm-up
#
# bench/phase9/plot variants draws hotspots.svg and the summary.
#
# Usage: bench/phase9/hotspots.sh [BEFORE]
#   BEFORE defaults to $HOTSPOTS_BEFORE; RUNS, TPUT_S, LAT_S, WARM_S, GAP_S
#   as for bench.sh; PLOT_ONLY=1 redraws.
# Writes bench/phase9/results/hotspots/. About 15 minutes.
set -euo pipefail
source "$(dirname "$0")/lib.sh"
source "$(dirname "$0")/benchlib.sh"

before="${1:-${HOTSPOTS_BEFORE:-}}"
export RUNS="${RUNS:-3}" TPUT_S="${TPUT_S:-15}" LAT_S="${LAT_S:-40}" WARM_S="${WARM_S:-20}" GAP_S="${GAP_S:-5}"
sc=ka-1k
c=200
out="${OUT:-$root/bench/phase9/results/hotspots}"

if [[ -z "${PLOT_ONLY:-}" ]]; then
  [[ -n $before ]] || { echo "usage: $0 BEFORE (a commit)" >&2; exit 2; }
  before_rev="$(git -C "$root" rev-parse --short "$before^{commit}")"
  rm -rf "$out/raw" "$out/before" "$out/after"
  mkdir -p "$out/raw"
  export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
  trap 'bench_down; git -C "$root" worktree remove --force "$tmp/before" 2>/dev/null || true; rm -rf "$tmp"' EXIT
  clock_start

  echo "== images: before = $before_rev, after = $rev"
  bench_up
  git -C "$root" worktree add -q --detach "$tmp/before" "$before_rev"
  docker build -q -f "$tmp/before/lab/Dockerfile" --target proxy -t tlslab/proxy:before "$tmp/before" >/dev/null
  { env_info; echo "before:      $before_rev"; echo "after:       $rev"
    echo "scenario:    $sc at $c connections; ${WARM_S}s warm-up, $RUNS x ${TPUT_S}s throughput, $RUNS x ${LAT_S}s latency at one rate"; } >"$out/env.txt"

  # pprof LABEL NAME QUERY: fetch one profile from the proxy's admin
  # listener into $out/LABEL/NAME.pprof.
  pprof() { dut_exec proxy wget -qO- "$admin/debug/pprof/$3" >"$out/$1/$2.pprof"; }

  declare -A image=([before]=tlslab/proxy:before [after]=tlslab/proxy:dev)
  rate=""
  for label in before after; do
    echo "== $label: profiles"
    mkdir -p "$out/$label"
    dut_up proxy BENCH_PROXY_IMAGE="${image[$label]}" BENCH_PROXY_CONFIG=./proxy-profile.toml
    warm proxy "$c" "$sc" "$WARM_S"
    lg wrk-noresume -t"$threads" -c"$c" -d40s "$url_base/1k" >"$out/$label/profile-wrk.txt" 2>&1 &
    w=$!
    sleep 5
    pprof "$label" cpu "profile?seconds=30" &
    p1=$!
    pprof "$label" allocs "allocs?seconds=30" &
    p2=$!
    pprof "$label" mutex "mutex?seconds=30"
    wait "$p1" "$p2" "$w"
    for prof in cpu allocs mutex; do
      (cd "$root" && go tool pprof -top -nodecount=30 "$out/$label/$prof.pprof") >"$out/$label/top-$prof.txt" 2>&1
    done
    echo "   CPU: $(grep -m1 -E '^ +[0-9.]+(s|ms)' "$out/$label/top-cpu.txt" | awk '{ print $NF }') on top"

    echo "== $label: measured"
    dir="$out/raw/proxy/$label-c$c"
    mkdir -p "$dir"
    dut_up proxy BENCH_PROXY_IMAGE="${image[$label]}"
    warm proxy "$c" "$sc" "$WARM_S"
    for i in $(seq "$RUNS"); do
      warm proxy "$c" "$sc" "$GAP_S"
      tput proxy "$c" "$sc" "$dir/tput-$i"
      echo "   throughput run $i: $(rps "$dir/tput-$i.txt") req/s"
    done
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

plot variants -dir "$out" -title "Before and after the profile-guided changes" -svg hotspots.svg | tee "$out/summary.txt"

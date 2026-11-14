# Helpers for the Phase 9 failure demos, which run in the lab
# (lab/docker-compose.yml). Source it; it sets root, compose, tmp, rev.
# shellcheck shell=bash

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose=(docker compose -f "$root/lab/docker-compose.yml")
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
rev="$(cd "$root" && git rev-parse --short HEAD 2>/dev/null || echo unknown)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase9/results' 2>/dev/null || echo '-dirty')"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

now_ms() { date +%s%3N; }

# lab_up [PROXY_CONFIG]: bring the lab up on this commit's build (make
# lab-up builds the images and waits for every container to be healthy),
# with the proxy recreated so it starts with fresh health state, pools
# and log. PROXY_CONFIG, relative to lab/, replaces lab/proxy.toml. Any
# netem qdisc a previous run left on a backend is removed.
lab_up() {
  local cfg="${1:-./proxy.toml}"
  make -s -C "$root" lab-up >"$tmp/lab-up.log" 2>&1 || { cat "$tmp/lab-up.log" >&2; return 1; }
  PROXY_CONFIG="$cfg" "${compose[@]}" up -d --wait --force-recreate --no-deps proxy >"$tmp/lab-up.log" 2>&1 ||
    { cat "$tmp/lab-up.log" >&2; return 1; }
  local b
  for b in backend1 backend2 backend3; do
    "${compose[@]}" exec -T "$b" tc qdisc del dev eth0 root 2>/dev/null || true
  done
}

# k6_run RATE DURATION_S NAME: bench/phase9/demo.js from the loadgen
# container against the proxy's default pool. Writes $tmp/NAME.csv (one
# line per request) and $tmp/NAME.summary (k6's end-of-test summary), and
# fails if k6 failed or skipped an arrival.
k6_run() {
  local rate=$1 duration=$2 name=$3
  "${compose[@]}" exec -T -e RATE="$rate" -e DURATION="${duration}s" loadgen \
    k6 run --quiet --no-color --log-format=raw --summary-export "/tmp/$name.json" - \
    <"$root/bench/phase9/demo.js" >"$tmp/$name.summary" 2>"$tmp/$name.log"
  grep -E '^[0-9]+,' "$tmp/$name.log" >"$tmp/$name.csv"
  "${compose[@]}" exec -T loadgen cat "/tmp/$name.json" >"$tmp/$name.json"
  if grep -q '"dropped_iterations"' "$tmp/$name.json"; then
    echo "k6 dropped arrivals in $name: the load was not constant" >&2
    grep -A3 '"dropped_iterations"' "$tmp/$name.json" >&2
    return 1
  fi
}

# proxy_health_log SINCE: the proxy's health log lines (ejections and
# active-check transitions) since SINCE (RFC 3339), without the access log.
proxy_health_log() {
  "${compose[@]}" logs --no-log-prefix --since "$1" proxy | grep '"msg":"backend ' || true
}

# plot ARGS...: the Phase 9 graph and summary tool, built once per run.
plot() {
  [[ -x "$tmp/plot" ]] || (cd "$root" && go build -o "$tmp/plot" ./bench/phase9/plot)
  "$tmp/plot" "$@"
}

# clock_start / clock_end: the clock guard around a run (plot clock). The
# start fails when the kernel tick is off, the end also when the wall
# clock was stepped in between.
clock_start() { plot clock -save "$tmp/clock"; }
clock_end() { plot clock -check "$tmp/clock"; }

# first_request_ms NAME: the start time of k6's first request in
# $tmp/NAME.log, once there is one (waits up to 30 s).
first_request_ms() {
  local line
  for _ in $(seq 300); do
    line="$(grep -m1 -E '^[0-9]+,' "$tmp/$1.log" 2>/dev/null || true)"
    if [[ -n $line ]]; then echo "${line%%,*}"; return; fi
    sleep 0.1
  done
  echo "k6 sent no request in 30 s" >&2
  return 1
}

# sleep_until MS: sleep until the wall clock reads MS (epoch ms).
sleep_until() {
  local d=$(($1 - $(now_ms)))
  ((d > 0)) && sleep "$(printf '%d.%03d' $((d / 1000)) $((d % 1000)))"
  return 0
}

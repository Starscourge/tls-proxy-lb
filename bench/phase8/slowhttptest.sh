#!/usr/bin/env bash
# Phase 8 "Done when": slowhttptest with 5,000 slow connections; normal
# requests keep their usual latency, and memory and goroutine count stay
# bounded.
#
# Runs in the lab (built from this tree unless --no-up). Three slowhttptest
# containers on their own addresses (10.0.0.31-33) open 1,667 connections
# each to the proxy, 5,001 in all: the lab caps one address at 2,048, so
# split three ways every connection is admitted and the proxy, not the
# per-IP cap, has to cope. Three attacks in turn, 60 s each:
#
#   headers  slowloris: a header line every 4 s, never finishing. The
#            5-second header deadline covers the whole block, so each
#            connection is dropped at 5 s however often it sends.
#   body     slow POST: 8 KiB announced, a few bytes every 2 s. The
#            minimum body rate (500 B/s after 10 s) cuts each one off.
#   read     slow read of a 1 MiB response through a tiny TCP window. The
#            proxy writes as fast as the client reads, so these stay open
#            the whole run: the case that holds 5,000 connections, each
#            with an upstream connection behind it.
#
# During each attack loadgen2 sends 100 normal requests per second (k6,
# constant arrival rate), and the script samples the proxy's
# proxy_client_connections, go_goroutines, go_memstats_heap_inuse_bytes and
# process_resident_memory_bytes every second. Passes when, in every mode: no normal request fails, their
# p99 is within twice the baseline p99 plus 5 ms, goroutines stay under
# 3 per open connection plus 200, resident memory under 1 GiB, and 20 s
# after the attack goroutines are back within 50 of the baseline.
#
# Without --no-up the lab is rebuilt and the proxy (with Prometheus, which
# shares its network namespace) recreated, so the baseline is a fresh
# process; with it, resident memory starts at whatever an earlier run left.
#
# Usage: bench/phase8/slowhttptest.sh [--no-up]
# Writes bench/phase8/results/slowhttptest.txt; exits non-zero on failure.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
compose=(docker compose -f "$root/lab/docker-compose.yml")
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
out="$root/bench/phase8/results/slowhttptest.txt"
image=tlslab/slowhttptest:dev
network=tlslab_lab
per_attacker=1667
attack_s=60
normal_rate=100
target=https://10.0.0.10:8443
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase8/results' ':!*/testdata/fuzz' || echo '-dirty')"
tmp="$(mktemp -d)"
attackers=()
cleanup() {
  for c in "${attackers[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  [[ -n ${sampler:-} ]] && kill "$sampler" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

if [[ "${1:-}" != "--no-up" ]]; then
  [[ -s "$root/lab/certs/default.test.crt" ]] || "$root/lab/gen-certs.sh" "$root/lab/certs"
  echo "== docker compose up"
  "${compose[@]}" up -d --build --wait >/dev/null
  "${compose[@]}" up -d --wait --force-recreate proxy prometheus >/dev/null
fi
echo "== building $image"
docker build -q -f "$root/lab/Dockerfile" --target slowhttptest -t "$image" "$root" >/dev/null

# metrics: the three gauges, space-separated: connections goroutines rss.
metrics() {
  "${compose[@]}" exec -T proxy wget -qO- http://127.0.0.1:9901/metrics 2>/dev/null |
    awk '$1 == "proxy_client_connections" { c = $2 }
         $1 == "go_goroutines" { g = $2 }
         $1 == "process_resident_memory_bytes" { r = $2 }
         $1 == "go_memstats_heap_inuse_bytes" { h = $2 }
         END { printf "%d %d %d %d\n", c, g, r, h }'
}

# normal DURATION NAME: k6 from loadgen2 at normal_rate/s, summary to
# $tmp/NAME.json.
normal() {
  "${compose[@]}" exec -T -e RATE="$normal_rate" -e DURATION="$1" loadgen2 \
    k6 run --quiet --no-color --summary-export "/tmp/$2.json" - \
    <"$root/bench/phase8/normal.js" >"$tmp/$2.txt" 2>&1 || true
  "${compose[@]}" exec -T loadgen2 cat "/tmp/$2.json" >"$tmp/$2.json"
}

# k6 NAME: "p50 p99 failed total" in milliseconds from a k6 summary.
k6stats() {
  python3 -I - "$tmp/$1.json" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))["metrics"]
d = m["http_req_duration"]
failed = int(m.get("status_other", {}).get("count", 0))
total = int(m["iterations"]["count"])
print(f'{d["p(50)"]:.2f} {d["p(99)"]:.2f} {failed} {total}')
PY
}

echo "== baseline: $normal_rate/s normal requests for 30 s"
read -r base_c base_g base_rss base_heap < <(metrics)
normal 30s baseline
read -r base_p50 base_p99 base_failed base_total < <(k6stats baseline)
echo "p50 $base_p50 ms, p99 $base_p99 ms, failed $base_failed of $base_total; goroutines $base_g, rss $((base_rss >> 20)) MiB"

declare -A args=(
  [headers]="-H -c $per_attacker -r 1000 -i 4 -x 24 -l $attack_s -u $target/"
  [body]="-B -c $per_attacker -r 1000 -i 2 -x 8 -s 8192 -l $attack_s -u $target/echo"
  [read]="-X -c $per_attacker -r 1000 -w 256 -y 512 -n 5 -z 32 -k 1 -l $attack_s -u $target/bytes?n=1048576"
)
fail=()
report=()
for mode in headers body read; do
  echo "== $mode: 3 x $per_attacker slow connections for ${attack_s}s, $normal_rate/s normal requests"
  : >"$tmp/$mode.samples"
  (
    while :; do
      echo "$(date +%s) $(metrics)" >>"$tmp/$mode.samples"
      sleep 1
    done
  ) &
  sampler=$!
  attackers=()
  for i in 1 2 3; do
    name="slowhttptest-$mode-$i"
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --name "$name" --network "$network" --ip "10.0.0.3$i" \
      --ulimit nofile=65536:65536 "$image" ${args[$mode]} -p 3 >/dev/null
    attackers+=("$name")
  done
  sleep 8 # connections ramp up
  normal "$((attack_s - 15))s" "$mode"
  # Kept until they exit, so their final report can be read: an attack
  # the proxy fends off early ends before its 60 s.
  for c in "${attackers[@]}"; do
    docker wait "$c" >/dev/null 2>&1 || true
    docker logs "$c" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' >"$tmp/$c.log" || true
    docker rm -f "$c" >/dev/null 2>&1 || true
  done
  sleep 20 # connections the attack left behind time out
  kill "$sampler" 2>/dev/null || true
  wait "$sampler" 2>/dev/null || true
  sampler=""
  read -r _ g_after _ _ < <(metrics)

  read -r p50 p99 failed total < <(k6stats "$mode")
  # Maxima over the attack, and the connections open 30 s in: by then a
  # defence that works has closed the slow headers and slow bodies.
  read -r peak_c peak_g peak_rss peak_heap at30 < <(awk 'NR == 1 { t0 = $1 }
    { if ($2 > c) c = $2; if ($3 > g) g = $3; if ($4 > r) r = $4; if ($5 > h) h = $5
      if (!seen && $1 - t0 >= 30) { at30 = $2; seen = 1 } }
    END { printf "%d %d %d %d %d\n", c, g, r, h, at30 }' "$tmp/$mode.samples")
  p99_limit=$(awk -v b="$base_p99" 'BEGIN { printf "%.2f", 2 * b + 5 }')
  g_limit=$((3 * peak_c + 200))
  [[ $failed -eq 0 ]] || fail+=("$mode: $failed of $total normal requests failed")
  awk -v a="$p99" -v l="$p99_limit" 'BEGIN { exit !(a <= l) }' || fail+=("$mode: normal p99 $p99 ms over $p99_limit ms")
  ((peak_g <= g_limit)) || fail+=("$mode: $peak_g goroutines for $peak_c connections")
  ((peak_rss <= 1 << 30)) || fail+=("$mode: resident memory $((peak_rss >> 20)) MiB")
  ((g_after <= base_g + 50)) || fail+=("$mode: $g_after goroutines 20 s after, baseline $base_g")
  report+=("$(printf '%-8s %7s %7s %7s/%-5s %10s %8s %9s %8s %8s %9s' "$mode" "$p50" "$p99" "$failed" "$total" "$peak_c" "$at30" "$peak_g" "$((peak_heap >> 20))" "$((peak_rss >> 20))" "$g_after")")
  echo "${report[-1]}"
done

{
  echo "# Phase 8: slowhttptest, 3 x $per_attacker slow connections per attack, against the lab proxy"
  echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:   $rev"
  echo "proxy:    lab/proxy.toml (2 CPUs): header_read_ms 5000, client_io_ms 60000,"
  echo "          min_body_rate 500 after body_grace_ms 10000, max_conns_per_ip 2048"
  echo "attacks:  slowhttptest 1.8.2 from 10.0.0.31-33, ${attack_s}s each:"
  for mode in headers body read; do echo "          $mode: slowhttptest ${args[$mode]}"; done
  echo "normal:   k6 from loadgen2, $normal_rate/s constant arrival rate, GET $target/"
  echo
  echo "baseline: p50 $base_p50 ms, p99 $base_p99 ms, $base_failed failed of $base_total;"
  echo "          $base_g goroutines, $((base_heap >> 20)) MiB heap in use, $((base_rss >> 20)) MiB resident"
  echo
  printf '%-8s %7s %7s %13s %10s %8s %9s %8s %8s %9s\n' attack p50_ms p99_ms failed/total peak_conns conns@30s peak_gor heap_mib rss_mib gor_after
  printf '%s\n' "${report[@]}"
  echo
  echo "peak_conns, peak_gor, heap_mib, rss_mib: maxima of proxy_client_connections,"
  echo "go_goroutines, go_memstats_heap_inuse_bytes and process_resident_memory_bytes, sampled"
  echo "each second; conns@30s: open connections 30 s into the attack; gor_after: goroutines"
  echo "20 s after. Resident memory includes heap the Go runtime has not returned to the OS yet."
  echo "slowhttptest's own \"connected\" count below is its view: over TLS it does not notice the"
  echo "proxy closing a connection until it next writes, so conns@30s is the proxy's view."
  echo "Limits: no normal request failed, p99 <= 2 x baseline + 5 ms, goroutines <= 3 per"
  echo "connection + 200, resident memory <= 1 GiB, goroutines after <= baseline + 50."
  for mode in headers body read; do
    echo
    echo "## $mode: slowhttptest's last report, attacker 1"
    grep -E 'test duration|number of connections|closed|pending|connected|error|service available' "$tmp/slowhttptest-$mode-1.log" |
      tail -7 | sed 's/^[[:space:]]*/          /' || true
  done
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} >"$out"

echo
cat "$out"
[[ ${#fail[@]} -eq 0 ]]

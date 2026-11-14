#!/usr/bin/env bash
# Phase 6 "Done when": k6 at twice the limit from one client gets an
# accepted rate within 5% of the limit, and a second client on a different
# source address is unaffected during that test.
#
# Runs in the lab (make lab-up first, so the proxy is this commit's build).
# lab/proxy.toml holds limited.test to 200 requests per second per client
# with a burst of 20. For DURATION seconds, loadgen (10.0.0.20) sends 400
# per second and loadgen2 (10.0.0.21) 100 per second, both with k6's
# constant-arrival-rate executor (bench/phase6/ratelimit.js). Passes when
# k6 kept both schedules (every iteration sent, none dropped), loadgen's
# 200s per second of the scheduled duration are within 5% of 200 (k6's
# own rates divide by wall-clock time, which WSL2 steps), loadgen2 gets no 429,
# neither sees another status or error, and the proxy's
# proxy_rate_limited_requests_total grew by loadgen's 429s.
#
# Usage: bench/phase6/k6.sh [DURATION_S]
# Writes bench/phase6/results/k6.txt and exits non-zero on failure.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
compose=(docker compose -f "$root/lab/docker-compose.yml")
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
duration="${1:-60}"
limit=200
burst=20
heavy_rate=$((2 * limit))
light_rate=$((limit / 2))
out="$root/bench/phase6/results/k6.txt"
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase6/results' || echo '-dirty')"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# metric NAME: proxy_rate_limited_requests_total and friends from the
# proxy's loopback admin listener, read from inside its container.
metric() {
  "${compose[@]}" exec -T proxy wget -qO- http://127.0.0.1:9901/metrics |
    awk -v m="$1" '$1 == m { print $2 }'
}
limited_metric='proxy_rate_limited_requests_total{route="limited.test"}'

# run SERVICE RATE NAME: k6 in SERVICE at RATE/s, script on stdin; the JSON
# summary is copied out to $tmp/NAME.json.
run() {
  local svc=$1 rate=$2 name=$3
  "${compose[@]}" exec -T -e RATE="$rate" -e DURATION="${duration}s" "$svc" \
    k6 run --quiet --no-color --summary-export "/tmp/$name.json" - \
    <"$root/bench/phase6/ratelimit.js" >"$tmp/$name.txt" 2>&1
  "${compose[@]}" exec -T "$svc" cat "/tmp/$name.json" >"$tmp/$name.json"
}

"${compose[@]}" ps --status running --services | grep -qx loadgen2 ||
  { echo "the lab is not up, or has no loadgen2: run make lab-up" >&2; exit 1; }

before=$(metric "$limited_metric")
before=${before:-0}
echo "== k6: loadgen at $heavy_rate/s and loadgen2 at $light_rate/s against limited.test ($limit/s, burst $burst) for ${duration}s"
run loadgen "$heavy_rate" heavy &
h=$!
run loadgen2 "$light_rate" light &
l=$!
wait "$h" "$l"
after=$(metric "$limited_metric")

summary=$(python3 -I - "$tmp/heavy.json" "$tmp/light.json" "$duration" "$limit" "$burst" "$before" "$after" "$heavy_rate" "$light_rate" <<'PY'
import json, sys
heavy, light = (json.load(open(p))["metrics"] for p in sys.argv[1:3])
duration, limit, burst = float(sys.argv[3]), float(sys.argv[4]), int(sys.argv[5])
before, after = float(sys.argv[6]), float(sys.argv[7] or 0)
heavy_rate, light_rate = float(sys.argv[8]), float(sys.argv[9])
def count(m, name):
    return int(m.get(name, {}).get("count", 0))
def iters(m):
    it = m["iterations"]
    return it["count"], it["rate"]
# The rate is taken over the scheduled duration. constant-arrival-rate
# starts iterations on the monotonic clock, as the proxy refills buckets;
# k6's own "rate" divides by wall-clock time, which WSL2 steps when it
# resyncs, so it is reported but not used.
h_it, h_k6rate = iters(heavy)
l_it, l_k6rate = iters(light)
h_secs = l_secs = duration
h_ok, h_429, h_other = count(heavy, "status_200"), count(heavy, "status_429"), count(heavy, "status_other")
l_ok, l_429, l_other = count(light, "status_200"), count(light, "status_429"), count(light, "status_other")
h_drop, l_drop = count(heavy, "dropped_iterations"), count(light, "dropped_iterations")
h_fail = heavy.get("http_req_failed", {}).get("passes", 0)
accepted = h_ok / h_secs
steady = (h_ok - burst) / h_secs
err = (accepted - limit) / limit
fails = []
for name, n, want, drop in (("loadgen", h_it, heavy_rate, h_drop), ("loadgen2", l_it, light_rate, l_drop)):
    if drop or abs(n - want * duration) > 1:
        fails.append(f"{name} sent {n} requests ({drop} dropped), want {want * duration:.0f}: the schedule was not kept")
if abs(err) > 0.05: fails.append(f"accepted rate {accepted:.1f}/s is {err:+.1%} from {limit:.0f}/s")
if l_429: fails.append(f"second client got {l_429} 429s")
if h_other or l_other: fails.append(f"other statuses or errors: heavy {h_other}, light {l_other}")
if after - before != h_429: fails.append(f"proxy counted {after - before:.0f} 429s, k6 saw {h_429}")
print(f"loadgen  (10.0.0.20): {h_it} requests scheduled over {h_secs:.0f}s ({h_it / h_secs:.1f}/s; k6's wall-clock rate {h_k6rate:.1f}/s), {h_drop} dropped by k6")
print(f"          200: {h_ok}  429: {h_429}  other: {h_other}")
print(f"          accepted {accepted:.2f}/s against a limit of {limit:.0f}/s ({err:+.2%});"
      f" {steady:.2f}/s after the burst of {burst}")
print(f"loadgen2 (10.0.0.21): {l_it} requests scheduled over {l_secs:.0f}s ({l_it / l_secs:.1f}/s; k6's wall-clock rate {l_k6rate:.1f}/s), {l_drop} dropped by k6")
print(f"          200: {l_ok}  429: {l_429}  other: {l_other}")
for name, m in (("loadgen", heavy), ("loadgen2", light)):
    d = m["http_req_duration"]
    print(f"{name:9} latency ms: avg {d['avg']:.2f}  p50 {d['p(50)']:.2f}  p99 {d['p(99)']:.2f}  max {d['max']:.2f}")
print(f"proxy_rate_limited_requests_total{{route=\"limited.test\"}}: {before:.0f} -> {after:.0f} (+{after - before:.0f})")
print("result:   " + ("PASS" if not fails else "FAIL: " + "; ".join(fails)))
PY
)

{
  echo "# Phase 6: k6 at twice the rate limit from one client, a second client under it"
  echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:   $rev"
  echo "kernel:   $(uname -sr)"
  echo "k6:       $("${compose[@]}" exec -T loadgen k6 version | head -1)"
  echo "lab:      proxy 10.0.0.10 (2 CPUs), route limited.test -> pool_a (3 backends, least_conn),"
  echo "          rate_limit = { rate = $limit, burst = $burst } per client address"
  echo "load:     k6 constant-arrival-rate for ${duration}s over HTTPS keep-alive (bench/phase6/ratelimit.js):"
  echo "          loadgen $heavy_rate/s, loadgen2 $light_rate/s, at the same time"
  echo
  echo "$summary"
  echo
  echo "## k6 output, loadgen"
  cat "$tmp/heavy.txt"
  echo
  echo "## k6 output, loadgen2"
  cat "$tmp/light.txt"
} >"$out"
echo "$summary"
grep -q '^result:   PASS' <<<"$summary"

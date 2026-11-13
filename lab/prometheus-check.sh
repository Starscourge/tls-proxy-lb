#!/usr/bin/env bash
# Phase 5 "Done when": Prometheus in the lab scrapes the proxy's /metrics.
# Checks that the scrape target is up, that all three backends are reported
# healthy, and that requests sent through the proxy show up in
# proxy_requests_total. The lab must be up (make lab-up).
#
# Usage: lab/prometheus-check.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/docker-compose.yml")
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"

# value QUERY: the first sample value of an instant query, URL-encoded by
# the caller, or empty if there is none.
value() {
  "${compose[@]}" exec -T prometheus \
    wget -qO- "http://127.0.0.1:9090/api/v1/query?query=$1" |
    sed -n 's/.*"value":\[[^,]*,"\([^"]*\)"\].*/\1/p'
}

# expect NAME QUERY WANT: wait up to 30 s for QUERY to return WANT.
expect() {
  local name="$1" query="$2" want="$3" got=""
  for _ in $(seq 60); do
    got="$(value "$query")"
    [[ "$got" == "$want" ]] && { echo "ok: $name = $got"; return; }
    sleep 0.5
  done
  echo "FAIL: $name = '$got', want '$want'" >&2
  exit 1
}

echo "== Prometheus scrapes the proxy"
expect 'up{job="proxy"}' 'up%7Bjob%3D%22proxy%22%7D' 1
expect 'count(proxy_backend_healthy == 1)' 'count(proxy_backend_healthy%3D%3D1)' 3

# Requests through the proxy, then the counter must grow past them.
before="$(value 'sum(proxy_requests_total)')"
before="${before:-0}"
for _ in $(seq 10); do
  "${compose[@]}" exec -T loadgen \
    curl -fsS -o /dev/null --max-time 5 --cacert /work/ca.crt \
      --resolve a.test:8443:10.0.0.10 https://a.test:8443/
done
for _ in $(seq 60); do
  after="$(value 'sum(proxy_requests_total)')"
  if [[ -n "$after" ]] && awk -v a="$after" -v b="$before" 'BEGIN { exit !(a >= b + 10) }'; then
    echo "ok: sum(proxy_requests_total) $before -> $after after 10 requests"
    exit 0
  fi
  sleep 0.5
done
echo "FAIL: sum(proxy_requests_total) went from $before to '${after:-}', want at least 10 more" >&2
exit 1

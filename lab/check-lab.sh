#!/usr/bin/env bash
# Lab checks. Brings the lab up (creating certificates if there are none),
# checks each backend answers from the load generator with its own name,
# checks the proxy's config, its TLS routing and its redirect listener
# (lab/proxy.toml), then checks netem on backend1.
#
# Usage: lab/check-lab.sh [--no-up]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/docker-compose.yml")
# The proxy container runs as the owner of lab/certs, so it can read the
# 0600 keys.
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"

if [[ "${1:-}" != "--no-up" ]]; then
  [[ -s "$here/certs/default.test.crt" ]] || "$here/gen-certs.sh" "$here/certs"
  echo "== docker compose up"
  "${compose[@]}" up -d --build --wait
fi

echo "== services"
"${compose[@]}" ps --format '{{.Service}} {{.State}} {{.Health}}'
for svc in proxy backend1 backend2 backend3 loadgen; do
  state="$("${compose[@]}" ps --format '{{.State}}' "$svc")"
  [[ "$state" == running ]] || { echo "FAIL: $svc is '$state', want running" >&2; exit 1; }
done

echo "== backends reachable from loadgen"
for i in 1 2 3; do
  name="$("${compose[@]}" exec -T loadgen \
    curl -fsS -o /dev/null -D - --max-time 5 "http://10.0.0.1$i:8000/" |
    tr -d '\r' | awk -F': ' 'tolower($1)=="x-backend"{print $2}')"
  [[ "$name" == "backend$i" ]] || { echo "FAIL: 10.0.0.1$i answered X-Backend '$name', want backend$i" >&2; exit 1; }
  echo "ok: 10.0.0.1$i -> $name"
done

# lcurl HOST [curl args...]: an HTTPS request to the proxy from the load
# generator, with HOST as SNI and Host, verified against the lab CA.
lcurl() {
  local host="$1"; shift
  "${compose[@]}" exec -T loadgen \
    curl -sS -o /dev/null --max-time 5 --cacert /work/ca.crt \
      --resolve "$host:8443:10.0.0.10" "$@" "https://$host:8443/"
}

# xbackend HOST [curl args...]: X-Backend of an HTTPS request through the
# proxy.
xbackend() {
  lcurl "$@" -f -D - | tr -d '\r' | awk -F': ' 'tolower($1)=="x-backend"{print $2}'
}

echo "== proxy config"
"${compose[@]}" exec -T proxy /usr/local/bin/proxy -config /etc/proxy/proxy.toml -check-config

echo "== proxy routes from loadgen"
# Default pool, round-robin, with the default certificate. The container
# healthcheck also uses this pool, so six requests are checked to cover all
# three backends rather than landing exactly twice on each.
seen="$(for _ in 1 2 3 4 5 6; do xbackend default.test; done | sort -u | tr '\n' ' ')"
[[ "$seen" == "backend1 backend2 backend3 " ]] || { echo "FAIL: default pool reached '$seen', want all three backends" >&2; exit 1; }
echo "ok: default pool (round_robin) -> $seen"

name="$(xbackend a.test)"
[[ "$name" == backend[123] ]] || { echo "FAIL: a.test answered X-Backend '$name'" >&2; exit 1; }
echo "ok: a.test (least_conn) -> $name"

first="$(xbackend b.test -H 'X-User-Id: alice')"
for _ in 1 2 3 4 5; do
  name="$(xbackend b.test -H 'X-User-Id: alice')"
  [[ "$name" == "$first" ]] || { echo "FAIL: b.test alice went to $first, then $name" >&2; exit 1; }
done
[[ "$first" == backend[123] ]] || { echo "FAIL: b.test answered X-Backend '$first'" >&2; exit 1; }
echo "ok: b.test (consistent_hash) alice -> $first, six times"

echo "== TLS"
# awk reads to the end: exiting early would kill curl with SIGPIPE and fail
# the pipeline.
for host in a.test b.test default.test; do
  subject="$(lcurl "$host" -f -v 2>&1 | awk '/ subject:/ && !n++ {sub(/.*subject: /, ""); print}')"
  [[ "$subject" == "CN=$host" ]] || { echo "FAIL: SNI $host got certificate '$subject'" >&2; exit 1; }
  echo "ok: SNI $host -> certificate $subject"
done
status="$(lcurl a.test -H 'Host: b.test' -w '%{http_code}')"
[[ "$status" == 421 ]] || { echo "FAIL: SNI a.test, Host b.test answered $status, want 421" >&2; exit 1; }
echo "ok: SNI a.test, Host b.test -> 421"

echo "== redirect listener"
location="$("${compose[@]}" exec -T loadgen \
  curl -sS -o /dev/null --max-time 5 -H 'Host: a.test' -w '%{http_code} %{redirect_url}' "http://10.0.0.10:8080/x?y=1")"
[[ "$location" == "308 https://a.test:8443/x?y=1" ]] || { echo "FAIL: redirect answered '$location'" >&2; exit 1; }
echo "ok: http://a.test:8080/x?y=1 -> $location"

exec "$here/netem-check.sh"

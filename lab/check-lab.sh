#!/usr/bin/env bash
# Lab checks. Brings the lab up, checks each backend answers from the load
# generator with its own name, checks the proxy forwards to backend1, then
# checks netem on backend1.
#
# Usage: lab/check-lab.sh [--no-up]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/docker-compose.yml")

if [[ "${1:-}" != "--no-up" ]]; then
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

echo "== proxy forwards from loadgen"
name="$("${compose[@]}" exec -T loadgen \
  curl -fsS -o /dev/null -D - --max-time 5 "http://10.0.0.10:8080/" |
  tr -d '\r' | awk -F': ' 'tolower($1)=="x-backend"{print $2}')"
[[ "$name" == "backend1" ]] || { echo "FAIL: proxy answered X-Backend '$name', want backend1" >&2; exit 1; }
echo "ok: 10.0.0.10:8080 -> $name"

exec "$here/netem-check.sh"

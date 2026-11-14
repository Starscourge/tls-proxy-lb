#!/usr/bin/env bash
# Phase 7 "Done when": docker stop exits cleanly inside the default grace
# period (10 seconds).
#
# Brings the lab up from the current tree (unless --no-up), then from the
# load generator holds one HTTPS keep-alive connection idle and starts a
# 3-second request, and runs `docker compose stop proxy`: SIGTERM to the
# proxy (PID 1 in its container), then SIGKILL after 10 s. It passes when
# the stop takes under 10 s, the container's exit code is 0 (137 would be
# the SIGKILL), the 3-second request gets its 200 with Connection: close,
# and the proxy logs a drain that did not hit shutdown_ms. Afterwards the
# proxy is started again, and Prometheus, which shares its network
# namespace, restarted. The proxy's counters start again from zero, so
# lab/prometheus-check.sh, which compares a sum over a minute, belongs
# before this check, not right after it.
#
# Usage: lab/stop-check.sh [--no-up]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/docker-compose.yml")
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
grace=10
tmp="$(mktemp -d)"
bg=()
cleanup() {
  for pid in "${bg[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

if [[ "${1:-}" != "--no-up" ]]; then
  [[ -s "$here/certs/default.test.crt" ]] || "$here/gen-certs.sh" "$here/certs"
  echo "== docker compose up"
  "${compose[@]}" up -d --build --wait
fi

# lcurl PATH [curl args...]: HTTPS to the proxy from the load generator,
# for a.test, verified against the lab CA.
url() { echo "https://a.test:8443$1"; }
lcurl() {
  "${compose[@]}" exec -T loadgen curl -sS --cacert /work/ca.crt --resolve a.test:8443:10.0.0.10 "$@"
}

echo "== an idle keep-alive connection and a 3-second request through the proxy"
# --rate holds the connection idle in curl's pool between the two
# transfers; the second never starts before the proxy is gone.
lcurl --rate 1/60s -o /dev/null -o /dev/null "$(url /)" "$(url /)" >/dev/null 2>&1 &
bg+=($!)
lcurl --max-time 20 -o /dev/null -D - "$(url /slow?ms=3000)" >"$tmp/slow.txt" 2>"$tmp/slow.err" &
slow=$!
sleep 1

since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "== docker compose stop proxy (grace ${grace}s)"
# The monotonic clock: WSL2 steps the wall clock by seconds.
mono_ms() { python3 -I -c 'import time; print(int(time.monotonic() * 1000))'; }
t0=$(mono_ms)
"${compose[@]}" stop -t "$grace" proxy
t1=$(mono_ms)
stop_ms=$((t1 - t0))
id="$("${compose[@]}" ps -a -q proxy)"
code="$(docker inspect -f '{{.State.ExitCode}}' "$id")"
oom="$(docker inspect -f '{{.State.OOMKilled}}' "$id")"
slow_rc=0
wait "$slow" || slow_rc=$?
status="$(tr -d '\r' <"$tmp/slow.txt" | awk 'NR==1 {print $2}')"
conn_hdr="$(tr -d '\r' <"$tmp/slow.txt" | awk -F': ' 'tolower($1)=="connection"{print tolower($2)}')"
logs="$(docker logs --since "$since" "$id" 2>&1)"
shutting="$(echo "$logs" | grep '"msg":"proxy shutting down"' || true)"
open_conns="$(echo "$shutting" | sed -n 's/.*"open_conns":\([0-9]*\).*/\1/p')"
drain_ms="$(echo "$logs" | grep '"msg":"proxy stopped"' | sed -n 's/.*"drain_ms":\([0-9.]*\).*/\1/p')"
timed_out="$(echo "$logs" | grep -c '"msg":"shutdown timeout reached' || true)"

echo "stop took:          ${stop_ms} ms"
echo "exit code:          $code (OOM killed: $oom)"
echo "open connections:   ${open_conns:-?} when SIGTERM arrived"
echo "drain:              ${drain_ms:-?} ms"
echo "3-second request:   status ${status:-none}, Connection: ${conn_hdr:-none}, curl exit $slow_rc"

fail=()
((stop_ms < grace * 1000)) || fail+=("docker stop took ${stop_ms} ms, not under ${grace}s")
[[ $code == 0 ]] || fail+=("exit code $code")
[[ $status == 200 && $conn_hdr == close && $slow_rc == 0 ]] || fail+=("the request in flight did not complete with 200 and Connection: close: $(cat "$tmp/slow.err")")
[[ -n $shutting ]] || fail+=("no 'proxy shutting down' in the log")
[[ -n $drain_ms ]] || fail+=("no 'proxy stopped' in the log")
[[ $timed_out == 0 ]] || fail+=("the drain hit shutdown_ms")
((${open_conns:-0} >= 2)) || fail+=("expected the idle and the busy connection open at SIGTERM, saw ${open_conns:-none}")

echo "== starting the proxy again"
"${compose[@]}" start proxy >/dev/null
# Prometheus runs in the proxy's network namespace, which the restart
# replaced.
"${compose[@]}" restart prometheus >/dev/null
"${compose[@]}" up -d --wait >/dev/null

if [[ ${#fail[@]} -eq 0 ]]; then
  echo "RESULT: PASS"
else
  printf 'RESULT: FAIL: %s\n' "${fail[@]}" >&2
  exit 1
fi

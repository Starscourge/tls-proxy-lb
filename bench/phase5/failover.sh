#!/usr/bin/env bash
# Phase 5 "Done when": a backend fails under load and the proxy routes
# around it.
#
#   kill  kill -9 one of three backends, then restart it on the same port.
#   stop  SIGSTOP one of three backends (it hangs without closing its
#         connections), then SIGCONT it.
#
# wrk sends GETs through a -race build of the proxy for the whole run. A
# quarter of the way in, backend b2 fails; at 60% it comes back. The script
# watches proxy_backend_healthy on /metrics to time the ejection and the
# re-admission, and splits the failed requests in the proxy's access log
# into: during detection (from the failure until the backend is out of
# rotation), while out of rotation, and the rest. It passes with no failed
# request while the backend is out of rotation, no data race, and a clean
# exit. The target for detection is zero too, since every request is a GET
# that can be retried; the count is recorded either way.
#
# Usage: bench/phase5/failover.sh kill|stop [DURATION_S]
# Writes bench/phase5/results/MODE.txt and exits non-zero on failure.
set -euo pipefail
source "$(dirname "$0")/../phase4/lib.sh"

mode="${1:-}"
duration="${2:-20}"
case "$mode" in kill | stop) ;; *) echo "usage: $0 kill|stop [DURATION_S]" >&2; exit 2 ;; esac
out="$root/bench/phase5/results/$mode.txt"
procs=4
conns=32

build -race
start_backends 3
victim_pid=${pids[1]}
victim=${backend_addrs[1]}

health="
[timeouts]
upstream_response_ms = 1000

[health]
interval_ms      = 500
timeout_ms       = 250
rise             = 2
fall             = 3
passive_failures = 3
eject_ms         = 2000"
write_config "$tmp/proxy.toml" 128 "$health"
start_proxy "$tmp/proxy.toml" "$procs" GORACE="log_path=$tmp/race exitcode=66"

now_ms() { date +%s%3N; }
healthy() {
  curl -fs "http://127.0.0.1:$ADMIN_PORT/metrics" |
    sed -n "s/^proxy_backend_healthy{backend=\"$victim\"} //p"
}
# wait_healthy WANT: poll every 10 ms until b2's healthy gauge is WANT,
# for up to 30 s, and print the time in epoch milliseconds.
wait_healthy() {
  for _ in $(seq 3000); do
    if [[ "$(healthy)" == "$1" ]]; then now_ms; return; fi
    sleep 0.01
  done
  echo "b2 healthy never became $1" >&2
  exit 1
}

echo "== wrk -c$conns for ${duration}s; b2 ($victim) fails at $((duration / 4))s, returns at $((duration * 6 / 10))s"
wrk_run -t2 -c"$conns" -d"${duration}s" --timeout 5s --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/wrk.txt" &
w=$!
t_start=$(now_ms)
sleep "$((duration / 4))"

t_fault=$(now_ms)
if [[ $mode == kill ]]; then
  kill -KILL "$victim_pid"
  wait "$victim_pid" 2>/dev/null || true
else
  kill -STOP "$victim_pid"
fi
t_out=$(wait_healthy 0)
echo "b2 out of rotation after $((t_out - t_fault)) ms"

sleep "$(awk -v d="$duration" -v s="$t_start" -v n="$(now_ms)" 'BEGIN { x = d * 0.6 - (n - s) / 1000; print (x > 0 ? x : 0) }')"
t_return=$(now_ms)
if [[ $mode == kill ]]; then
  taskset -c "$BACKEND_CPUS" "$tmp/testbackend" -listen 127.0.0.1 -port "${victim##*:}" -name b2 >/dev/null 2>"$tmp/backend1-restarted.err" &
  pids+=($!)
else
  kill -CONT "$victim_pid"
fi
t_back=$(wait_healthy 1)
echo "b2 back in rotation $((t_back - t_return)) ms after it returned"
wait "$w"
metrics="$(curl -fs "http://127.0.0.1:$ADMIN_PORT/metrics")"
stop_proxy

# Failed requests by phase, from the access log's timestamps.
during=0 out_of_rotation=0 other=0
while IFS= read -r ts; do
  t=$(date -d "$ts" +%s%3N)
  if ((t >= t_fault && t < t_out)); then during=$((during + 1))
  elif ((t >= t_out && t < t_back)); then out_of_rotation=$((out_of_rotation + 1))
  else other=$((other + 1)); fi
done < <(grep '"msg":"access"' "$tmp/access.log" | grep -v '"status":200' | sed -n 's/.*"time":"\([^"]*\)".*/\1/p')
access=$(grep -c '"msg":"access"' "$tmp/access.log" || true)
races=$(cat "$tmp"/race.* 2>/dev/null | grep -c "WARNING: DATA RACE" || true)
races=$((races + $(grep -c "WARNING: DATA RACE" "$tmp/proxy.err" || true)))

fail=()
[[ $out_of_rotation -eq 0 ]] || fail+=("$out_of_rotation requests failed while b2 was out of rotation")
[[ $other -eq 0 ]] || fail+=("$other requests failed outside the failure window")
[[ $races -eq 0 ]] || fail+=("$races data race reports")
[[ $proxy_exit -eq 0 ]] || fail+=("proxy exit code $proxy_exit")

{
  if [[ $mode == kill ]]; then
    echo "# Phase 5: kill -9 one of three backends under load, then restart it"
  else
    echo "# Phase 5: SIGSTOP one of three backends under load, then SIGCONT"
  fi
  header
  echo "proxy:    -race build, GOMAXPROCS $procs, 3 test backends, round-robin, max_retries 1,"
  echo "          upstream_response_ms 1000; health: interval 500 ms, timeout 250 ms,"
  echo "          rise 2, fall 3, passive_failures 3, eject_ms 2000"
  echo "command:  wrk -t2 -c$conns -d${duration}s --timeout 5s --latency http://127.0.0.1:$PROXY_PORT/"
  echo
  cat "$tmp/wrk.txt"
  echo
  echo "## Timeline (b2 = $victim)"
  echo "failure at:                    $(((t_fault - t_start)))ms ($mode)"
  echo "out of rotation after:         $((t_out - t_fault)) ms"
  echo "returned at:                   $(((t_return - t_start)))ms"
  echo "back in rotation after:        $((t_back - t_return)) ms"
  echo
  echo "## Failed requests (access log, status other than 200)"
  echo "during detection:              $during   (target 0: every request is a retryable GET)"
  echo "while out of rotation:         $out_of_rotation   (must be 0)"
  echo "outside the failure window:    $other"
  echo "access log lines:              $access"
  echo
  echo "## Metrics at the end"
  echo "$metrics" | grep -E '^proxy_(retries_total|backend_transitions_total|backend_healthy)' | grep -v ' 0$' || true
  echo
  echo "data race reports:             $races"
  echo "proxy exit code:               $proxy_exit (after SIGTERM)"
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "RESULT: PASS"; else printf 'RESULT: FAIL: %s\n' "${fail[*]}"; fi
} >"$out"

echo
sed -n '/## Timeline/,$p' "$out"
[[ ${#fail[@]} -eq 0 ]]

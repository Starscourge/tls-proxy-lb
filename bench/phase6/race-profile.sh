#!/usr/bin/env bash
# Phase 6 "Done when": clean with -race under load, and the limiter's lock
# does not stand out in a mutex profile.
#
# Every request goes through the rate-limit table, with limits no request
# reaches, so the lookup cost is measured, not 429s. Two wrk runs at once:
#   - one client: wrk -c100 with no X-Forwarded-For to the default pool,
#     limited to a million per second with a burst of a million. Every
#     request is 127.0.0.1, one bucket behind one shard lock: the worst
#     case for contention.
#   - many clients: wrk -c50 to many.test with a random X-Forwarded-For per
#     request (xff-random.lua), believed because 127.0.0.1 is in
#     trusted_proxies. many.test allows 1 per second with a burst of 10, so
#     each new client's bucket stays short of full for a second: with
#     max_entries 4096 the table stays full and evicting, and sweep_ms 100
#     keeps the sweeper running throughout.
# max_conns_per_ip 1000 puts every accept through the per-IP count.
#
# Run 1, a -race build for DURATION: passes with no race report, no non-2xx
# response or socket error, a clean exit, every access-log line a 200,
# evictions counted and the table never over its cap.
#
# wrk times requests with the wall clock. WSL2 steps that clock (by seconds,
# every half minute or so), and a step backwards makes the request in flight
# on each connection look timed out. The script measures the wall clock
# against the monotonic one; wrk "timeouts" are counted as failures unless
# the clock stepped and there are no more than one per connection, and the
# proxy's own access log (monotonic timings) decides either way.
# Run 2, a normal build with mutex profiling: a 10-second mutex profile
# midway. Passes when the rate-limit table's locks hold under 10% of the
# total mutex wait.
#
# Usage: bench/phase6/race-profile.sh [DURATION]
# Writes bench/phase6/results/{race-profile.txt,mutex.pprof,cpu.pprof} and
# exits non-zero on failure.
set -euo pipefail
source "$(dirname "$0")/../phase4/lib.sh"

duration="${1:-30s}"
window=10
procs=4
res="$root/bench/phase6/results"
out="$res/race-profile.txt"
max_entries=4096

config() {
  write_config "$tmp/proxy.toml" 128 "${1:-}"
  # write_config ends inside [admin]; the rest are top-level key and tables.
  sed -i "1i trusted_proxies = [\"127.0.0.1\"]" "$tmp/proxy.toml"
  cat >>"$tmp/proxy.toml" <<TOML

[rate_limit]
rate        = 1000000
burst       = 1000000
max_entries = $max_entries
sweep_ms    = 100

[limits]
max_conns_per_ip = 1000

[[route]]
host = "many.test"
pool = "default"
rate_limit = { rate = 1, burst = 10 }
TOML
}

# clock_offset: wall clock minus monotonic clock, in seconds.
clock_offset() { python3 -I -c 'import time; print(time.time() - time.monotonic())'; }

# wrk_failures FILE CONNS STEPPED: failure descriptions for one wrk output.
wrk_failures() {
  local f=$1 conns=$2 stepped=$3 name
  name=$(basename "$f" .wrk)
  grep -q "Non-2xx or 3xx responses" "$f" && echo "$name: non-2xx responses"
  local errs timeouts
  errs=$(sed -n 's/.*Socket errors: connect \([0-9]*\), read \([0-9]*\), write \([0-9]*\), timeout \([0-9]*\).*/\1 \2 \3 \4/p' "$f")
  [[ -z $errs ]] && return 0
  read -r c r w timeouts <<<"$errs"
  ((c + r + w > 0)) && echo "$name: socket errors: connect $c, read $r, write $w"
  if ((timeouts > 0)) && ! { [[ $stepped == yes ]] && ((timeouts <= conns)); }; then
    echo "$name: $timeouts timeouts"
  fi
  return 0
}

metric() { curl -fs "http://127.0.0.1:$ADMIN_PORT/metrics" | awk -v m="$1" '$1 == m { print $2 }'; }

# load SECONDS: both wrk runs at once, outputs in $tmp/{one,many}.wrk.
load() {
  wrk_run -t3 -c100 -d"$1" --latency "http://127.0.0.1:$PROXY_PORT/" >"$tmp/one.wrk" &
  local a=$!
  wrk_run -t2 -c50 -d"$1" --latency -s "$root/bench/phase6/xff-random.lua" "http://127.0.0.1:$PROXY_PORT/" >"$tmp/many.wrk" &
  local b=$!
  wait "$a" "$b"
}

fail=()

# ---- Run 1: -race ----
build -race
start_backends 3
config
start_proxy "$tmp/proxy.toml" "$procs" GORACE="log_path=$tmp/race exitcode=66"
echo "== run 1: -race build, wrk -c100 (one client) and -c50 (random X-Forwarded-For) for $duration"
offset0=$(clock_offset)
peak=0
load "$duration" &
l=$!
while kill -0 "$l" 2>/dev/null; do
  n=$(metric proxy_rate_limit_buckets || echo 0)
  n=${n%.*}
  ((${n:-0} > peak)) && peak=${n:-0}
  sleep 0.5
done
wait "$l"
evictions=$(metric proxy_rate_limit_evictions_total)
buckets_max=$(metric proxy_rate_limit_buckets_max)
limited=$(curl -fs "http://127.0.0.1:$ADMIN_PORT/metrics" | awk '/^proxy_rate_limited_requests_total/ { s += $2 } END { print s + 0 }')
offset1=$(clock_offset)
stop_proxy
step=$(echo "$offset1 - $offset0" | bc -l)
stepped=$(awk -v s="$step" 'BEGIN { print (s > 0.1 || s < -0.1) ? "yes" : "no" }')
cp "$tmp/one.wrk" "$tmp/race-one.wrk"
cp "$tmp/many.wrk" "$tmp/race-many.wrk"
while read -r line; do [[ -n $line ]] && fail+=("$line"); done < <(
  wrk_failures "$tmp/race-one.wrk" 100 "$stepped"
  wrk_failures "$tmp/race-many.wrk" 50 "$stepped"
)
access=$(grep -c '"msg":"access"' "$tmp/access.log" || true)
non200=$(grep '"msg":"access"' "$tmp/access.log" | grep -vc '"status":200' || true)
logged_errors=$(grep '"msg":"access"' "$tmp/access.log" | grep -c '"error"' || true)
[[ $non200 -eq 0 ]] || fail+=("$non200 non-200 access log lines")
[[ $logged_errors -eq 0 ]] || fail+=("$logged_errors access log lines with an error")
races=$(cat "$tmp"/race.* 2>/dev/null | grep -c "WARNING: DATA RACE" || true)
races=$((races + $(grep -c "WARNING: DATA RACE" "$tmp/proxy.err" || true)))
race_exit=$proxy_exit
[[ $races -eq 0 ]] || fail+=("$races data race reports")
[[ $race_exit -eq 0 ]] || fail+=("-race proxy exit code $race_exit")
[[ ${evictions%.*} -gt 0 ]] || fail+=("no evictions: the table was never full")
[[ $peak -le ${buckets_max%.*} ]] || fail+=("table held $peak buckets, cap ${buckets_max%.*}")
[[ $limited == 0 ]] || fail+=("$limited requests rate limited")

# ---- Run 2: mutex profile ----
build
config "mutex_profile_fraction = 10"
start_proxy "$tmp/proxy.toml" "$procs"
secs=$((window + 10))
echo "== run 2: mutex and CPU profiles over ${window}s under the same load"
load "${secs}s" &
l=$!
sleep 5
admin="http://127.0.0.1:$ADMIN_PORT/debug/pprof"
curl -fs -o "$res/mutex.pprof" "$admin/mutex?seconds=$window" &
c1=$!
curl -fs -o "$res/cpu.pprof" "$admin/profile?seconds=$window" &
c2=$!
wait "$c1" "$c2" "$l"
stop_proxy

# total_ms FILE [FOCUS]: the profile's total delay in milliseconds,
# restricted to stacks matching FOCUS if given.
total_ms() {
  local focus=()
  [[ -n ${2:-} ]] && focus=(-focus "$2")
  go tool pprof -top "${focus[@]}" "$1" 2>/dev/null |
    sed -n 's/.*of \([0-9.]*\)\([mu]*s\) total.*/\1 \2/p;s/.*accounting for \([0-9.]*\)\([mu]*s\), .*/\1 \2/p' | head -1 |
    awk '{v = $1; if ($2 == "s") v *= 1000; if ($2 == "us") v /= 1000; print v + 0}'
}
mutex_ms=$(total_ms "$res/mutex.pprof")
rl_ms=$(go tool pprof -top -focus 'ratelimit\.' "$res/mutex.pprof" 2>/dev/null |
  sed -n 's/.*Showing nodes accounting for \([0-9.]*\)\([mu]*s\), .*/\1 \2/p' |
  awk '{v = $1; if ($2 == "s") v *= 1000; if ($2 == "us") v /= 1000; print v + 0}')
rl_ms=${rl_ms:-0}
rl_pct=$(echo "100 * $rl_ms / $mutex_ms" | bc -l)
rate_one=$(rps "$tmp/one.wrk")
rate_many=$(rps "$tmp/many.wrk")
awk -v p="$rl_pct" 'BEGIN { exit !(p < 10) }' || fail+=("$(printf 'rate-limit locks hold %.1f%% of mutex wait' "$rl_pct")")

{
  echo "# Phase 6: rate limiting under load, -race and mutex profile"
  header
  echo "proxy:    GOMAXPROCS $procs, 3 test backends, round-robin, upstream reuse on;"
  echo "          [rate_limit] rate 1e6, burst 1e6 (nothing refused), max_entries $max_entries, sweep_ms 100;"
  echo "          route many.test rate_limit { rate 1, burst 10 }; trusted_proxies [127.0.0.1]; max_conns_per_ip 1000"
  echo "load:     wrk -t3 -c100 --latency http://127.0.0.1:$PROXY_PORT/   (one client, one bucket)"
  echo "          wrk -t2 -c50 --latency -s bench/phase6/xff-random.lua    (many.test, a random client per request)"
  echo
  echo "## Run 1: -race build, $duration"
  echo "data races: $races   proxy exit: $race_exit   requests rate limited: $limited"
  printf "access log: %d requests, %d not 200, %d with an error\n" "$access" "$non200" "$logged_errors"
  printf "wall clock against monotonic over the run: %+.3f s (stepped: %s)\n" "$step" "$stepped"
  echo "table: peak $peak buckets sampled every 0.5s, cap ${buckets_max%.*}, ${evictions%.*} evictions"
  echo "### one client"
  cat "$tmp/race-one.wrk"
  echo "### many clients"
  cat "$tmp/race-many.wrk"
  echo
  echo "## Run 2: normal build, mutex_profile_fraction 10, profiles over ${window}s from t=5s"
  echo "one client:   $rate_one req/s;  many clients: $rate_many req/s"
  printf "mutex wait: %.1f ms in all; in the rate-limit table (stacks through ratelimit.*): %.2f ms, %.2f%%\n" \
    "$mutex_ms" "$rl_ms" "$rl_pct"
  echo
  echo "## mutex contention by lock site (callers of Unlock; runtime.unlock is the runtime's own locks)"
  go tool pprof -peek 'sync.\(\*Mutex\).Unlock$|^runtime.unlock$' "$res/mutex.pprof" 2>/dev/null | sed -n '/flat/,$p' | grep -v '^ *$'
  echo
  echo "## mutex contention by caller, cumulative, top 25"
  go tool pprof -top -cum -nodecount=25 "$res/mutex.pprof" 2>/dev/null | sed -n '/flat/,$p'
  echo
  echo "## CPU in the rate limiter (stacks through ratelimit.*)"
  go tool pprof -top -focus 'ratelimit\.' -nodecount=10 "$res/cpu.pprof" 2>/dev/null | sed -n '/Showing nodes/,$p'
  echo
  echo "## CPU, top 25"
  go tool pprof -top -nodecount=25 "$res/cpu.pprof" 2>/dev/null | sed -n '/Showing nodes/,$p'
  echo
  if [[ ${#fail[@]} -eq 0 ]]; then echo "result: PASS"; else printf 'result: FAIL: %s\n' "${fail[@]}"; fi
} >"$out"
sed -n '/## Run 1/,/### one/p;/## Run 2/,/^$/p;/^result/p' "$out"
[[ ${#fail[@]} -eq 0 ]]

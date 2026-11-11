#!/usr/bin/env bash
# Checks that `tc qdisc add dev eth0 root netem delay 100ms` inside backend1
# adds about 100 ms to its responses, measured with curl from the load
# generator. The lab must already be up.
#
# netem delays every packet backend1 sends, so a new connection pays the delay
# twice: once on the SYN-ACK (connect) and once on the response. The check is
# on the response alone, time_starttransfer - time_connect; the connect delta
# is printed for reference.
#
# Usage: lab/netem-check.sh [DELAY_MS] [SAMPLES]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/docker-compose.yml")
delay_ms="${1:-100}"
samples="${2:-21}"
target=http://10.0.0.11:8000/
# Accept the measured response delay within this window around delay_ms.
lo=$((delay_ms * 80 / 100))
hi=$((delay_ms * 130 / 100))

# measure prints "<median connect ms> <median response ms>".
measure() {
  "${compose[@]}" exec -T loadgen sh -c '
    for i in $(seq '"$samples"'); do
      curl -fsS -o /dev/null --max-time 10 \
        -w "%{time_connect} %{time_starttransfer}\n" '"$target"'
    done' |
  awk '{ c[NR] = $1 * 1000; r[NR] = ($2 - $1) * 1000 }
       function median(a, n,   i, j, t) {
         for (i = 1; i <= n; i++) for (j = i + 1; j <= n; j++) if (a[j] < a[i]) { t = a[i]; a[i] = a[j]; a[j] = t }
         return (n % 2) ? a[(n + 1) / 2] : (a[n / 2] + a[n / 2 + 1]) / 2
       }
       END { printf "%.2f %.2f\n", median(c, NR), median(r, NR) }'
}

tc_backend1() { "${compose[@]}" exec -T backend1 tc "$@"; }
cleanup() { tc_backend1 qdisc del dev eth0 root 2>/dev/null || true; }
trap cleanup EXIT
cleanup

read -r base_c base_r < <(measure)
echo "baseline:   connect ${base_c} ms, response ${base_r} ms (median of $samples)"

tc_backend1 qdisc add dev eth0 root netem delay "${delay_ms}ms"
tc_backend1 qdisc show dev eth0
read -r net_c net_r < <(measure)
echo "netem ${delay_ms}ms: connect ${net_c} ms, response ${net_r} ms (median of $samples)"

cleanup
read -r after_c after_r < <(measure)
echo "removed:    connect ${after_c} ms, response ${after_r} ms"

awk -v b="$base_r" -v n="$net_r" -v bc="$base_c" -v nc="$net_c" -v a="$after_r" \
    -v lo="$lo" -v hi="$hi" -v d="$delay_ms" '
  BEGIN {
    dr = n - b; dc = nc - bc
    printf "added: response %.1f ms (want %d-%d), connect %.1f ms\n", dr, lo, hi, dc
    if (dr < lo || dr > hi) { print "FAIL: response delay outside window"; exit 1 }
    if (a - b > d * 0.2)    { print "FAIL: delay still present after qdisc removal"; exit 1 }
    print "PASS: netem works"
  }'

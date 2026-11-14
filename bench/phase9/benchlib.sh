# Helpers for the Phase 9 nginx benchmark (bench/phase9/bench/compose.yml):
# bench.sh, profile.sh and gc.sh source this after lib.sh.
# shellcheck shell=bash

bc=(docker compose -f "$root/bench/phase9/bench/compose.yml")
url_base="https://default.test:8443"
threads="${THREADS:-8}"
# The device under test's admin listener, inside its container.
admin="http://127.0.0.1:9901"

# scenario_path NAME, scenario_args NAME: the URL path and extra wrk
# arguments of a scenario.
scenario_path() {
  case "$1" in
  ka-1k | new-1k) echo /1k ;;
  ka-100k) echo /100k ;;
  *) echo "unknown scenario $1" >&2; return 1 ;;
  esac
}
scenario_args() {
  case "$1" in
  new-1k) echo "-H Connection:close" ;;
  *) echo "" ;;
  esac
}

# bench_up: build the images and start the origins and the load generator.
bench_up() {
  test -s "$root/lab/certs/default.test.crt" || "$root/lab/gen-certs.sh" "$root/lab/certs" >/dev/null
  "${bc[@]}" --profile proxy --profile nginx build -q >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; return 1; }
  "${bc[@]}" up -d --wait origin1 origin2 loadgen >"$tmp/up.log" 2>&1 || { cat "$tmp/up.log" >&2; return 1; }
}

bench_down() { "${bc[@]}" --profile proxy --profile nginx down >/dev/null 2>&1 || true; }

# dut_up DUT [ENV=VALUE...]: (re)create the device under test, proxy or
# nginx, with the other stopped, so each cell starts with fresh memory, GC
# state and connections. ENV pairs go to the proxy's environment (GOGC,
# GOMEMLIMIT, BENCH_PROXY_IMAGE, BENCH_PROXY_CONFIG) through the compose
# file's variables. Images are built by bench_up only, so an image named
# here is never rebuilt from the working tree.
dut_up() {
  local dut=$1 other=nginx
  shift
  [[ $dut == nginx ]] && other=proxy
  "${bc[@]}" --profile "$other" stop "$other" >/dev/null 2>&1 || true
  env "$@" "${bc[@]}" --profile "$dut" up -d --wait --force-recreate --no-deps --no-build "$dut" >"$tmp/dut.log" 2>&1 ||
    { cat "$tmp/dut.log" >&2; "${bc[@]}" --profile "$dut" logs "$dut" | tail -20 >&2; return 1; }
}

dut_exec() { "${bc[@]}" exec -T "$@"; }
lg() { "${bc[@]}" exec -T loadgen "$@"; }

# cgroup DUT: "cpu_usec mem_peak_bytes" of the device under test's
# container; the peak is since the container started, so the cell's peak
# so far.
cgroup() {
  dut_exec "$1" sh -c 'awk "/^usage_usec/ {print \$2}" /sys/fs/cgroup/cpu.stat; cat /sys/fs/cgroup/memory.peak' | tr '\n' ' '
}

# gc_cpu: "gc_seconds busy_seconds" from the proxy's runtime CPU classes:
# the garbage collector's CPU and all the CPU the process used (total
# minus idle). Empty for nginx.
gc_cpu() {
  [[ $1 == proxy ]] || { echo ""; return; }
  dut_exec proxy wget -qO- "$admin/metrics" | awk '
    $1 == "go_cpu_classes_gc_total_cpu_seconds_total" { gc = $2 }
    $1 == "go_cpu_classes_total_cpu_seconds_total" { total = $2 }
    $1 == "go_cpu_classes_idle_cpu_seconds_total" { idle = $2 }
    END { printf "%s %s", gc, total - idle }'
}

# tls_line DUT: what a handshake with the device under test negotiates,
# from the load generator's OpenSSL (wrk and wrk2 use the same library).
tls_line() {
  lg sh -c "openssl s_client -connect default.test:8443 -servername default.test </dev/null 2>&1" |
    awk '/^Protocol:/ { p = $2 } /^New,/ { c = $NF } /Negotiated TLS1.3 group:/ { g = $NF }
         END { printf "%s %s %s", p, c, g }'
}

# dut_procs DUT: the proxy's GOMAXPROCS, or nginx's worker count.
dut_procs() {
  if [[ $1 == proxy ]]; then
    dut_exec proxy wget -qO- "$admin/metrics" | awk '$1 == "go_sched_gomaxprocs_threads" { print $2 }'
  else
    dut_exec nginx sh -c 'ps -o args | grep -c "^nginx: worker process"'
  fi
}

# check_duts: both devices under test negotiate the same TLS version,
# suite and group, and have the same number of workers. Prints one line
# per device for env.txt.
check_duts() {
  local p n pp np
  dut_up proxy && p="$(tls_line)" && pp="$(dut_procs proxy)"
  dut_up nginx && n="$(tls_line)" && np="$(dut_procs nginx)"
  echo "tls-proxy-lb: $p, GOMAXPROCS $pp"
  echo "nginx:        $n, worker_processes $np"
  if [[ "$p" != "$n" || -z "$p" ]]; then
    echo "the two negotiate different TLS: proxy \"$p\", nginx \"$n\"" >&2
    return 1
  fi
  if [[ "$pp" != "$np" ]]; then
    echo "GOMAXPROCS $pp but $np nginx workers" >&2
    return 1
  fi
}

# measure DUT FILE CMD...: run CMD in the load generator, its output to
# FILE.txt, with the device under test's CPU, peak memory and (proxy) GC
# CPU across it in FILE.meta.
measure() {
  local dut=$1 file=$2
  shift 2
  local c0 c1 g0 g1 t0 t1
  c0=($(cgroup "$dut"))
  g0=($(gc_cpu "$dut"))
  t0=$(now_ms)
  lg "$@" >"$file.txt" 2>&1
  t1=$(now_ms)
  c1=($(cgroup "$dut"))
  g1=($(gc_cpu "$dut"))
  {
    echo "wall_ms=$((t1 - t0))"
    echo "cpu_usec=$((c1[0] - c0[0]))"
    echo "mem_peak_bytes=${c1[1]}"
    if [[ ${#g1[@]} -eq 2 ]]; then
      echo "gc_cpu_s=$(awk -v a="${g1[0]}" -v b="${g0[0]}" 'BEGIN { printf "%.6f", a - b }')"
      echo "go_busy_cpu_s=$(awk -v a="${g1[1]}" -v b="${g0[1]}" 'BEGIN { printf "%.6f", a - b }')"
    fi
  } >"$file.meta"
}

# warm DUT CONNS SCENARIO SECONDS: wrk at full speed, results thrown away:
# before a cell's first run, long enough for the CPU to leave turbo for
# its sustained clock (WARM_S); before each later run, a short top-up
# (GAP_S) that opens the run's connections and keeps it there.
warm() {
  local path args
  path="$(scenario_path "$3")"
  args="$(scenario_args "$3")"
  lg wrk-noresume -t"$threads" -c"$2" -d"$4s" $args "$url_base$path" >/dev/null 2>&1
}

# tput DUT CONNS SCENARIO FILE: wrk at full speed for TPUT_S seconds.
# wrk-noresume is wrk 4.2.0 built never to resume a TLS session
# (lab/Dockerfile), so a new connection is a full handshake.
tput() {
  local path args
  path="$(scenario_path "$3")"
  args="$(scenario_args "$3")"
  measure "$1" "$4" wrk-noresume -t"$threads" -c"$2" -d"${TPUT_S:-15}s" --timeout 10s --latency $args "$url_base$path"
}

# lat DUT CONNS SCENARIO RATE FILE: wrk2 at a constant RATE for LAT_S
# seconds (wrk2 calibrates for its first 10 s and records after that; it
# is patched like wrk-noresume).
lat() {
  local path args
  path="$(scenario_path "$3")"
  args="$(scenario_args "$3")"
  measure "$1" "$5" wrk2 -t"$threads" -c"$2" -d"${LAT_S:-40}s" -R"$4" --timeout 10s --latency $args "$url_base$path"
  echo "rate=$4" >>"$5.meta"
}

# rps FILE.txt: wrk's Requests/sec.
rps() { awk '/^Requests\/sec:/ { print int($2) }' "$1"; }

# median: the median of the numbers on stdin (the lower one of an even count).
median() { sort -g | awk '{ a[NR] = $1 } END { print a[int((NR + 1) / 2)] }'; }

# env_info: the machine and software the results come from.
env_info() {
  echo "date:        $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:      $rev"
  echo "cpu:         $(lscpu | sed -n 's/^Model name: *//p'), $(nproc) vCPUs"
  echo "memory:      $(awk '/MemTotal/ { printf "%.1f GiB", $2 / 1048576 }' /proc/meminfo)"
  echo "kernel:      $(uname -sr)$(grep -qi microsoft /proc/version && echo ' (WSL2)')"
  echo "clock:       $(cat /sys/devices/system/clocksource/clocksource0/current_clocksource)"
  echo "docker:      $(docker version --format '{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})')$(docker info --format ' {{.OperatingSystem}}' 2>/dev/null)"
  echo "go:          $(sed -n 's/^ARG GO_VERSION=//p' "$root/lab/Dockerfile") (proxy image), $(sed -n 's/^go //p' "$root/go.mod") in go.mod"
  echo "nginx:       $(dut_exec nginx nginx -v 2>&1 | sed 's/^nginx version: //'), $(dut_exec nginx sh -c 'apk info -v 2>/dev/null | grep ^libssl3-')"
  echo "loadgen:     wrk $(sed -n 's/^ARG WRK_VERSION=//p' "$root/lab/Dockerfile") and wrk2 $(sed -n 's/^ARG WRK2_COMMIT=//p' "$root/lab/Dockerfile" | cut -c1-12), both never resuming TLS sessions; $(lg sh -c 'apk info -v 2>/dev/null | grep -E "^libssl3-"')"
  echo "network:     host (loopback in Docker Desktop's VM)"
  echo "cpusets:     device under test ${BENCH_DUT_CPUS:-2,4,6,8}; origins ${BENCH_ORIGIN1_CPUS:-10,11} and ${BENCH_ORIGIN2_CPUS:-12,13}; load generator ${BENCH_LOADGEN_CPUS:-14-21}"
  echo "wrk threads: $threads"
}

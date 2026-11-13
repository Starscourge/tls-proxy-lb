# Shared setup for the Phase 4 bench scripts: build the binaries, start
# test backends and the proxy on localhost with CPU pinning, wait for them,
# and tear everything down on exit. Source it; do not run it.
#
# CPU layout (override with the env vars). One logical CPU per physical
# core for the proxy, so GOMAXPROCS N means N cores; the backends and wrk
# get CPUs of their own so the proxy is what is measured.
#   PROXY_CPUS    0,2,4,6       (the first N are used for GOMAXPROCS N)
#   BACKEND_CPUS  8-15
#   WRK_CPUS      16-21
# Ports: PROXY_PORT (18080), ADMIN_PORT (18090), BACKEND_PORT (18001..).

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROXY_CPUS="${PROXY_CPUS:-0,2,4,6}"
BACKEND_CPUS="${BACKEND_CPUS:-8-15}"
WRK_CPUS="${WRK_CPUS:-16-21}"
PROXY_PORT="${PROXY_PORT:-18080}"
ADMIN_PORT="${ADMIN_PORT:-18090}"
BACKEND_PORT="${BACKEND_PORT:-18001}"

# Recorded before anything is written, so results files do not make the
# tree look dirty.
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!bench/phase4/results' || echo '-dirty')"

tmp="$(mktemp -d)"
pids=()
proxy_pid=""
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

need() { command -v "$1" >/dev/null || { echo "$1 not installed" >&2; exit 1; }; }
need wrk; need taskset; need curl; need ss

port_free() {
  if ss -Htln "sport = :$1" | grep -q .; then echo "port $1 is in use" >&2; exit 1; fi
}

# build [-race]: proxy (optionally with the race detector) and testbackend
# into $tmp.
build() {
  echo "== building proxy ${1:-} and testbackend"
  (cd "$root" && go build ${1:-} -o "$tmp/proxy" ./cmd/proxy && go build -o "$tmp/testbackend" ./cmd/testbackend)
}

# start_backends N: N test backends on consecutive ports, pinned to
# BACKEND_CPUS. Sets backend_addrs.
backend_addrs=()
start_backends() {
  local n=$1 i port
  backend_addrs=()
  for ((i = 0; i < n; i++)); do
    port=$((BACKEND_PORT + i))
    port_free "$port"
    taskset -c "$BACKEND_CPUS" "$tmp/testbackend" -listen 127.0.0.1 -port "$port" -name "b$((i + 1))" >/dev/null 2>"$tmp/backend$i.err" &
    pids+=($!)
    backend_addrs+=("127.0.0.1:$port")
  done
  for a in "${backend_addrs[@]}"; do wait_http "http://$a/healthz"; done
}

# write_config FILE MAX_IDLE [ADMIN_EXTRA]: one plain HTTP listener and a
# round-robin pool over backend_addrs, with an admin listener.
write_config() {
  local file=$1 max_idle=$2 extra=${3:-} list="" a
  for a in "${backend_addrs[@]}"; do list+="{ address = \"$a\" }, "; done
  cat >"$file" <<TOML
default_pool = "default"

[[listener]]
address = "127.0.0.1:$PROXY_PORT"
mode    = "http"

[pool.default]
algorithm = "round_robin"
backends  = [ ${list%, } ]

[upstream]
max_idle_per_backend = $max_idle

[admin]
address = "127.0.0.1:$ADMIN_PORT"
$extra
TOML
}

# start_proxy CONFIG NCPUS [ENV...]: the proxy on the first NCPUS of
# PROXY_CPUS with GOMAXPROCS=NCPUS. Access log to $tmp/access.log.
start_proxy() {
  local cfg=$1 n=$2
  shift 2
  local cpus
  cpus=$(echo "$PROXY_CPUS" | cut -d, -f1-"$n")
  port_free "$PROXY_PORT"
  port_free "$ADMIN_PORT"
  env GOMAXPROCS="$n" "$@" taskset -c "$cpus" "$tmp/proxy" -config "$cfg" >"$tmp/access.log" 2>"$tmp/proxy.err" &
  proxy_pid=$!
  pids+=($proxy_pid)
  wait_http "http://127.0.0.1:$PROXY_PORT/healthz"
}

# stop_proxy: SIGTERM and wait; sets proxy_exit to the exit code. Not to
# be called in a subshell, which could not wait for the proxy.
proxy_exit=""
stop_proxy() {
  kill -TERM "$proxy_pid"
  proxy_exit=0
  wait "$proxy_pid" || proxy_exit=$?
  local keep=()
  for pid in "${pids[@]}"; do [[ $pid == "$proxy_pid" ]] || keep+=("$pid"); done
  pids=("${keep[@]}")
  proxy_pid=""
}

wait_http() {
  for _ in $(seq 100); do
    curl -fs -o /dev/null "$1" && return 0
    sleep 0.05
  done
  echo "$1 did not come up" >&2
  cat "$tmp"/*.err >&2 || true
  exit 1
}

# wrk_run ARGS...: wrk pinned to WRK_CPUS.
wrk_run() { taskset -c "$WRK_CPUS" wrk "$@"; }

# rps FILE: requests per second from wrk output.
rps() { awk '/^Requests\/sec:/ {print $2}' "$1"; }

# accepted: connections accepted so far, summed over the backends. Each
# call opens one connection per backend itself, which the count includes.
accepted() {
  local sum=0 a n
  for a in "${backend_addrs[@]}"; do
    n=$(curl -fs "http://$a/stats" | sed -n 's/.*"conns_accepted":\([0-9]*\).*/\1/p')
    sum=$((sum + n))
  done
  echo "$sum"
}

# time_wait: sockets in TIME_WAIT to or from the backend ports.
time_wait() {
  local filter="" a
  for a in "${backend_addrs[@]}"; do filter+="sport = :${a##*:} or dport = :${a##*:} or "; done
  ss -Htan state time-wait "( ${filter% or } )" | wc -l
}

header() {
  echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:   $rev"
  echo "go:       $(go version | awk '{print $3, $4}')"
  echo "kernel:   $(uname -sr)"
  echo "cpu:      $(lscpu | sed -n 's/^Model name: *//p'), $(nproc) logical CPUs"
  echo "pinning:  proxy $PROXY_CPUS (first GOMAXPROCS of them), backends $BACKEND_CPUS, wrk $WRK_CPUS"
}

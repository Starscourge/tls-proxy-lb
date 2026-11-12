#!/usr/bin/env bash
# Phase 3 "Done when": a testssl.sh report with no weak-protocol or
# weak-cipher findings.
#
# Brings the lab up (built from the working tree, lab/proxy.toml), runs
# testssl.sh from its pinned Docker image on the lab network against
# https://a.test:8443 on the proxy (10.0.0.10) with the lab CA trusted, and
# saves the report. Running inside the lab network works the same with
# Docker Engine and Docker Desktop, where a container cannot reach the
# host's loopback.
#
# Usage: bench/phase3/testssl.sh [REPORT]
#   REPORT defaults to docs/testssl-phase3.txt.
# Exits non-zero if the scan did not cover protocols and ciphers, or if any
# finding in those sections is rated LOW or worse. Findings of LOW or worse
# in other sections are listed at the top of the report for the record.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="${1:-$root/docs/testssl-phase3.txt}"
image="ghcr.io/testssl/testssl.sh:3.2@sha256:a0b0f35dea9e129804eae31e47d310d5c15423c9e495b5c2826ec0b5e4494cc7"
network="tlslab_lab"
proxy_ip="10.0.0.10"
target="https://a.test:8443"
rev="$(cd "$root" && git rev-parse --short HEAD)$(cd "$root" && git diff --quiet HEAD -- . ':!docs' || echo '-dirty')"

for bin in docker python3; do
  command -v "$bin" >/dev/null || { echo "testssl: $bin not found" >&2; exit 1; }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cd "$root"
export LAB_UID="${LAB_UID:-$(id -u)}" LAB_GID="${LAB_GID:-$(id -g)}"
[[ -s lab/certs/default.test.crt ]] || lab/gen-certs.sh lab/certs
echo "== lab up"
docker compose -f lab/docker-compose.yml up -d --build --wait

echo "== testssl.sh against $target on $proxy_ip (a few minutes)"
# Run as us so the JSON report can be written into the mounted directory.
# testssl.sh's exit code reflects its findings, which are judged below.
docker run --rm --network "$network" -u "$(id -u):$(id -g)" \
  -v "$work:/data" -v "$root/lab/certs/ca.crt:/ca.crt:ro" "$image" \
  --color 0 --warnings off --quiet --add-ca /ca.crt --ip "$proxy_ip" \
  --jsonfile-pretty /data/report.json "$target" >"$work/report.txt" || true
[[ -s "$work/report.json" ]] || { echo "testssl: no JSON report" >&2; cat "$work/report.txt" >&2; exit 1; }

# judge prints the summary and exits 1 if the protocol or cipher sections
# are missing or hold a finding rated LOW or worse.
judge() {
  python3 -I - "$work/report.json" <<'PY'
import json, sys
bad = {"LOW", "MEDIUM", "HIGH", "CRITICAL"}
gated = ("protocols", "ciphers")
doc = json.load(open(sys.argv[1]))
results = doc.get("scanResult", [])
failed = False
for section in gated:
    if not any(isinstance(r.get(section), list) and r[section] for r in results):
        print(f"FAIL: the scan has no {section} section")
        failed = True
for result in results:
    for section, items in result.items():
        if not isinstance(items, list):
            continue
        for f in items:
            if not isinstance(f, dict) or f.get("severity") not in bad:
                continue
            gate = section in gated
            failed |= gate
            print(f"{'FAIL' if gate else 'note'}: [{section}] {f.get('id')} {f.get('severity')}: {f.get('finding')}")
print("protocols and ciphers: " + ("FAILED" if failed else "no findings rated LOW or worse"))
sys.exit(1 if failed else 0)
PY
}

status=0
summary="$(judge)" || status=$?
mkdir -p "$(dirname "$out")"
{
  echo "# Phase 3: testssl.sh report"
  echo "date:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit:   $rev"
  echo "testssl:  $image"
  echo "target:   $target on $proxy_ip (lab proxy, lab/proxy.toml), lab CA trusted"
  echo "command:  bench/phase3/testssl.sh"
  echo
  echo "## Findings rated LOW or worse (protocol and cipher sections gate the check)"
  echo
  echo "$summary"
  echo
  echo "## Full report"
  echo
  cat "$work/report.txt"
} >"$out"
echo "$summary"
echo "report: $out"
exit "$status"

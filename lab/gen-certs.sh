#!/usr/bin/env bash
# Creates a local CA and server certificates for the lab hostnames.
#
# Usage: lab/gen-certs.sh [OUT_DIR] [HOST...]
#   OUT_DIR defaults to lab/certs; HOSTs default to a.test b.test
#   default.test (the proxy's default certificate in the lab).
#
# The CA (ca.crt, ca.key) is reused if it already exists, so a trust store
# that holds it stays valid; server certificates are re-issued on every run.
# Keys are ECDSA P-256. Server certificates last 397 days, the most browsers
# accept (and testssl.sh does not flag). Nothing here is fit for production
# use.
set -euo pipefail

out="${1:-$(dirname "$0")/certs}"
shift || true
hosts=("$@")
[[ ${#hosts[@]} -gt 0 ]] || hosts=(a.test b.test default.test)

command -v openssl >/dev/null || { echo "gen-certs: openssl not found" >&2; exit 1; }
mkdir -p "$out"
umask 077

ca_key="$out/ca.key"
ca_crt="$out/ca.crt"
if [[ ! -s "$ca_key" || ! -s "$ca_crt" ]]; then
  openssl ecparam -name prime256v1 -genkey -noout -out "$ca_key"
  openssl req -x509 -new -key "$ca_key" -sha256 -days 3650 \
    -subj "/CN=tls-proxy-lb lab CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash" \
    -out "$ca_crt" 2>/dev/null
  echo "gen-certs: created CA $ca_crt"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for host in "${hosts[@]}"; do
  [[ "$host" =~ ^[A-Za-z0-9.-]+$ ]] || { echo "gen-certs: bad hostname '$host'" >&2; exit 1; }
  key="$out/$host.key"
  crt="$out/$host.crt"
  openssl ecparam -name prime256v1 -genkey -noout -out "$key"
  openssl req -new -key "$key" -subj "/CN=$host" -out "$tmp/$host.csr"
  cat > "$tmp/$host.ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:$host
authorityKeyIdentifier=keyid
subjectKeyIdentifier=hash
EXT
  openssl x509 -req -in "$tmp/$host.csr" -CA "$ca_crt" -CAkey "$ca_key" \
    -CAcreateserial -CAserial "$tmp/ca.srl" -sha256 -days 397 \
    -extfile "$tmp/$host.ext" -out "$crt" 2>/dev/null
  echo "gen-certs: issued $crt"
done

# Certificates are public; keys stay owner-only.
chmod 644 "$ca_crt" "$out"/*.crt

#!/bin/bash
# Start the loopback Radicale CardDAV server for the M6 gate. Generates a
# fresh throwaway bcrypt user file in /tmp/opencode (never committed, the
# repo's data dirs stay out of git) and waits for /.well-known/carddav.
# Password: Bridge-Dav-Gate-Only — loopback test credential, dev only.
set -euo pipefail
cd "$(dirname "$0")/../.."
VENV=${RADICALE_VENV:-/tmp/opencode/radicale-venv}
HTPASSWD=/tmp/opencode/radicale-htpasswd

if [ ! -x "$VENV/bin/radicale" ]; then
  echo "radicale venv missing at $VENV (python3 -m venv + pip install radicale)" >&2
  exit 1
fi

if [ ! -s "$HTPASSWD" ] || [ "${REGEN_DAV_USER:-}" = "1" ]; then
  "$VENV/bin/python" - "$HTPASSWD" <<'PY'
import bcrypt, sys
with open(sys.argv[1], "wb") as f:
    f.write(b"bridge:" + bcrypt.hashpw(b"Bridge-Dav-Gate-Only", bcrypt.gensalt()) + b"\n")
PY
  chmod 600 "$HTPASSWD"
fi

if [ -s /tmp/opencode/radicale.pid ] && kill -0 "$(cat /tmp/opencode/radicale.pid)" 2>/dev/null; then
  kill "$(cat /tmp/opencode/radicale.pid)" 2>/dev/null || true
  sleep 0.5
fi

mkdir -p /tmp/radicale-data
nohup "$VENV/bin/radicale" --config dev/radicale.conf \
  > /tmp/opencode/radicale.log 2>&1 &
echo $! > /tmp/opencode/radicale.pid

for _ in $(seq 1 50); do
  if curl -s -o /dev/null -u "bridge:Bridge-Dav-Gate-Only" \
      -X OPTIONS -L http://127.0.0.1:5230/.well-known/carddav; then
    echo "radicale ready on 127.0.0.1:5230"
    exit 0
  fi
  sleep 0.2
done
echo "radicale did not come up; see /tmp/opencode/radicale.log" >&2
exit 1

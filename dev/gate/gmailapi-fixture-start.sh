#!/bin/bash
# Start the M10 fixture Gmail API rig: an in-process Gmail API fixture and
# a bridge whose backend is backend = "gmail_api" pointed at it. Used for
# the cross-client gate (jmap-tui browses API mode) — no Google account,
# no OAuth, no real mail.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

for f in /tmp/opencode/gmailapi-fixture.pid /tmp/opencode/bridge-gmailapi.pid; do
  if [ -s "$f" ]; then kill "$(cat "$f")" 2>/dev/null || true; fi
done
sleep 0.3

go build -o /tmp/opencode/gmailfixture ./dev/gate/gmailfixture || exit 1
go build -o /tmp/opencode/jmap-bridge-gmailapi ./cmd/jmap-bridge || exit 1

rm -rf /tmp/opencode/gmailapi-fixture-data
/tmp/opencode/gmailfixture > /tmp/opencode/gmailfixture.log 2>&1 &
echo $! > /tmp/opencode/gmailapi-fixture.pid

URL=""
for _ in $(seq 1 50); do
  URL=$(grep -m1 '^URL=' /tmp/opencode/gmailfixture.log 2>/dev/null | cut -d= -f2- || true)
  [ -n "$URL" ] && break
  sleep 0.1
done
if [ -z "$URL" ]; then
  echo "fixture did not start" >&2
  cat /tmp/opencode/gmailfixture.log >&2 || true
  exit 1
fi

cat > /tmp/opencode/gmailapi-fixture.toml <<EOF
listen = "127.0.0.1:8081"
base_url = "http://127.0.0.1:8081"
data_dir = "/tmp/opencode/gmailapi-fixture-data"
log_level = "info"

[sync]
interval = "1s"

[auth]
mode = "token"

[[accounts]]
id = "gapi"
name = "Gmail API fixture"
address = "fixture@example.test"
token = "dev-token-gapi-local-only-0123456789"
backend = "gmail_api"

  [accounts.gmail_api]
  endpoint = "$URL"
  token = "fixture-token"
  quota_units_per_second = 1000
EOF

/tmp/opencode/jmap-bridge-gmailapi --config /tmp/opencode/gmailapi-fixture.toml \
  > /tmp/opencode/bridge-gmailapi.log 2>&1 &
echo $! > /tmp/opencode/bridge-gmailapi.pid

for _ in $(seq 1 60); do
  curl -sf -u any:dev-token-gapi-local-only-0123456789 \
    http://127.0.0.1:8081/gapi/.well-known/jmap >/dev/null 2>&1 && break
  sleep 0.2
done

echo "fixture: $URL"
echo "bridge:  http://127.0.0.1:8081/gapi (token dev-token-gapi-local-only-0123456789)"
echo "logs:    /tmp/opencode/gmailfixture.log /tmp/opencode/bridge-gmailapi.log"

#!/bin/bash
# M13 gate: Gmail API Pub/Sub push, entirely on the in-process fixture — no
# Google account, no OAuth, no Pub/Sub, no public HTTPS. It wires the real
# sync engine + the real HTTP push endpoint to test/fixturegmail and proves
# (FR-S.14):
#   - a verified push makes a foreign change visible well inside 2 s;
#   - a push for an account with no verifier is a 404;
#   - a forged push is rejected (401) and does not nudge the engine;
#   - the watch renews before its expiration (expiry simulation);
#   - watch="poll" never arms a watch and still converges.
# The production Pub/Sub path is documented in PLAN §17.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

exec go test ./test/pushgate -count=1 -v

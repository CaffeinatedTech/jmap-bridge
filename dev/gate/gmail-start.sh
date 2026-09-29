#!/bin/bash
# Launch the bridge for the Gmail live gate. All secrets come from .env
# (0600, gitignored): the OAuth2 client secret and the token-encryption
# key. Never prints secret values.
set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root
if [ -s /tmp/opencode/bridge-gmail.pid ]; then
  kill "$(cat /tmp/opencode/bridge-gmail.pid)" 2>/dev/null || true
  sleep 0.5
fi
set -a
source .env
set +a
# Build from the committed tree when the working tree is dirty: the
# gate result must describe M4, never half-landed work from another
# milestone's session happening to share this checkout.
SRC="$PWD"
if ! git diff --quiet || ! git diff --cached --quiet; then
  # Sibling of the repo so the go.mod replace to ../go-imap resolves.
  SRC="$PWD/../jmap-bridge-gate-src"
  rm -rf "$SRC"
  git worktree add --detach "$SRC" HEAD >/dev/null 2>&1 || {
    echo "could not snapshot HEAD for a clean build" >&2; exit 1; }
fi
go build -C "$SRC" -o /tmp/opencode/jmap-bridge-gmail ./cmd/jmap-bridge || exit 1
if [ "$SRC" != "$PWD" ]; then git worktree remove --force "$SRC" >/dev/null 2>&1 || true; fi
/tmp/opencode/jmap-bridge-gmail --config dev/config-gmail.toml > /tmp/opencode/bridge-gmail.log 2>&1 &
echo $! > /tmp/opencode/bridge-gmail.pid
for i in $(seq 1 50); do
  curl -sf -u any:dev-token-gmail-local-only http://127.0.0.1:8080/gmail/.well-known/jmap >/dev/null 2>&1 && break
  sleep 0.2
done

# Gmail live-gate rig (M4)

The rig for the M4 gate against the live Gmail account: the bridge on
loopback with the OAuth2 account (`dev/config-gmail.toml`) and the gate
script (JMAP over the bridge + an independent `imaplib` XOAUTH2 client).

Secrets live in `.env` (never committed): the OAuth2 client secret and
`JMAP_BRIDGE_SECRET_KEY`, which unseals the stored refresh token in
`data/bridge.db` — losing that key means re-consenting in a browser.

## Resume after a reboot

    bash dev/gate/gmail-start.sh        # bridge up (builds the binary first)
    python3 dev/gate/m4gate.py          # one gate pass, results on stdout

Gmail throttles IMAP writes per **account** — reads keep working, writes
answer `OK [THROTTLED]` and are silently dropped — after sustained heavy
usage. The restriction lifts after roughly 24 h of *decreased* usage, so
do not probe it in a loop: retrying works against the cooldown, and every
client on the account (the bridge, a mail sorter, a desktop client)
counts. Stop them all, wait, then run a single pass:

    bash dev/gate/gmail-start.sh
    python3 dev/gate/m4gate.py

If the first `CREATE` does not stick, stop and wait another day; if it
does, the gate runs to completion. (An unattended watcher was tried and
removed 2026-09-30: its periodic probes fed the quarantine it was waiting
out.)

## The helper binary

`m4gate.py` shells out to `test/live/gmailtoken` (build:
`go build -o /tmp/opencode/gmailtoken ./test/live/gmailtoken`) to mint
a fresh access token from the sealed refresh token into a 0600 file for
the independent IMAP client.

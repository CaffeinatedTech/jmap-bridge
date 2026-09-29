# Gmail live-gate rig (M4)

The rig for the M4 gate against the live Gmail account: the bridge on
loopback with the OAuth2 account (`dev/config-gmail.toml`), the gate
script (JMAP over the bridge + an independent `imaplib` XOAUTH2 client),
and a cooldown watcher that probes Gmail's IMAP-write quarantine and
runs the gate unattended once writes stick again.

Secrets live in `.env` (never committed): the OAuth2 client secret and
`JMAP_BRIDGE_SECRET_KEY`, which unseals the stored refresh token in
`data/bridge.db` — losing that key means re-consenting in a browser.

## Resume after a reboot

    bash dev/gate/gmail-start.sh        # bridge up (builds the binary first)
    python3 dev/gate/m4gate.py          # one gate pass, results on stdout

To wait out Gmail's IMAP-write quarantine (reads work, writes dropped
with `OK [THROTTLED]` for up to ~24 h after heavy IMAP usage):

    setsid nohup bash dev/gate/m4-watch.sh > /dev/null 2>&1 &

The watcher probes every 15 min; when a CREATE sticks it starts the
bridge and runs the gate, writing `dev/gate/m4gate-result.txt`.

## The helper binary

`m4gate.py` shells out to `test/live/gmailtoken` (build:
`go build -o /tmp/opencode/gmailtoken ./test/live/gmailtoken`) to mint
a fresh access token from the sealed refresh token into a 0600 file for
the independent IMAP client.

# Gmail live-gate rig (M4)

The rig for the M4 gate against the live Gmail account: the bridge on
loopback with the OAuth2 account (`dev/config-gmail.toml`) and the gate
script (JMAP over the bridge + an independent `imaplib` XOAUTH2 client).

Secrets live in `.env` (never committed): the OAuth2 client secret and
`JMAP_BRIDGE_SECRET_KEY`, which unseals the stored refresh token in
`data/bridge.db` — losing that key means re-consenting in a browser.
`dev/config-gmail.toml` holds a real account address and OAuth client id and
is **gitignored**: copy `dev/config-gmail.toml.example` and fill in your own
values before running the Gmail gates.

## What the gate covers

The 2026-10-01 partial runs demonstrated live: OAuth2 consent → stored
token → XOAUTH2 IMAP connect (`tier=condstore`, COMPRESS); the Gmail
namespace and roles; implicit All Mail; label create/list both ways; the
bridge→Gmail message-label write; and an archived message still searchable
in All Mail. The fixture tiers cover those repeatably. `m4gate.py` therefore
re-checks only the deltas those runs never reached:

1. a message label applied in Gmail appears in the bridge;
2. archiving from the bridge drops INBOX membership (then undoes it, so no
   mail is left moved);
3. compose + send over Gmail SMTP (XOAUTH2) → one Sent copy, one delivered.

## Running it — once, not in a loop

    bash dev/gate/gmail-start.sh        # bridge up (builds the binary first)
    # wait for the first sync pass to settle (INBOX non-empty in the bridge)
    python3 dev/gate/m4gate.py

Gmail quarantines IMAP writes per **account** after sustained use: reads keep
working, writes answer `OK [THROTTLED]` and are silently dropped, and the
restriction lifts only after ~24 h of *decreased* usage. Every client on the
account counts (the bridge, a mail sorter, a desktop client). `m4gate.py`
aborts on the first `[THROTTLED]` and paces its writes, so a run can never
deepen the quarantine. If it aborts: stop every writer, wait another day,
then try once more — re-running a throttled gate is what turns this into a
never-ending loop. (An unattended watcher did exactly that and was removed
2026-09-30.)

## The helper binary

`m4gate.py` shells out to `test/live/gmailtoken` (build:
`go build -o /tmp/opencode/gmailtoken ./test/live/gmailtoken`) to mint
a fresh access token from the sealed refresh token into a 0600 file for
the independent IMAP client.

# Gmail API mode (M10)

The read path is proven two ways; neither touches existing mail.

## Fixture gate (no Google account)

    bash dev/gate/gmailapi-fixture-start.sh
    # bridge: http://127.0.0.1:8081/gapi  (token dev-token-gapi-local-only-0123456789)

Starts `dev/gate/gmailfixture` (the in-process Gmail API fixture with a
seeded INBOX/Sent/Drafts/Trash/Spam + user label + two messages) and a
bridge with `backend = "gmail_api"` pointed at it. Then, from the
jmap-tui repo:

    JMAP_TUI_TEST_URL=http://127.0.0.1:8081/gapi \
    JMAP_TUI_TEST_USER=any \
    JMAP_TUI_TEST_PASSWORD=dev-token-gapi-local-only-0123456789 \
      go test ./internal/jmapclient/ -run TestLiveSessionAndMailboxes -v

## Live read-only gate

    python3 dev/gate/gmailapi-live-gate.py

Uses the user's real Gmail account on 127.0.0.1:8080 (the registered OAuth
redirect URI). It reuses the stored refresh token; if Google revoked it the
script prints the consent URL and exits — approve that once, then re-run.
It creates one test label + message (never touching existing mail), proves
the bridge discovers roles and browses them through the API backend, stars
the message out-of-band via the Gmail API and observes it through history
incremental, then deletes exactly the test message and label. Backfill is
scoped with `backfill_query` to the test label so the gate caches a handful
of messages, not the mailbox.


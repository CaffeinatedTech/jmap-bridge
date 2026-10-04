# jmap-bridge

> **A JMAP server that fronts an IMAP account.** Mail, sending, and contacts —
> served over JMAP from one container, synced with the mail server behind it.

`jmap-bridge` lets any JMAP client (including [jmap-tui](https://github.com/CaffeinatedTech/jmap-tui))
work against mailboxes that only speak IMAP/SMTP/CardDAV — Gmail, Dovecot, cPanel
hosts, Namecheap Private Email, and friends. It keeps a local hybrid cache so JMAP
reads are fast, writes go straight back to the real server, and sends go out over SMTP.

```text
                       ┌──────────────────────────────────────────┐
   JMAP client         │  jmap-bridge (container)                 │
  (jmap-tui, …)  ───►  │  session · POST /jmap · upload/download  │
      JMAP/HTTP        │  EventSource push                        │        IMAP  ──► read, flags, folders
                       │  SQLite cache (headers + FTS + blobs)    │  ───►  SMTP  ──► send
                       │  sync engine (QRESYNC/CONDSTORE/baseline)│  ───►  CardDAV ──► contacts
                       └──────────────────────────────────────────┘
```

## Why

JMAP is a better protocol than IMAP, but most mailboxes still only offer IMAP —
and plenty of JMAP clients refuse to implement anything else. Rather than add a
second protocol to every client, run one bridge:

- **Clients stay JMAP-only.** No IMAP code in the client, no per-client sync engine.
- **The bridge is the only thing that needs to know about IMAP's quirks** — CONDSTORE
  vs QRESYNC vs neither, Gmail labels, expunge semantics, provider quirks.
- **One place for credentials, OAuth tokens, and the local cache.**

## Features

- **JMAP Mail (RFC 8621)** — browse, search, read, flag, move, archive, delete,
  threads, drafts. Session, result references, `/changes`, state strings, SSE push.
- **Sending** — `EmailSubmission/set` over SMTP (password or OAuth2), Bcc
  stripped on the wire, the sent message filed into Sent either by the
  client's `onSuccessUpdateEmail` or by an IMAP `APPEND` — never both — and
  `Identity/get` for the from-address.
- **Contacts (RFC 9610)** — `AddressBook/*` and `ContactCard/*`, translated to the
  provider's CardDAV (vCard ↔ JSContact per RFC 9553/9554). Photo blobs included.
- **Hybrid local cache** — headers, flags, MIME structure and a full-text index are
  always local; message bodies are fetched from IMAP the first time you open one
  and cached from then on. Fast startup, bounded disk, offline re-reads.
- **Search-driven backfill** — a text search over messages you haven't downloaded
  yet returns header matches instantly, then hydrates bodies in the background and
  pushes results to the client as they land.
- **Live** — IMAP `IDLE` → SQLite → JMAP `/changes` + Server-Sent Events, so
  changes from other mail clients show up in yours within seconds.
- **Gmail-ready** — OAuth2 (XOAUTH2) only, `X-GM-LABELS` ↔ mailboxes, All Mail /
  archive semantics, `X-GM-THRID` seeding, CONDSTORE tier.
- **Any IMAP server** — runtime capability detection with three sync strategies
  (QRESYNC → CONDSTORE → baseline scan) and graceful degradation throughout.
- **Multi-account** — path-prefixed accounts on one origin, one container.

## Implemented JMAP surface

| Capability | Methods & endpoints | Status |
|---|---|---|
| `urn:ietf:params:jmap:core` (RFC 8620) | session resource, `POST /jmap` (batched, result references), `/changes`, upload/download, error taxonomy | v0.1 |
| `urn:ietf:params:jmap:mail` (RFC 8621) | `Mailbox/get\|query\|changes\|set`, `Email/get\|query\|changes\|set\|import`, `Thread/get`, `Identity/get` | v0.1 |
| `urn:ietf:params:jmap:submission` | `EmailSubmission/set` (create → SMTP, or the provider API in Gmail API mode) | v0.1 |
| `urn:ietf:params:jmap:contacts` (RFC 9610) | `AddressBook/get\|changes`, `ContactCard/get\|changes\|set` | v0.1 |
| push (RFC 8620 §7.3) | EventSource `/{account}/eventsource` (SSE) | v0.1 |
| roadmap | `Email/queryChanges`, `Email/copy`, `Email/parse`, `Thread/changes`, `ContactCard/query`, `AddressBook/set`, `Identity/set`, `EmailSubmission/get\|query`, `SearchSnippet/get`, `PushSubscription` | later |
| not planned | JMAP Calendar, `Principal`/sharing, `VacationResponse`, `MDN`, `Quota`, Sieve | see PLAN roadmap |

If a capability is not configured (no CardDAV URL → no contacts capability), the
session simply doesn't advertise it and clients degrade — jmap-tui hides contacts
rather than failing.

## Backends

| Backend | IMAP auth | Sync tier | Send | Contacts |
|---|---|---|---|---|
| **Gmail / Google Workspace** | OAuth2 (XOAUTH2) | CONDSTORE (Gmail has no QRESYNC) | SMTP + XOAUTH2 | CardDAV, OAuth2-only, **no contact groups** |
| **Gmail API mode** (`backend = "gmail_api"`) | OAuth2 (Google) | REST `history.list` | Gmail API (`drafts.send`/`messages.send`, files Sent itself) | same OAuth token |
| **Dovecot** (self-hosted) | password | QRESYNC | SMTP | needs a separate CardDAV server |
| **cPanel mail** (Dovecot-based) | password | QRESYNC (detected) | SMTP | when the host offers CardDAV |
| **Namecheap Private Email** | password | detected at runtime | SMTP | when the host offers CardDAV |
| **Radicale** (CardDAV, self-hosted) | — | — | — | password — **M6-verified** end-to-end (sync-collection, conditional PUT/DELETE) |
| anything else | password or OAuth2 | detected at runtime | SMTP | any RFC 6352 server |

Contacts are optional: supply a CardDAV URL (or let the bridge discover it via
`/.well-known/carddav`, RFC 6764) and the contacts capability appears — only
after the first sync succeeded, so a half-working server never advertises
half-working data.

## Quick start

```sh
mkdir -p ./data
cat > ./config.toml <<'EOF'
listen = "0.0.0.0:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"

[[accounts]]
id = "personal"
name = "Personal"
address = "me@example.com"
token = "change-me"

  [accounts.imap]
  host = "imap.example.com"
  port = 993
  tls = true
  auth = "password"
  username = "me@example.com"
  password = "…"

  [accounts.smtp]
  host = "smtp.example.com"
  port = 465
  tls = "implicit"
EOF

docker run -d --name jmap-bridge \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/config.toml:/config/config.toml:ro" \
  -v "$PWD/data:/data" \
  ghcr.io/caffeinatedtech/jmap-bridge:latest
```

The image runs as a non-root user (FR-D.1); `--user` maps it to your
uid so `./data` stays writable and owned by you.

Images are published to GHCR on `v*` tags (multi-arch amd64/arm64 —
`deploy/README.md` covers building and publishing). Until the first tag exists,
build from source (`go build -o jmap-bridge ./cmd/jmap-bridge`) and run the
binary with `--config ./config.toml` instead of the container.

Then point a client at it:

```toml
[accounts.personal]
url = "http://127.0.0.1:8080/personal"   # path prefix = account id
username = "personal"                     # any non-empty value
# password = the `token` above (or put it in your keyring)
```

First start syncs headers and flags for every folder — for a 20k-message mailbox
that's seconds, not minutes. Bodies arrive when you open them.

## Deployment

### Why HTTPS is required

The Gmail OAuth consent flow redirects the browser back to an **HTTPS endpoint on
this bridge** (loopback `http://localhost` is the only cleartext exception Google
accepts, and only for the dev/port-forward mode). So a real deployment means:

1. DNS record for e.g. `jmap.example.com` → your reverse proxy
2. TLS certificate (Let's Encrypt via Caddy, or cert-manager in Kubernetes)
3. Reverse proxy → `jmap-bridge:8080`
4. `base_url = "https://jmap.example.com"` and Google redirect URI
   `https://jmap.example.com/oauth/{account}/callback` registered on your OAuth client

jmap-tui's own URL policy mirrors this: cleartext `http://` is accepted only for
loopback hosts, so `https://` (or a local port-forward) is the only way to reach
the bridge from another machine.

### Caddy

```caddyfile
jmap.example.com {
    reverse_proxy jmap-bridge:8080
}
```

### nginx

```nginx
server {
    listen 443 ssl;
    server_name jmap.example.com;
    ssl_certificate     /etc/ssl/certs/jmap.pem;
    ssl_certificate_key /etc/ssl/private/jmap.key;

    client_max_body_size 64m;          # attachment uploads
    proxy_read_timeout   3600s;        # EventSource streams are long-lived

    location / {
        proxy_pass http://jmap-bridge:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_buffering off;            # SSE must stream
    }
}
```

### Kubernetes

A ready-made kustomize stack lives in [`deploy/k8s/`](deploy/k8s/): `Deployment`
+ `PVC` (SQLite + blobs) + `Service` + `Ingress` (TLS) + `ConfigMap`, with the
Secret created out of band. See [`deploy/README.md`](deploy/README.md) for the
image, GHCR and `kubectl apply -k` walkthrough.

Two ways in:

- **Direct:** Ingress with a real certificate and hostname → clients use
  `https://jmap.example.com/{account}`. This is also the OAuth callback target.
- **Dev/tunnel:** `kubectl port-forward svc/jmap-bridge 8080:80` → clients use
  `http://127.0.0.1:8080/{account}` (loopback, so cleartext is allowed).

The Deployment is a single replica on a `ReadWriteOnce` volume by design: the
cache is one SQLite writer (PLAN §10), so it must not be scaled horizontally.

Health endpoints for probes: `GET /healthz` (process liveness) and `GET /readyz`
(200 only once every account — IMAP or Gmail API — has completed a sync pass;
503 again if an account hits an authentication failure, e.g. a dead Gmail
refresh token).

### Google OAuth setup (Gmail accounts)

Every self-hoster creates their **own** Google Cloud project and OAuth client —
the bridge ships no shared client, and Google's OAuth verification is per-project.
Do this once; the settings in step 2 are what make it set-and-forget.

1. **Create a project and enable APIs.** In the Google Cloud Console create a
   project, then enable the **Gmail API** and the **CardDAV API** (contacts).
   CalDAV is only relevant once calendar support lands (roadmap, PLAN §15).

2. **Configure the OAuth consent screen.**
   - **User type:** *External* for personal `@gmail.com` accounts. Choose
     *Internal* instead if every account is on one Google Workspace domain —
     Internal skips verification, shows no warning screen, and is not subject to
     the 7-day limit below.
   - **Publishing status: set it to "In production", not "Testing".** This is the
     one setting people get wrong, and it decides whether the bridge is
     set-and-forget. An *External* consent screen left in **Testing** is issued
     refresh tokens that **expire after 7 days**. That is Google's rule, not the
     bridge's, and refreshing the access token does not extend it (see below).
   - **Scopes:** `https://mail.google.com/` (used for IMAP *and* SMTP) and
     `https://www.googleapis.com/auth/carddav` (contacts).

3. **Create the OAuth client:** Credentials → Create credentials → **OAuth client
   ID → Web application**, with authorized redirect URI
   `https://jmap.example.com/oauth/{account}/callback`.

4. **Configure the bridge** with the client ID/secret (environment or mounted file
   preferred over inline config), start it, open
   `https://jmap.example.com/oauth/{account}/start` in a browser, and approve.
   Refresh tokens are stored encrypted at rest.

5. **Publish the consent screen.** Set the **Application home page** to
   `https://jmap.example.com/` and the **Privacy policy** to
   `https://jmap.example.com/privacy` — the bridge serves both (FR-D.13) — and
   add the host to **Authorized domains**. Moving the publishing status to
   **In production** clears the 7-day Testing refresh-token expiry; personal use
   qualifies for the verification exemption, so no full review is needed.

#### The warnings you will see when consenting

Because your app is unverified, Google shows its "unverified app" interstitial on
every consent even in production. This is expected, and it is your own app:

- A screen reading **"Google hasn't verified this app"**. Click **Advanced**, then
  **Go to \<your app name\> (unsafe)**. The "(unsafe)" label just means Google has
  not reviewed the app; it does not mean the credentials are at risk.
- The scope list will say the app wants to **"Read, compose, send, and permanently
  delete all of your email"** — that is exactly what `https://mail.google.com/`
  grants, and the bridge needs all of it (faithful IMAP+SMTP proxying).
- An unverified app that requests Gmail's restricted scope is capped at **100
  users**. Personal self-hosting is one user; you will not hit it. Completing
  Google's verification process removes both the warning and the cap, but it is
  not required to run the bridge.

#### The 7-day trap (Testing vs. In production)

If you leave the External consent screen in **Testing**, the account will stop
syncing a week after you consented. There is no bridge-side fix:

- The refresh token is issued with a **fixed 7-day expiry** while the status is
  Testing. It is not an idle timeout — using it does **not** reset the clock.
- Google does **not** return a new refresh token when the bridge refreshes the
  access token, so "refresh early" cannot roll the expiry forward either.
- The remedy is the setting, not the bridge: switch publishing status to
  **In production** and consent once more (the warnings above apply). Internal
  user type avoids the limit entirely.

When a refresh token does lapse, the bridge marks the account auth-failed, `/readyz`
returns false, and the log names the account and says consent is needed — reopen
`/oauth/{account}/start` to re-consent. The same applies to the other ways Google
kills a token: password change, six months unused, admin policy, or manual revoke.

App passwords are deliberately **not** supported for Gmail: Google has withdrawn
basic authentication for third-party mail clients (Workspace enforced 2025-03-14)
and OAuth2 is the durable path.

### Development mode

```sh
# mail:   start the loopback Dovecot (needs sudo) — dev/dovecot.conf
# contacts: start the loopback Radicale CardDAV — dev/radicale.conf
bash dev/gate/radicale-start.sh
go run ./cmd/jmap-bridge --config ./dev/config.toml
```

`auth.mode = "none"` is accepted **only** when `listen` is a loopback address —
the bridge refuses to start otherwise. The dev config front-ends both rig
servers with throwaway loopback credentials; without Radicale running, the
contacts capability simply never appears (FR-P.3), and mail is unaffected.

## How the local cache works

| Layer | What | When |
|---|---|---|
| Metadata | folders, flags, keywords, UIDs, modseq, sizes, thread ids | always synced (headers-only FETCH) |
| Structure | `BODYSTRUCTURE` → JMAP `bodyStructure`, `hasAttachment`, preview | synced / 4 KB partial fetch |
| Bodies | full RFC 5322 → parsed parts, `bodyValues`, attachment blobs | first `Email/get` that asks for a body |
| Index | FTS5 over subject/from/to, plus body text as it hydrates | continuously |

Storage is a single SQLite database (WAL) plus a blob directory under `data_dir`.
Nothing is ever written by clients — the bridge is the only writer, and every
mutation is applied to the real IMAP server *first*, then committed locally.

Search is an FTS5 index over subject, sender and recipient (indexed the moment
headers arrive) plus body text as bodies hydrate. A `text` query answers
immediately from whatever is indexed: header matches return at once, and with
`search.backfill = true` the unhydrated messages in the query's scope (bounded
by `backfill_scan`) hydrate in the background, each finished body re-entering
the index and nudging query state so clients pick up late matches via
`Email/changes` or SSE. With `search.backfill = false` the bridge behaves
identically except that body-only matches are absent — the search degrades to
headers, it never errors. Interactive body reads (`Email/get`) never queue
behind search backfill: backfill workers share the same fetch connection but
an interactive request waits at most one in-flight download.

The body cache is grow-only in v0.1: a hydrated body stays until you delete it
from the data directory. It grows with mail you actually read (plus anything
inside `prefetch_window`, plus search-driven backfill when enabled), never
with the whole mailbox — plan disk for `data_dir` accordingly. Eviction is not
implemented yet; bodies are always re-fetchable from IMAP, so a safe policy
can be added later without data loss.

## Configuration reference

```toml
listen      = "0.0.0.0:8080"   # socket to bind
base_url    = "https://jmap.example.com"   # public origin (session + OAuth callback)
data_dir    = "/data"          # SQLite + blobs
log_level   = "info"

[auth]
mode = "token"                 # "token" (Basic account-id:token) | "none" (loopback only)

[search]
backfill      = true            # hydrate bodies in the background on text search
concurrency   = 4               # parallel body downloads
backfill_scan = 2000            # max unhydrated candidates one text search enqueues

[sync]
interval     = "5m"            # fallback poll when IDLE is unavailable
batch_size   = 500             # UIDs per FETCH during backfill
prefetch_window = "30d"        # bodies auto-hydrate for mail newer than this (0 = never)

[rate]                         # in-process abuse protection (NFR-5); see SECURITY-PLAN.md
enabled          = true
auth_failures    = 10          # failed client logins per window before a lockout
auth_window      = "5m"
auth_block       = "15m"
max_concurrent_requests = 8    # advertised and enforced
max_concurrent_uploads  = 4
max_eventsource_per_account = 8
max_eventsource_total       = 128
trusted_proxies  = []          # CIDRs whose X-Forwarded-For is believed
# client_ip_header = "CF-Connecting-IP"   # set behind Cloudflare (peer must be trusted)

[metrics]                      # opt-in Prometheus endpoint (FR-D.6)
enabled = false                 # GET /metrics (text/plain, version 0.0.4)

[[accounts]]
id      = "personal"           # path prefix and JMAP account identity
name    = "Personal"
address = "me@example.com"     # default identity / envelope sender
token   = "…"                  # client password (Basic auth), ≥24 chars:
                               #   openssl rand -hex 24
backend = "imap"               # "imap" (default) | "gmail_api" (see Gmail API mode)

  [accounts.imap]              # host, port, tls, auth = "password"|"oauth2",
                               # username, password
  [accounts.smtp]              # host, port, tls = "implicit"|"starttls"|"none",
                               # username, password (defaults to IMAP's)
  [accounts.carddav]           # optional: url (or leave empty for discovery)
  [accounts.oauth2]            # provider = "google"|"generic",
                               # client_id, client_secret, auth_url, token_url, scopes
  [accounts.gmail_api]         # backend = "gmail_api" only: watch, pubsub_topic,
                               # pubsub_audience, pubsub_service_account,
                               # quota_units_per_second, backfill_query/limit
```

Secrets may also be supplied as `JMAP_BRIDGE_<ACCOUNT>_<FIELD>` environment
variables or mounted files — never commit them, never log them.

`[metrics] enabled = true` serves `GET /metrics` on the same origin for
Prometheus scraping: per-account sync lag and tier, hydration queue depth,
JMAP method counts/errors, IMAP reconnects and EventSource clients. The endpoint
is unauthenticated (like `/healthz`) and names accounts, so enable it only where
the origin is not publicly reachable, or gate `/metrics` at the reverse proxy.

Stored OAuth2 tokens are encrypted at rest with a 32-byte key from the
`JMAP_BRIDGE_SECRET_KEY` environment variable (raw, base64, or hex). Without
the key the bridge starts in plaintext mode and logs a prominent warning —
fine for loopback experiments, not for a deployment with Gmail accounts.

## Gmail accounts (OAuth2)

Gmail is OAuth2-only (no app passwords). Create an OAuth client in Google
Cloud Console (type "Web application", redirect URI
`https://your-host/oauth/<account-id>/callback`), then configure:

```toml
[[accounts]]
id      = "gmail"
name    = "Gmail"
address = "me@gmail.com"
token   = "…"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"          # no password fallback: a configured password is a startup error
  username = "me@gmail.com"

  [accounts.smtp]
  host = "smtp.gmail.com"
  port = 465
  auth = "oauth2"

  [accounts.oauth2]
  provider = "google"      # endpoints and the mail scope are implied
  client_id = "…apps.googleusercontent.com"
  # client_secret = … (or JMAP_BRIDGE_GMAIL_OAUTH2_CLIENT_SECRET / _FILE)
```

Then consent once: open `https://your-host/oauth/gmail/start` in a browser,
approve, and the bridge stores the refresh token and starts syncing. With the
consent screen **In production** (see "Google OAuth setup" above) the refresh
token is durable and re-used indefinitely; if it is left in **Testing** it dies
after 7 days. Either way, when Google revokes or expires a token the bridge logs
that consent is needed again and the same URL restarts the flow.

Behind a Kubernetes ingress, the shipped Service sets
`publishNotReadyAddresses: true` so this URL is reachable while `/readyz` is
still `503` (no account has synced yet). Without it, an ingress that routes only
to Ready pods would 503 the very endpoint needed to make the account Ready.
Labels appear as mailboxes, archiving removes Inbox membership only, and
Gmail's thread grouping drives the client's threads.

### Gmail API mode

An account can instead be served by the Gmail REST API, which exposes Gmail's
real model (account-global message ids, threads, labels, history) rather than
IMAP's emulation of it. Add `backend = "gmail_api"` and a `[accounts.gmail_api]`
block; `[accounts.imap]` and `[accounts.smtp]` become forbidden, and the same
`[accounts.oauth2]` consent and stored token are reused (no re-consent):

```toml
[[accounts]]
id      = "gmail"
address = "me@gmail.com"
token   = "…"
backend = "gmail_api"

  [accounts.oauth2]
  provider = "google"
  client_id = "…apps.googleusercontent.com"

  [accounts.gmail_api]
  watch          = "pubsub"          # or "poll"; see below
  pubsub_topic   = "projects/acme-mail/topics/jmap-bridge-push"
  pubsub_audience = "https://jmap.example.com"
  pubsub_service_account = "jmap-push@acme-mail.iam.gserviceaccount.com"
  # backfill_query = "newer_than:30d"  # optional: bound a large cold start
  # backfill_limit = 5000
```

The bridge implements the read path (browse, threads, search, lazy hydration,
`/changes`, SSE), the write path (triage, label moves, archive, draft
create, `Mailbox/set`) and submission over the API (`drafts.send` for a
Gmail draft, otherwise `messages.send`; Gmail files its own Sent copy, so
the bridge never APPENDs a second one, and an ambiguous send is reconciled
by Message-ID before it is reported as failed). Semantics are documented
honestly: a synthetic `All Mail` archive with implicit membership, only
`$seen`/`$flagged`/`$draft`/`$important` keywords (no
`$answered`/`$deleted`/custom), `size` as Gmail's `sizeEstimate` until a body
is hydrated, and `onDestroyRemoveEmails=true` refused (a label never owns its
messages). `Email/import` imports raw RFC 5322 into any mailbox via Gmail's
`messages.import`; an imported message's received time follows its `Date`
header (`internalDateSource=dateHeader`), the closest the API allows to IMAP
`APPEND`'s `INTERNALDATE`.

Live change notification is one of two modes:

- **`watch = "pubsub"`** (the ≤ 2 s path, NFR-2): `users.watch` registers
  `pubsub_topic`, Pub/Sub pushes to `POST /gmail/push/{account}`, and a
  verified push runs a history pass. The bridge re-arms the watch before its
  expiration and calls `users.stop` on shutdown. It is the default only when
  `base_url` is `https` **and** `pubsub_topic` is set; otherwise the account
  falls back to poll.
- **`watch = "poll"`** (the documented degraded mode): no GCP setup, no public
  ingress needed; foreign changes appear within `sync.interval`. Use it for
  loopback/self-hosted bridges and when you do not want to run Pub/Sub.

#### Pub/Sub setup (for `watch = "pubsub"`)

Change notification needs a publicly reachable HTTPS bridge (`base_url` on its
public origin, already required for the OAuth callback). In the same Google
Cloud project as the OAuth client, with the Gmail API enabled:

1. **Create the topic** and let Gmail publish to it:

   ```sh
   gcloud pubsub topics create jmap-bridge-push
   gcloud pubsub topics add-iam-policy-binding jmap-bridge-push \
     --member="serviceAccount:gmail-api-push@system.gserviceaccount.com" \
     --role="roles/pubsub.publisher"
   ```

2. **Create a service account and an OIDC push subscription** targeting the
   bridge. The bridge verifies the push's OIDC token against the audience and
   the service account's email, so both must match the config below:

   ```sh
   gcloud iam service-accounts create jmap-push \
     --display-name="jmap-bridge Pub/Sub push"
   gcloud pubsub subscriptions create jmap-bridge-push-sub \
     --topic=jmap-bridge-push \
     --push-endpoint="https://jmap.example.com/gmail/push/gmail" \
     --push-auth-service-account="jmap-push@<project>.iam.gserviceaccount.com" \
     --push-auth-token-audience="https://jmap.example.com"
   ```

3. **Point the account at it** (`[accounts.gmail_api]` above), restart the
   bridge, and watch for `gmailapi: watch renewed` in the logs. `pubsub_topic`,
   `pubsub_audience` and `pubsub_service_account` are all required in `pubsub`
   mode; `pubsub_audience` must equal the subscription's audience and
   `pubsub_service_account` its push-auth service account. The endpoint fails
   closed: a missing or mismatched token is rejected `401` and runs no sync.

PLAN.md §17 is the full production runbook — it also covers the forged-push
and poll-fallback negative checks and how to observe a foreign change end to
end. `push_allow_plain = true` (loopback/fixture only) replaces OIDC with the
account's client token as a shared secret and must never be used on a public
origin.

#### Migrating an existing IMAP account to API mode

The change is configuration-only: set `backend = "gmail_api"`, remove the
`[accounts.imap]` and `[accounts.smtp]` blocks, and add `[accounts.gmail_api]`.
The `[accounts.oauth2]` block and the stored refresh token are reused, so no
re-consent is needed. The bridge re-discovers the account's labels and
re-syncs from the API; it does **not** migrate the cache left behind by IMAP
(the two backends address messages differently). For a clean switch, stop the
bridge and start from a fresh `data_dir` — note the OAuth token lives in that
same SQLite database, so a fresh directory means consenting once more. Keep the
old `data_dir` if you want to switch back, or copy it aside first.

## Development

```sh
go build ./...          # build
go vet ./...            # vet
gofumpt -l -w .         # format
golangci-lint run       # lint
go test ./... -race     # unit + fixture-server tests
```

There is no CI: these gates run locally before every commit (see `AGENTS.md`).
Live integration tests read credentials from `JMAP_BRIDGE_TEST_*` environment
variables and skip when unset.

## Documents

| File | Purpose |
|---|---|
| [`REQUIREMENTS.md`](REQUIREMENTS.md) | scope — numbered functional/non-functional requirements (source of truth) |
| [`PLAN.md`](PLAN.md) | architecture, data model, milestones, decisions, risks, roadmap |
| [`AGENTS.md`](AGENTS.md) | rules for AI agents (and humans) working in this repo |

## License

MIT — see [LICENSE](LICENSE).

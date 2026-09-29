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
| `urn:ietf:params:jmap:mail` (RFC 8621) | `Mailbox/get\|query\|changes\|set`, `Email/get\|query\|changes\|set`, `Thread/get`, `Identity/get` | v0.1 |
| `urn:ietf:params:jmap:submission` | `EmailSubmission/set` (create → SMTP) | v0.1 |
| `urn:ietf:params:jmap:contacts` (RFC 9610) | `AddressBook/get\|changes`, `ContactCard/get\|changes\|set` | v0.1 |
| push (RFC 8620 §7.3) | EventSource `/{account}/eventsource` (SSE) | v0.1 |
| roadmap | `Email/queryChanges`, `Email/copy`, `ContactCard/query`, `AddressBook/set`, `Identity/set`, `EmailSubmission/get\|query` | later |
| not planned | JMAP Calendar, `Principal`/sharing, `VacationResponse`, `MDN`, `Quota`, Sieve | see PLAN roadmap |

If a capability is not configured (no CardDAV URL → no contacts capability), the
session simply doesn't advertise it and clients degrade — jmap-tui hides contacts
rather than failing.

## Backends

| Backend | IMAP auth | Sync tier | Send | Contacts |
|---|---|---|---|---|
| **Gmail / Google Workspace** | OAuth2 (XOAUTH2) | CONDSTORE (Gmail has no QRESYNC) | SMTP + XOAUTH2 | CardDAV, OAuth2-only, **no contact groups** |
| **Dovecot** (self-hosted) | password | QRESYNC | SMTP | needs a separate CardDAV server |
| **cPanel mail** (Dovecot-based) | password | QRESYNC (detected) | SMTP | when the host offers CardDAV |
| **Namecheap Private Email** | password | detected at runtime | SMTP | when the host offers CardDAV |
| anything else | password or OAuth2 | detected at runtime | SMTP | any RFC 6352 server |

Contacts are optional: supply a CardDAV URL (or let the bridge discover it via
`/.well-known/carddav`, RFC 6764) and the contacts capability appears.

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

Published images arrive at milestone M7 (PLAN §12); until then build from source
(`go build -o jmap-bridge ./cmd/jmap-bridge`) and run the binary with
`--config ./config.toml` instead of the container.

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

`Deployment` + `PVC` (SQLite + blobs) + `Service` + `Ingress` (TLS). Two ways in:

- **Direct:** Ingress with a real certificate and hostname → clients use
  `https://jmap.example.com/{account}`. This is also the OAuth callback target.
- **Dev/tunnel:** `kubectl port-forward svc/jmap-bridge 8080:8080` → clients use
  `http://127.0.0.1:8080/{account}` (loopback, so cleartext is allowed).

Health endpoints for probes: `GET /healthz` (liveness), `GET /readyz` (all
configured accounts have completed at least one sync pass).

### Google OAuth setup (Gmail accounts)

1. Create a project in Google Cloud Console → **OAuth consent screen** (External,
   add yourself as a test user, or publish).
2. **Credentials → OAuth client ID → Web application**, with authorized redirect
   URI `https://jmap.example.com/oauth/{account}/callback`.
3. **Enable APIs**: Gmail API and **CardDAV API** (contacts). CalDAV API is only
   relevant once calendar support lands (roadmap, PLAN §15).
4. Scopes the bridge requests: `https://mail.google.com/` (IMAP *and* SMTP) and
   `https://www.googleapis.com/auth/carddav` (contacts).
5. Put `client_id`/`client_secret` in the account's `[accounts.oauth2]` block, start
   the bridge, open `https://jmap.example.com/oauth/{account}/start` in a browser,
   consent, done. Refresh tokens are stored encrypted at rest.

App passwords are deliberately **not** supported for Gmail: Google has withdrawn
basic authentication for third-party mail clients (Workspace enforced 2025-03-14)
and OAuth2 is the durable path.

### Development mode

```sh
go run ./cmd/jmap-bridge --config ./dev/config.toml
```

`auth.mode = "none"` is accepted **only** when `listen` is a loopback address —
the bridge refuses to start otherwise.

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

The body cache is grow-only in v0.1: a hydrated body stays until you delete it
from the data directory. It grows with mail you actually read (plus anything
inside `prefetch_window`), never with the whole mailbox — plan disk for
`data_dir` accordingly. Eviction is not implemented yet; bodies are always
re-fetchable from IMAP, so a safe policy can be added later without data loss.

## Configuration reference

```toml
listen      = "0.0.0.0:8080"   # socket to bind
base_url    = "https://jmap.example.com"   # public origin (session + OAuth callback)
data_dir    = "/data"          # SQLite + blobs
log_level   = "info"

[auth]
mode = "token"                 # "token" (Basic account-id:token) | "none" (loopback only)

[search]
backfill     = true            # hydrate bodies in the background on text search
concurrency  = 4               # parallel body downloads

[sync]
interval     = "5m"            # fallback poll when IDLE is unavailable
batch_size   = 500             # UIDs per FETCH during backfill
prefetch_window = "30d"        # bodies auto-hydrate for mail newer than this (0 = never)

[[accounts]]
id      = "personal"           # path prefix and JMAP account identity
name    = "Personal"
address = "me@example.com"     # default identity / envelope sender
token   = "…"                  # client password (Basic auth)

  [accounts.imap]              # host, port, tls, auth = "password"|"oauth2",
                               # username, password
  [accounts.smtp]              # host, port, tls = "implicit"|"starttls"|"none",
                               # username, password (defaults to IMAP's)
  [accounts.carddav]           # optional: url (or leave empty for discovery)
  [accounts.oauth2]            # provider = "google"|"generic",
                               # client_id, client_secret, auth_url, token_url, scopes
```

Secrets may also be supplied as `JMAP_BRIDGE_<ACCOUNT>_<FIELD>` environment
variables or mounted files — never commit them, never log them.

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

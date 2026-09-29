# PLAN — jmap-bridge

Architecture, data model, milestones, decisions, risks, roadmap.

**Doc hierarchy:** `REQUIREMENTS.md` = scope (source of truth for *what*);
this file = *how* and *when*. `README.md` = user-facing. If code and docs
disagree, flag it and fix one of them — don't let them drift.

Module: `github.com/CaffeinatedTech/jmap-bridge` · License: MIT · Language: Go ·
Config: TOML · Packaging: Docker/Kubernetes · CI: none (local gates, `AGENTS.md`).

---

## 1. Locked decisions

| # | Decision | Date |
|---|---|---|
| D-1 | Separate project at `~/projects/jmap-bridge`, deployed as a Docker container or in a Kubernetes cluster | 2026-09-28 |
| D-2 | **Hybrid local copy**: headers/flags/structure always synced, bodies lazy-fetched and cached on first open | 2026-09-28 |
| D-3 | Contacts (JMAP RFC 9610 ↔ provider CardDAV) are in v0.1; **JMAP Calendar is roadmap only** (user corrected scope) | 2026-09-28 |
| D-4 | Target is *any* IMAP server: Gmail (headline), cPanel, Dovecot, Namecheap Private Email → runtime capability detection, no server hard-coded | 2026-09-28 |
| D-5 | **Gmail auth is OAuth2-only** (XOAUTH2); app passwords deliberately unsupported | 2026-09-28 |
| D-6 | Freetext search over non-hydrated mail uses **search-driven backfill** (instant header matches + background hydration + SSE), not IMAP SEARCH merging, not full mirror | 2026-09-28 |
| D-7 | IMAP client: **`github.com/kiliant/go-imap`**, isolated behind our own driver interface in `internal/imapdrv` (**amended 2026-09-29**: was `emersion/go-imap/v2`, which has no QRESYNC/COMPRESS and no raw-command escape hatch — verified against v2.0.0-beta.8; kiliant/go-imap v1.1.0 ships tested client QRESYNC/CONDSTORE/COMPRESS, zero deps, frozen v1 API, interop-verified incl. Dovecot) | 2026-09-28, amended 2026-09-29 |
| D-8 | MIT, Go, TOML config | 2026-09-28 |
| D-9 | Module path `github.com/CaffeinatedTech/jmap-bridge` | 2026-09-28 |
| D-10 | **No CI.** All gates run locally before every commit | 2026-09-28 |
| D-11 | `REQUIREMENTS.md` at full FR/NFR rigor, traceable to milestones | 2026-09-28 |
| D-12 | Real deployments terminate TLS at a reverse proxy with DNS — required by the Gmail OAuth callback (deployment documentation, README §Deployment) | 2026-09-28 |
| D-13 | **Path-prefixed multi-account** on a single origin (`/{account}/…`), one container serves N accounts | 2026-09-28 |
| D-14 | **IMAP-first writes**: apply to the real server, commit locally only on success — the cache never diverges optimistically | 2026-09-28 |
| D-15 | Client-facing auth is a per-account bearer token over HTTP Basic (or `none` on loopback) | 2026-09-28 |
| D-16 | Request size caps are final: JSON ≤ 32 MiB, upload ≤ 64 MiB (REQUIREMENTS NFR-5) | 2026-09-28 |
| D-17 | `AddressBook/set` stays roadmap (v0.2+); v0.1 ships `AddressBook/get\|changes` only | 2026-09-28 |
| D-18 | Body cache is grow-only in v0.1; growth documented in README; eviction revisited only on demonstrated disk pressure | 2026-09-28 |
| D-19 | NFR-1 / NFR-8 numbers are binding as written (no M5 "pin"); M5 revalidates by measurement, revisions are REQUIREMENTS edits | 2026-09-28 |

---

## 2. Constraints that shape the design

### 2.1 The client contract (what jmap-tui actually exercises)

Measured from `jmap-tui/internal/jmapclient` on 2026-09-28:

- Session: `GET {url}/.well-known/jmap` → `apiUrl`, `uploadUrl`, `downloadUrl`,
  `eventSourceUrl`, `accounts`, `primaryAccounts`, `sessionState`; requires the
  `urn:ietf:params:jmap:mail` capability, gates `submission` and `contacts`.
- Methods: `Mailbox/get|query|changes`, `Email/get|query|changes|set`,
  `Thread/get`, `Identity/get`, `EmailSubmission/set`, `AddressBook/get|changes`,
  `ContactCard/get|changes|set`.
- Protocol details that must be honoured:
  - batched `POST /jmap` with **result references** (`#ids` → `Email/get`),
  - `Email/get` with `properties` + `bodyProperties` + `fetchAllBodyValues`,
  - `Email/query` with `anchor`/`anchorOffset`, `collapseThreads`, `calculateTotal`,
    sorts incl. `Mailbox/query` by `sortOrder`,
  - `Email/set` patches `keywords/<kw>` and `mailboxIds/<id>` using `true`/`null`
    (`null` removes — Fastmail rejects `false`),
  - `updated` in a set response may be an **array** (Stalwart) or an **object**
    (go-jmap's typed assumption) — the bridge sends the array form,
  - `EmailSubmission/set` with `onSuccessUpdateEmail` patches, response carries
    `undoStatus`, `sendAt`.
- URL/auth policy: cleartext `http://` accepted **only for loopback**; credentials
  attach to the configured origin and to advertised *https non-IP* origins only →
  serve session/API/upload/download/SSE from **one origin** (D-13).
- Degradations the client already implements: no `eventSourceUrl` → 60 s polling;
  no contacts capability → contacts hidden. Both are legitimate cut lines.

Not used by jmap-tui but expected of a "proper" JMAP server by other clients:
`Email/queryChanges`, `Email/copy`, `ContactCard/query`, `AddressBook/set`,
`Identity/set`, `EmailSubmission/get|query`. These are roadmap, not v0.1 gates.

### 2.2 Backend reality

| Backend | Notable capabilities | Consequences |
|---|---|---|
| Gmail | `UIDPLUS MOVE CONDSTORE ESEARCH SPECIAL-USE LIST-EXTENDED COMPRESS=DEFLATE UTF8=ACCEPT X-GM-EXT-1`, **no QRESYNC** | tier-2 sync; `X-GM-LABELS` ↔ mailbox membership; `X-GM-THRID` seeds threads; OAuth2 only; `[Gmail]/All Mail` archive semantics |
| Dovecot (self-hosted, cPanel) | full extension set incl. QRESYNC, UIDPLUS, MOVE, SPECIAL-USE | tier-1 sync; CONDSTORE/QRESYNC assumed but still detected |
| Namecheap Private Email, misc providers | unknown mix | detect at runtime; fall back to tier-3 baseline |
| CardDAV providers | RFC 6352; `sync-collection` (RFC 6578) common but not guaranteed | sync-token path + ctag/ETag diff fallback |
| Dovecot alone | **no CardDAV** | contacts capability simply not advertised |

Gmail has no CardDAV *groups* (Google does not map contact labels) and no
CalDAV-for-consumers worth depending on → documented gaps, not code paths.

---

## 3. Architecture

```text
cmd/jmap-bridge
 └─ internal/httpapi      HTTP: session, /jmap, upload, download, eventsource, oauth, health
     └─ internal/jmapapi  method dispatch, result references, /changes, state strings
         ├─ internal/store      SQLite (metadata, content, FTS5, threads, contacts) + blobs
         ├─ internal/sync       per-account sync engine (tiers, IDLE, hydration, backfill)
         │   └─ internal/imapdrv  kiliant/go-imap behind the driver seam (QRESYNC, X-GM-*, PREVIEW, COMPRESS)
         ├─ internal/submit     SMTP submission (password / XOAUTH2) + Sent APPEND
         ├─ internal/dav        CardDAV client (discovery, sync-collection, PUT/DELETE)
         ├─ internal/convert    RFC 5322 ↔ JMAP Email · vCard ↔ JSContact
         └─ internal/auth       client tokens, OAuth2 bootstrap + refresh, credential encryption
             └─ internal/config   TOML + env, strict validation
```

**Process model (v0.1):** one binary, one goroutine group per configured account
(sync loop, hydration workers, IDLE connection), one SQLite database in WAL mode
with a single writer. `context.Context` on every network call. No global mutable
state, no `init()` side effects.

**Request flow (read):**
`POST /jmap` → auth → per-method handler → SQLite query (index-covered) →
assemble JMAP response (properties filtered) → JSON.

**Request flow (write):** see §7.

---

## 4. Data model (SQLite, WAL)

Schema follows the [jmap.io server guide](https://jmap.io/server/) — split so each
common query is one index range scan.

```sql
CREATE TABLE mailboxes (            -- JMAP Mailbox ⇄ IMAP folder
  id TEXT PRIMARY KEY,              -- opaque JMAP id (stable across uidvalidity)
  account TEXT NOT NULL,
  parent_id TEXT, role TEXT,        -- \Inbox \Sent \Drafts \Trash \Junk \Archive via SPECIAL-USE
  name TEXT NOT NULL,               -- IMAP path (hierarchical, separator kept)
  sort_order INTEGER NOT NULL DEFAULT 0,
  uidvalidity INTEGER, uidnext INTEGER, highestmodseq INTEGER,
  total_emails INTEGER NOT NULL DEFAULT 0, unread_emails INTEGER NOT NULL DEFAULT 0,
  total_threads INTEGER NOT NULL DEFAULT 0, unread_threads INTEGER NOT NULL DEFAULT 0,
  may_read_items INTEGER NOT NULL DEFAULT 1, may_add_items INTEGER NOT NULL DEFAULT 1,
  may_remove_items INTEGER NOT NULL DEFAULT 1, may_create_child INTEGER NOT NULL DEFAULT 1,
  may_rename INTEGER NOT NULL DEFAULT 1, may_delete INTEGER NOT NULL DEFAULT 1,
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL,
  updated_not_counts_modseq INTEGER NOT NULL DEFAULT 0,
  deleted INTEGER                   -- deletion modseq (tombstone for /changes)
);
CREATE INDEX mailboxes_changes ON mailboxes(account, updated_modseq);
CREATE INDEX mailboxes_parent ON mailboxes(account, parent_id, deleted);

CREATE TABLE emails (               -- JMAP Email metadata (narrow, hot)
  id TEXT PRIMARY KEY,
  account TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  keywords TEXT NOT NULL,           -- JSON: keyword → true
  received_at INTEGER NOT NULL,     -- unix micros
  size INTEGER NOT NULL,
  has_attachment INTEGER NOT NULL,
  preview TEXT NOT NULL DEFAULT '',
  created_modseq INTEGER NOT NULL,
  updated_modseq INTEGER NOT NULL,
  deleted INTEGER                   -- deletion modseq (tombstone for /changes)
);
CREATE INDEX emails_changes ON emails(account, updated_modseq);
CREATE INDEX emails_thread ON emails(account, thread_id, received_at);
CREATE INDEX emails_recv ON emails(account, received_at);
CREATE INDEX emails_live ON emails(account, deleted, received_at);

CREATE TABLE email_mailbox (        -- MailboxEmailList: membership + history
  account TEXT NOT NULL,
  mailbox_uid INTEGER NOT NULL,     -- mailboxes.rowid: compact per-mailbox key
  email_id TEXT NOT NULL,
  removed_modseq INTEGER NOT NULL DEFAULT 0,
  added_modseq INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX email_mailbox_live ON email_mailbox(mailbox_uid, email_id)
  WHERE removed_modseq = 0;
CREATE INDEX email_mailbox_email ON email_mailbox(email_id);

CREATE TABLE email_content (        -- parsed at backfill; bodies at hydration
  id TEXT PRIMARY KEY,
  headers TEXT NOT NULL,            -- JSON subset (from/to/cc/bcc/replyTo/subject/…)
  message_ids TEXT, in_reply_to TEXT, "references" TEXT,  -- JSON arrays
  sent_at INTEGER,
  subject_l TEXT NOT NULL DEFAULT '',  -- lowercase mirrors for query filters/sorts
  from_l TEXT NOT NULL DEFAULT '',     -- (M5's FTS5 supersedes them for text)
  to_l TEXT NOT NULL DEFAULT '',
  body_structure TEXT NOT NULL DEFAULT '{}',  -- JSON EmailBodyPart tree
  body_values TEXT NOT NULL DEFAULT '{}',     -- partId → {value, isTruncated}
  hydrated_at INTEGER                -- null until the body was fetched
);

CREATE VIRTUAL TABLE email_fts USING fts5(
  email_id UNINDEXED, subject, people, body,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE threads (              -- thread key → thread id (FR-M.7)
  account TEXT NOT NULL,
  thread_key TEXT NOT NULL,         -- 'm:<sha1(Message-ID)>' | 's:<sha1(base subject)>'
  thread_id TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  PRIMARY KEY (account, thread_key)
);
CREATE INDEX threads_by_id ON threads(account, thread_id);

CREATE TABLE imap_uids (            -- (folder, uidvalidity, uid) → email id
  account TEXT NOT NULL, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL,
  uid INTEGER NOT NULL, email_id TEXT NOT NULL,
  PRIMARY KEY (account, folder, uidvalidity, uid)
);
CREATE INDEX imap_uids_email ON imap_uids(email_id);

CREATE TABLE addressbooks (         -- JMAP AddressBook ⇄ CardDAV collection
  id TEXT PRIMARY KEY, account TEXT, href TEXT NOT NULL,
  name TEXT, description TEXT, sort_order INTEGER,
  may_read INTEGER NOT NULL DEFAULT 1, may_write INTEGER NOT NULL DEFAULT 1,
  may_share INTEGER NOT NULL DEFAULT 1, may_delete INTEGER NOT NULL DEFAULT 1,
  sync_token TEXT, ctag TEXT, etag TEXT,
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL, deleted INTEGER
);
CREATE TABLE cards (                -- JSContact ⇄ vCard
  id TEXT PRIMARY KEY,              -- = vCard UID (RFC 9610 §3)
  account TEXT, book_id TEXT NOT NULL,
  uid TEXT NOT NULL, kind TEXT, name TEXT,   -- denormalised for search/sort
  jscontact TEXT NOT NULL,          -- full JSContact JSON
  vcard TEXT NOT NULL,              -- last known wire form
  href TEXT NOT NULL, etag TEXT,    -- CardDAV addressing
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL, deleted INTEGER,
  UNIQUE (account, uid)
);

CREATE TABLE blobs (                -- blobId → relative file path, media type, size
  id TEXT PRIMARY KEY, account TEXT, path TEXT NOT NULL,
  media_type TEXT, size INTEGER, created_at INTEGER
);

CREATE TABLE sync_state (           -- per account/folder sync bookkeeping
  account TEXT NOT NULL, scope TEXT NOT NULL,   -- 'folder:<name>' | 'contacts' | 'account'
  highestmodseq INTEGER NOT NULL DEFAULT 0, sync_token TEXT,
  last_full_scan INTEGER,
  PRIMARY KEY (account, scope)
);

CREATE TABLE counters (             -- process-global monotonic allocators
  name TEXT PRIMARY KEY,            -- 'seq': change counter, feeds id minting
  value INTEGER NOT NULL
);

CREATE TABLE email_msgid (          -- a message's own Message-ID → its email id
  account TEXT NOT NULL, msgid TEXT NOT NULL, email_id TEXT NOT NULL,
  PRIMARY KEY (account, msgid)
);

CREATE TABLE tokens (               -- client-facing auth tokens
  account TEXT PRIMARY KEY, token_hash TEXT NOT NULL
);

```

**Bookkeeping conventions** (M1, §4.1 companions):

- `counters` holds one process-global `seq` that every mutation takes (and
  that id minting embeds — global so ids cannot collide across accounts).
- `sync_state` row `scope='account'` holds the account's JSON state floors
  (`emailState`, `mailboxState`, `purgedThrough`): every mutation raises the
  relevant floor to its `seq`, which keeps type states monotonic even after
  tombstone retention expires, and `purgedThrough` marks the oldest change
  `/changes` can no longer replay. `sync_state` `scope='folder:<name>'` holds
  per-folder sync bookkeeping as JSON
  (`{"uidvalidity":…,"uidnext":…,"highestmodseq":…,"backfill_uid":…}`).
- Type state strings are the floors above; `Thread` state is the `Email`
  state (any email change can change threads) and `queryState`'s counter is
  the `Email` state (PLAN §4.1).
- `email_content.subject_l/from_l/to_l` are lowercase query mirrors of the
  header subset — M1 filters and sorts on them with `LIKE`; M5's FTS5 index
  supersedes them for `text` search (FR-X).

**Blobs** live in `{data_dir}/blobs/{aa}/{blobId}` (raw messages, attachments,
contact photos). SQLite holds metadata only; bodies are never inside rows.

### 4.1 Identifiers and state strings

- JMAP ids are opaque, immutable, unique per account and **independent of IMAP
  UIDs** — they must survive `UIDVALIDITY` changes. Form: 13-char
  base62 of the global `seq` counter (time-prefixed, per jmap.io guidance, so id
  order approximates date order for `Email/query` fast paths).
- `UIDVALIDITY` change → re-point `imap_uids`, mint **new** ids for survivors,
  emit destroys/creates through `/changes` (clients see a normal change set).
- `state` for `Email`/`Mailbox`/`Thread`/`AddressBook`/`ContactCard` =
  `"<typeModseq>"` (decimal). Equality short-circuit is all clients may assume.
- `queryState` = `"<queryCounter>:<sha1(args)>"`, bumped on any change that could
  affect the result (any Email/membership modseq bump is enough for v0.1).

---

## 5. Sync engine

Per account: `discover → select tier → incremental loop`.

**Discovery** (once per account): `CAPABILITY`, `LIST "" ""` with
`LIST-EXTENDED` + `SPECIAL-USE` (roles) → folders; `NAMESPACE` (prefix/separator);
`ENABLE UTF8=ACCEPT` where offered; `COMPRESS=DEFLATE` where offered.

**Tiers** (highest supported wins):

| Tier | Trigger | Incremental | Expunge detection |
|---|---|---|---|
| 1 QRESYNC | `QRESYNC` in CAPABILITY | `SELECT (QRESYNC (UIDVALIDITY MODSEQ …))` → `FETCH … (CHANGEDSINCE)` + `VANISHED` | exact (`VANISHED`) |
| 2 CONDSTORE | `CONDSTORE` (Gmail) | `SELECT (CONDSTORE)` → new UIDs by `UIDNEXT` scan + `UID FETCH (CHANGEDSINCE)` for flags | UID gaps → rescan folder |
| 3 Baseline | always | periodic `UID FETCH * (UID)` diff + full `FLAGS` refetch when changed | missing UID → rescan folder |

**Backfill (initial)**: per folder, `UID FETCH` in batches of `sync.batch_size`
with `UID, FLAGS, INTERNALDATE, RFC822.SIZE, ENVELOPE, BODYSTRUCTURE` — headers
only, never bodies. `BODYSTRUCTURE` gives `hasAttachment` and the preview target.

**Live**: `IDLE` on INBOX + watched folders; on notification run the tier's
incremental pass for that folder. Fallback: `sync.interval` poll. IDLE
disconnections back off exponentially.

**Preview**: if `PREVIEW` (RFC 8970) is advertised use it; otherwise a partial
`BODY[]<0.4096>` per message on first summary request, cached in `emails.preview`.

**Body hydration** (lazy, D-2): `Email/get` requesting body properties with
`hydrated_at IS NULL` → `UID FETCH BODY.PEEK[]` → `convert.RFC5322ToEmail` →
store `email_content`, split attachments into blobs, insert FTS body tokens,
set `hydrated_at`, bump modseq *only if* derived summary fields changed
(preview usually does → a small `/changes` is correct and desirable).
`sync.prefetch_window` optionally hydrates recent mail in the background.

**Rate discipline**: batched FETCH, `COMPRESS=DEFLATE` when available, bounded
hydration workers (`search.concurrency`), jittered backoff, never a tight loop
against a provider.

---

## 6. Search (D-6)

```
Email/query with a text/filter component
 ├─ fast path: FTS5 over subject/people (+ body tokens already indexed)
 │             → respond immediately, queryState as of now
 └─ if backfill enabled and unhydrated candidates exist:
       enqueue hydration jobs (bounded) → each batch parses, indexes FTS,
       bumps query state, emits an SSE StateChange → client re-queries via /changes
```

- Filters supported in v0.1 (what jmap-tui sends): `inMailbox`, `text`, `from`,
  `to`, `subject`, `after`, `before`, `hasKeyword`, `hasAttachment`; sorts:
  `receivedAt`, `subject`, `from`, `size`, `hasAttachment`, plus `Mailbox/query`
  `sortOrder`; `collapseThreads`, `position/limit/anchor/anchorOffset`,
  `calculateTotal`.
- `search.backfill = false` degrades to header-only matches (documented).

---

## 7. Write, send, and blob paths

### 7.1 Email/set (IMAP-first, D-14)

| JMAP op | IMAP op |
|---|---|
| update `keywords/$seen` | `UID STORE … (\Seen)` — `$seen↔\Seen`, `$draft↔\Draft`, `$flagged↔\Flagged`, `$answered↔\Answered`, `$deleted↔\Deleted`, `$labelN`/custom keywords ↔ IMAP keywords verbatim |
| add `mailboxIds/<id>` | `UID COPY` (or `UID MOVE` where the semantics are move) |
| remove `mailboxIds/<id>` | `UID MOVE` to target, or `COPY` + `STORE \Deleted` + `UID EXPUNGE` (UIDPLUS) fallback |
| destroy | `STORE \Deleted` + `UID EXPUNGE` (UIDPLUS) in **every** folder holding the message — destroy is permanent (RFC 8621 §4.6); a client files to Trash with a `mailboxIds` patch instead |
| create (draft) | `APPEND` to Drafts with `\Draft \Seen` + `$draft` |
| Gmail label change | `UID STORE +X-GM-LABELS (…)` where supported (avoids copy storms) |

Local commit + modseq bump + SSE **only after** the IMAP command returns OK; on
failure the JMAP response carries `notUpdated`/`notDestroyed` with `serverFail`.
Patch values follow RFC 8620 §5.3 — `true` adds, `null` removes — and `false`
also removes (Stalwart accepts it, Fastmail rejects it; accepting both never
breaks a client while the canonical form works everywhere). A patch that would
leave an Email in no Mailbox is refused with `invalidProperties`, since the mail
store requires at least one membership (RFC 8621 §4.1).

`Mailbox/set`: `CREATE` / `RENAME` / `DELETE` (+ role detection refresh), with
`alreadyExists`, `mailboxHasChild` and `mailboxHasEmail` (unless
`onDestroyRemoveEmails`) reported as the RFC 8621 §2.5 SetErrors.

### 7.2 EmailSubmission/set

1. Validate every create and every `onSuccessUpdateEmail` patch first: SMTP
   cannot be undone, so a malformed argument must fail before anything is
   sent. `emailId` may be a plain id or the `#handle` creation reference of
   an `Email/set` create in the same batch (RFC 8620 §3.7), `sendAt` is
   refused (maxDelayedSend is 0) and `onSuccessDestroyEmail` is refused
   (client-side fallback exists in jmap-tui).
2. SMTP submit: `MAIL FROM` = identity address, `RCPT TO` = the envelope
   (or To+Cc+Bcc when the client supplied none), `AUTH PLAIN` over TLS or
   loopback (XOAUTH2 arrives with M4), TLS per config, and **Bcc stripped
   from the bytes** — the
   envelope still carries those recipients (RFC 8621 §7.5), the Sent copy
   keeps its Bcc header.
3. On 2xx: file the sent message, in exactly one of two shapes. The
   caller's `onSuccessUpdateEmail` files it when it moves the Email into a
   mailbox — that is what every composing client sends (Drafts → Sent,
   `$draft` cleared) and the move *is* the filing. Only when the patch says
   nothing about membership does the bridge `APPEND` the bytes it just sent
   to the Sent mailbox with `\Seen`, because a sent message with no record
   anywhere is worse than one the server filed. Appending *and* patching
   would leave two copies in Sent (jmap-tui's own gate asserts one). Return
   `created` with `undoStatus: "final"` (send-delay is client-side in
   jmap-tui; no `EmailSubmission/undo` endpoint in v0.1). Everything after
   SMTP acceptance is logged, never reported as a failed create — a client
   that retries a "failed" send sends the message twice.
4. The `onSuccess*` effects run as one implicit `Email/set` after every
   create has been processed, and its response follows the
   `EmailSubmission/set` response (RFC 8621 §7.5); a patch that fails lands
   in that response's `notUpdated`.
5. On SMTP failure: `notCreated` with the SMTP reply as description. Never
   APPEND to Sent for a message that wasn't accepted.

### 7.3 Blobs

- `POST /{account}/upload/` → size/type caps → `blobs` row + file →
  `blobId`. The session's `{accountId}` expands into the account path
  prefix (D-13), so one template serves every account on the origin.
- `GET /{account}/download/{blobId}/{name}?type={type}` — RFC 8620
  template expansion exactly as jmap-tui performs it; `{name}` becomes the
  `Content-Disposition` filename and `{type}` the response's media type.
- Draft attachments reference `blobId`s; the MIME part is materialised at
  APPEND, and the bytes of a message the bridge built or fetched are kept
  as its raw blob so `Email/get` can answer `blobId` and a submission
  resends exactly what was stored.

---

## 8. Contacts (M6)

**Discovery**: `/.well-known/carddav` (RFC 6764) → principal (RFC 5397) →
`addressbook-home-set` → `PROPFIND` collections (displayname, sort order,
`getctag`, `sync-token`, privileges).

**Sync**: `REPORT sync-collection` (RFC 6578) per book with the stored
`sync-token`; fallback = `getctag` change → full list diff by ETag; fetch changed
cards with `addressbook-multiget`, then `GET` for new hrefs.

**Writes**: create = `PUT` with `If-None-Match: *`; update = `PUT` with
`If-Match: <etag>` (412 → refetch and retry once, then `overwritten`/`serverFail`);
destroy = `DELETE` with `If-Match`. Address book destruction of a non-empty book →
`addressBookHasContents` (RFC 9610 §7.4.1).

**Conversion** (`internal/convert`): vCard 3.0 (RFC 2426) *and* 4.0 (RFC 6350) ⇄
JSContact (RFC 9553) guided by RFC 9554 (vCard Format Extensions for JSContact),
so no semantics are silently dropped. Golden-pair fixtures are the test corpus.
Known limits, documented not hacked around: one address book per card (CardDAV
cannot express JMAP multi-book membership), Google has no groups, photos capped
to recognised image types with a size limit.

**Capability gating**: `urn:ietf:params:jmap:contacts` appears in the session
only when an account has CardDAV configured **and** the first sync succeeded.

**State strings**: `AddressBook` state = hash of book sync-tokens; `ContactCard`
state = per-account modseq bumped whenever any card changes (including via
sync-collection results).

---

## 9. Authentication and credentials

**Client-facing** (D-15): HTTP Basic, username ignored (any non-empty value),
password = the account `token`; compared constant-time against a stored hash.
`auth.mode = "none"` is accepted only when `listen` is loopback — the process
refuses to start otherwise.

**Backend-facing**:
- `password`: used for IMAP `LOGIN`/SASL PLAIN and SMTP `AUTH LOGIN`.
- `oauth2` (D-5): authorization-code + PKCE (S256) via
  `GET /oauth/{account}/start` → provider consent → `GET /oauth/{account}/callback`
  (served by the bridge, reachable through the TLS reverse proxy — README
  §Deployment). Refresh tokens persisted encrypted; refresh on expiry and on
  `AUTHENTICATE` failure with one retry; XOAUTH2 on IMAP and SMTP.
  `provider = "google"` fills in endpoints/scopes; `provider = "generic"` takes
  them from config (future: Microsoft).

**Encryption at rest**: credentials and refresh tokens AES-256-GCM with a key
from `JMAP_BRIDGE_SECRET_KEY` (env/K8s Secret, never in the data volume). Without
a key the bridge starts in plaintext mode with a loud warning (matching
jmap-perl's behaviour, minus the silent default).

**Never**: log or `fmt`-print passwords, tokens, Authorization headers, OAuth
codes; commit `.env*` or secrets; echo test credentials.

---

## 10. Concurrency and consistency

- Single writer per SQLite database (WAL, `busy_timeout`, one write path).
- Sync loop and HTTP handlers share the DB; cross-writes are transactions that
  bump modseq **and** queue the SSE notification atomically.
- Mutations are serialised per account (a small queue) so a client's read-your-
  own-writes holds: response state strings reflect the write that just happened.
- Gmail's eventual consistency: after our own IMAP write, absent results on the
  next fetch are not treated as deletions (grace window before tombstoning).
- SSE delivery is best-effort; clients always recover via `/changes` (jmap-tui
  has a 60 s poll fallback), so a dropped stream is a latency event, not data loss.

---

## 11. Observability and operations

- Structured logs (`log/slog`): level via config, request ids, per-account sync
  stats (folder counts, lag, tier, last pass). Never bodies, never credentials.
- `GET /healthz` (process alive), `GET /readyz` (every account completed a pass;
  no account stuck in auth failure).
- Metrics endpoint (optional, text format): sync lag seconds, hydration queue
  depth, JMAP method counts/errors, IMAP reconnects.
- Graceful shutdown: stop accepting HTTP, cancel IDLE, checkpoint SQLite.
- Data volume layout: `{data_dir}/config-ref`, `bridge.db`, `bridge.db-wal`,
  `blobs/`. Schema migrations via `schema_version` (forward-only in v0.1).

---

## 12. Milestones

Each milestone ends with a hard gate that must be demonstrated before the next
begins. FR references point at `REQUIREMENTS.md`.

**Status is written here and nowhere else** (REQUIREMENTS §10 maps FRs →
milestones but deliberately carries no completion state). Rules:

- `pending` → `landed` when the deliverable's code and docs merge.
- `landed` → `✅ done` **only** when the gate column has actually been
  demonstrated, recorded as `✅ done YYYY-MM-DD (<how>)`. Never mark done on
  the strength of code that looks right.
- Status changes land in the same commit as the gate demonstration (AGENTS.md).

| # | Deliverable | Gate | FRs | Status |
|---|---|---|---|---|
| **M0** | Repo skeleton, TOML config + validation, HTTP session + `POST /jmap` dispatch (derived from jmap-tui's `test/mockjmap`), fixture store, Dockerfile, gates green | jmap-tui connects to `http://127.0.0.1:PORT/{account}` and browses fixture mail | FR-J.1–.6, FR-A.1–.4 (A.4 config-level; live login lands M1/M3), FR-A.11–.12, FR-D.1 | ✅ done 2026-09-29 (jmap-tui live suite over loopback) |
| **M1** | SQLite store (schema §4), read-only IMAP sync (discovery, tier detection, backfill, IDLE), hydration, `/changes` + SSE, preview | real Dovecot account browsable read-only; a flag flipped in another IMAP client appears in jmap-tui ≤ 2 s | FR-S.1–.9, FR-M.1–.8, FR-J.7–.8 | ✅ done 2026-09-29 (live local Dovecot 2.4: `test/live` foreign flag → store 505 ms and jmap-tui engine 563 ms, both ≤ 2 s over IDLE+SSE; backfill, hydration and preview against the real server; jmap-tui live suite + `smoke` green over loopback) |
| **M2** | Write path §7.1, `Mailbox/set`, drafts, IMAP-first commits | jmap-tui triage (star/archive/move/delete/undo) round-trips; changes visible from a second IMAP client | FR-M.9–.13 | ✅ done 2026-09-29 (live local Dovecot 2.4: `test/live` write gate — star/move/copy with undo, `Mailbox/set` create/rename/delete, draft `APPEND` with `\Draft`, destroy — each re-read from an **independent IMAP session**; jmap-tui triage scratch green over loopback 3× — read/star/undo/move/undo/copy/undo/archive/delete-to-trash/destroy, every step confirmed by python3/imaplib; gate caught and fixed `/get` dropping `id` per RFC 8620 §5.1) |
| **M3** | Send §7.2, `Identity/get`, blob upload/download, `EmailSubmission/set` with `onSuccessUpdateEmail` | compose → send → message in Sent **and** delivered to a test sink; attachment round-trip byte-exact | FR-M.14–.17 | pending |
| **M4** | Gmail profile: OAuth2 bootstrap, XOAUTH2 IMAP/SMTP, `X-GM-LABELS`↔mailboxes, All Mail/archive, `X-GM-THRID`, CONDSTORE tier validation, rate limits | live Gmail: folders+labels both ways, compose/send, archive from jmap-tui, no rate-limit warnings | FR-A.5–.10, FR-S.10, FR-S.12, FR-M.18 | pending |
| **M5** | FTS5 + search-driven backfill, filter/sort/anchor/collapseThreads correctness, `PREVIEW`/partial-fetch, COMPRESS, 100k soak | jmap-tui live search cases green; cold browse of a 100k mailbox stays responsive; soak within NFR bounds | FR-X.1–.8, FR-S.11, NFR-1, NFR-2, NFR-8 | pending |
| **M6** | CardDAV §8: discovery, sync-collection, PUT/DELETE, vCard↔JSContact, photo blobs, capability gating | jmap-tui `contacts_live_test` suite green against a real CardDAV server; create/edit/delete contact round-trips | FR-P.1–.13 | pending |
| **M7** | Packaging: multi-account paths, credential encryption, metrics/health, UIDVALIDITY recovery drill, Docker image + k8s manifests, README deployment verified, `JMAP-TestSuite` run | fresh `docker run` + `kubectl` install works end-to-end from the docs; conformance suite passing; all gates green | FR-J.9–.10, FR-D.2–.12, NFR-3–.7, NFR-9–.10 | pending |

**Verification assets**: jmap-tui's live integration suite pointed at the bridge
(`JMAP_BRIDGE_TEST_*`); its `mockjmap`-derived fixtures seeded M0; Fastmail's
`JMAP-TestSuite` (Perl) as the external conformance gate at M7.

**Known conformance gaps for that M7 run** (deliberate, and rejected rather
than faked): `Mailbox/set` accepts only `name`/`parentId` — a client-set
`sortOrder`, `role` or `isSubscribed` is refused with `invalidProperties`,
because role and order are re-derived from the server on every discovery pass
and a value we cannot keep must not be acknowledged; `/get` ignores property
names it does not model instead of answering `invalidArguments`; and Email/get
has no top-level `blobId` yet (M3's blob endpoints).

---

## 13. Risk register

| Risk | Impact | Mitigation |
|---|---|---|
| `kiliant/go-imap` is new (first release 2026-08, single maintainer, low adoption) | a client bug stalls sync | driver interface isolates it (types never leave `internal/imapdrv`); zero-dependency + frozen v1 API means vendoring is painless if upstream goes quiet; its parser fuzzing and Dovecot interop matrix de-risk the gate; tier-3 fallback always exists. `X-GM-LABELS` (M4) is requestable today via its open-ended FETCH item types. **All three tiers landed and fixture-verified 2026-09-29** (QRESYNC anchor replay incl. VANISHED, CONDSTORE CHANGEDSINCE, baseline flag refetch), **and the QRESYNC tier ran against live Dovecot 2.4 the same day** (tier=qresync, COMPRESS=DEFLATE active) |
| Gmail eventual consistency after writes | false expunge / duplicate work | grace window before tombstoning own writes (§10) |
| No Go library for vCard↔JSContact | conversion bugs, data loss | golden-pair fixtures; RFC 9554 as the normative map; Stalwart `calcard` as cross-check reference |
| Provider extension roulette (Namecheap/cPanel) | sync failures | tier detection + capability probing at connect; strict-but-tolerant parsing; live tests per provider as they're added |
| OAuth callback requires public HTTPS | deploy friction | documented as a first-class deployment path (D-12); port-forward dev mode for everything else |
| Lazy bodies vs. freetext search expectations | "search misses mail" | backfill is on by default, progress is visible via SSE, `search.backfill=false` documented as a trade |
| SQLite hot rows (large mailbox counts) | slow list queries | narrow `emails` table + `email_mailbox` covering index (schema §4); count fields maintained incrementally |
| SMTP submission is inline: a send that is accepted can no longer be rolled back, and a failure *after* acceptance (filing the Sent copy, applying a patch) cannot be reported without inviting a duplicate send | a sent message with a stale local view | id minted before the send; post-acceptance failures are logged only, and the `onSuccess*` effects run after acceptance in the implicit `Email/set` (§7.2) where a patch failure is visible as `notUpdated` |
| Scope creep toward calendars/sharing | v0.1 slips | REQUIREMENTS out-of-scope list; roadmap (§15) is where those requests land |
| FR-D.1 image never actually built: the dev machine has no docker-daemon access (sudo needs a password) | container problems surface late, at M7 | native binary is the documented dev/test path (AGENTS.md); `docker build` + the README `docker run` are verified on a docker-capable host before M7 sign-off |

---

## 14. Spec register

Normative for v0.1 (verify status before relying on a draft):

| Spec | Subject | Status |
|---|---|---|
| RFC 8620 | JMAP Core (session, batch, `/changes`, blobs, push) | RFC |
| RFC 8621 | JMAP Mail (Email, Mailbox, Thread, Identity, EmailSubmission) | RFC |
| RFC 9610 | JMAP Contacts (AddressBook, ContactCard) | RFC |
| RFC 9553 / 9554 | JSContact; vCard Format Extensions for JSContact | RFC |
| RFC 8474 | OBJECTID (stable object ids) | RFC (adopted for id guidance) |
| RFC 3501 / 9051 | IMAP4rev1 / rev2 | RFC |
| RFC 2177 | IMAP IDLE | RFC |
| RFC 7162 (obsoletes 4551) | CONDSTORE + QRESYNC | RFC |
| RFC 4315 / 6851 / 6154 / 5258 / 5819 / 4731 / 5161 / 4978 / 7888 / 6855 / 7889 | UIDPLUS, MOVE, SPECIAL-USE, LIST-EXTENDED, LIST-STATUS, ESEARCH, ENABLE, COMPRESS=DEFLATE, LITERAL-, UTF8=ACCEPT, APPENDLIMIT | RFC |
| RFC 8970 | IMAP PREVIEW | RFC |
| RFC 5321 / 5322 / 6409 / 4954 / 3207 | SMTP, message format, submission port, SMTP AUTH, STARTTLS | RFC |
| RFC 4918 / 6352 / 6578 / 6764 / 5397 | WebDAV, CardDAV, Sync Collection, service discovery, DAV:current-user-principal | RFC |
| RFC 2426 / 6350 | vCard 3.0 / 4.0 | RFC |
| Google | IMAP extensions, XOAUTH2, CalDAV/CardDAV OAuth transition (2025-03-14) | vendor docs |
| draft-ietf-jmap-calendars-29 | JMAP Calendars | **draft — roadmap only** |
| draft-ietf-calext-jscalendar-icalendar-28 | JSCalendar ⇄ iCalendar | **draft — roadmap only** |
| RFC 8984 | JSCalendar data model | RFC — roadmap only |

---

## 15. Roadmap (v0.2+)

- **JMAP Calendar (D-3)**: `urn:ietf:params:jmap:calendars` — `Calendar/get|query|changes|set`,
  `CalendarEvent/get|query|queryChanges|changes|set|copy`, `ParticipantIdentity/*`
  ⇄ CalDAV (RFC 4791) with VEVENT conversion per draft-ietf-calext-jscalendar-icalendar.
  Google CalDAV works but is OAuth2-only (scope `…/auth/calendar`, CalDAV API
  enabled, endpoint `apidata.googleusercontent.com/caldav/v2`; the legacy
  `www.google.com/calendar/dav` is dead). Dovecot provides no CalDAV → same
  capability-gating pattern as contacts.
- `Email/queryChanges`, `Email/copy`, `ContactCard/query`, `AddressBook/set`,
  `Identity/set`, `EmailSubmission/get|query`, `Thread/changes`.
- Microsoft 365 OAuth2 profile (basic auth dies December 2026).
- WebSocket push (RFC 8620 §7.6) alongside SSE.
- Sieve/vacation, Quota, MDN — only on demonstrated demand.

---

## 16. Open questions

Resolved 2026-09-28 and locked as D-16…D-19: size caps (32 MiB JSON / 64 MiB
upload) final; `AddressBook/set` stays roadmap; body cache grow-only; NFR-1 /
NFR-8 numbers binding rather than "pin at M5".

No open questions remain for v0.1. M5 measures NFR-1/NFR-8 against the binding
numbers; any revision is a REQUIREMENTS edit at sign-off.

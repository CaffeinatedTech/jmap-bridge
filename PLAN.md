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
| D-7 | IMAP client: **`github.com/CaffeinatedTech/go-imap`**, isolated behind our own driver interface in `internal/imapdrv` (**amended 2026-09-29**: chose `kiliant/go-imap` v1.1.0 over `emersion/go-imap/v2`, which has no QRESYNC/COMPRESS and no raw-command escape hatch — verified against v2.0.0-beta.8; kiliant ships tested client QRESYNC/CONDSTORE/COMPRESS, zero deps, frozen v1 API, interop-verified incl. Dovecot. **amended 2026-09-30**: the client is now the hard fork `github.com/CaffeinatedTech/go-imap` v1.2.2 — see D-21) | 2026-09-28, amended 2026-09-29, 2026-09-30 |
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
| D-20 | Gmail's `[Gmail]/All Mail` maps to JMAP role `archive` and its membership is implicit (may_add/may_remove false): archive = remove `\Inbox` only, the message stays listed in All Mail (FR-M.18, approved by the user) | 2026-09-29 |
| D-21 | kiliant/go-imap is **hard-forked** and published as `github.com/CaffeinatedTech/go-imap` (root v1.2.2, nested `imapserver` v0.2.1), taken at kiliant's `main`; the bridge depends on it directly with **no `replace`**. The fork carries the X-GM-EXT-1 label store, flag-form value capture and long-response-line tolerance, retains upstream's MIT licence and README credit, and is not proposed upstream. `PATCH-NOTES.md` in the fork records the divergence | 2026-09-29, amended 2026-09-30 |
| D-22 | Abuse protection is **two-layer** (NFR-5, SECURITY-PLAN.md): the reverse proxy rate-limits only the **unauthenticated OAuth bootstrap** — it deliberately does **not** rate-limit or concurrency-cap authenticated JMAP traffic, since one client action is legitimately many requests and the proxy cannot see the client token; `internal/ratelimit` adds credential-aware failed-auth lockout, enforced concurrency and EventSource caps in-process. The limits advertised in the session are the limits enforced; source identity is trusted only from configured `rate.trusted_proxies` (+ `rate.client_ip_header`). App-side volume limiting is deliberately **not** duplicated for the authenticated surface | 2026-10-02, amended 2026-10-03 |

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
  - `updated` in a set response is an **object** `{"id": null}` — RFC 8620 §5.3
    `Id[Foo|null]`, measured on Fastmail and Stalwart 2026-09-29 (jmap-tui's
    wrapper also tolerates an array, but the RFC shape is the map, and go-jmap's
    typed field only decodes that),
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
         ├─ internal/sync       per-account sync engine (passes, IDLE/push, hydration, backfill)
         │   └─ internal/mailbackend  provider-neutral Backend seam (D-API-2, M8)
         │       ├─ internal/imapdrv  CaffeinatedTech/go-imap (fork of kiliant) adapter (QRESYNC, X-GM-*, PREVIEW, COMPRESS)
         │       └─ internal/gmailapi Gmail REST API adapter (M9+, GMAIL_API_PLAN.md)
         ├─ internal/submit     SMTP submission (password / XOAUTH2) + Sent APPEND
         ├─ internal/dav        CardDAV client (discovery, sync-collection, PUT/DELETE)
         ├─ internal/convert    RFC 5322 ↔ JMAP Email · vCard ↔ JSContact
         └─ internal/auth       client tokens, OAuth2 bootstrap + refresh, credential encryption
             └─ internal/config   TOML + env, strict validation
```

**Process model (v0.1):** one binary, one goroutine group per configured account
(sync loop, hydration workers, IDLE connection), one SQLite database in WAL mode
with a single writer. `context.Context` on every network call. No global mutable
state, no `init()` side effects. Each account holds up to four IMAP sessions —
the sync/work connection, a dedicated write session (§7.1), a dedicated
hydration session, and an IDLE watcher — so a long backfill never starves a read
or a mutation (FR-X.6, NFR-1).

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
  thread_key TEXT NOT NULL,         -- 'm:<sha1(Message-ID)>' | 's:<sha1(base subject)>' | 'g:<sha1(GM thrid)>' (M4)
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

CREATE TABLE oauth_tokens (         -- OAuth2 provider tokens (M4, v3)
  account TEXT PRIMARY KEY,
  refresh_token TEXT NOT NULL,      -- sealed by the oauth layer (FR-A.8)
  access_token TEXT NOT NULL DEFAULT '',
  access_expiry INTEGER NOT NULL DEFAULT 0,   -- unix seconds
  updated_at INTEGER NOT NULL
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

**M8 note:** tier dispatch, per-folder cursors and the Gmail label
strategy live in the `internal/imapdrv` adapter behind the
`internal/mailbackend` seam (D-API-2); the tables here describe that
adapter's behaviour. `internal/sync` orchestrates passes and commits
what the backend reported, reaching the store only through
`mailbackend.Hooks`.

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
| import raw message (FR-M.19) | `APPEND` the blob's bytes to the first target mailbox, then file the remaining `mailboxIds` through the membership path (Gmail label writes included) — the message is never copied per mailbox by hand |
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

**Gmail write profile** (M4, FR-S.10 / FR-M.18). On an X-GM-EXT-1 server
the membership commands above become label writes:

| JMAP op | Gmail op |
|---|---|
| add `mailboxIds/<id>` | `UID STORE ±X-GM-LABELS` on one copy holding the message — UIDs are account-global, so one command covers every folder; system folders map to their labels (`INBOX`→`\Inbox`, `[Gmail]/Sent Mail`→`\Sent`, …), user labels are the mailbox path |
| add to `[Gmail]/All Mail` | no server command — All Mail membership is implicit (every message is there by definition); the membership commits locally because it *is* the server's state |
| remove from `[Gmail]/All Mail` | refused: the server model cannot express it (`may_remove_items` is false for the implicit mailbox) |
| destroy | MOVE every copy to `[Gmail]/Trash`, then `UID EXPUNGE` there — expunge is permanent only inside Trash/Spam; without a trash mailbox the generic expunge-everywhere path is the honest fallback |
| keywords | flags are per message, not per copy: one STORE covers the account |

Archiving is the client's patch "remove INBOX, add archive(=All Mail)":
the effective delta is the `\Inbox` label removal alone, which is
exactly Gmail's archive semantics (the message remains listed in All
Mail). The `\All` mailbox maps to JMAP role `archive` at discovery.
After our own writes the reconcile paths apply a grace window
(FR-S.12): uids we touched within the last minute are not tombstoned on
a folder list that has not caught up yet — Gmail rebuilds its per-folder
indexes around label changes, and a premature tombstone is a
client-visible lie while a deferred one is one pass late. Gmail's
shared-UID namespace is honoured on the read path too: a message first
seen through one folder is deduped by (uidvalidity, uid) in every other
folder, and backfill skips the header fetch for uids the account already
knows, ingesting membership-only rows instead (FR-S.10).

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

**State strings**: both contacts types carry a per-account modseq floor,
bumped inside the same transaction that commits a server-confirmed change —
`AddressBook` when a book's membership or client-visible properties move
(discovery bookkeeping — sync-token/ctag refreshes — never moves it),
`ContactCard` whenever any card changes (including via sync-collection
results). Monotonic by construction (golden rule 5); the earlier "hash of
book sync-tokens" sketch would have let a server-side token rotation look
like a client-visible change.

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
- `GET /` and `GET /privacy`: unauthenticated static pages for the OAuth consent
  screen's home page and privacy policy (FR-D.13); neither carries account data.
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
| **M3** | Send §7.2, `Identity/get`, blob upload/download, `EmailSubmission/set` with `onSuccessUpdateEmail` | compose → send → message in Sent **and** delivered to a test sink; attachment round-trip byte-exact | FR-M.14–.17 | ✅ done 2026-09-29 (`test/live` M3 gate against dev Dovecot + in-process SMTP sink: batched draft+submission, `#draft` creation reference, patch files Sent, sink held the message with its attachment byte-exact and no Bcc, second IMAP session saw one `\Seen` copy — plus fixture-tier suites `TestSubmission*`, `internal/submit` against the SMTP fixture, and the jmap-tui cross-client rig: `TestLiveM5Gate` (compose→attach→send→Sent), `TestLiveM5ProbeDraftRoundTrip` and the M1/M2 bridge gates green over loopback) |
| **M4** | Gmail profile: OAuth2 bootstrap, XOAUTH2 IMAP/SMTP, `X-GM-LABELS`↔mailboxes, All Mail/archive, `X-GM-THRID`, CONDSTORE tier validation, rate limits | live Gmail: folders+labels both ways, compose/send, archive from jmap-tui, no rate-limit warnings | FR-A.5–.10, FR-S.10, FR-S.12, FR-M.18 | ✅ done 2026-10-01 (live Gmail the user's Gmail account, `dev/gate/m4gate.py` once, **14/14 checks, no `[THROTTLED]`**, every claim re-read from an independent `imaplib` client: a label applied in Gmail appears in the bridge; archiving from the bridge drops INBOX membership and the undo restores it; compose/send over XOAUTH2 SMTP delivers exactly one copy and files exactly one Sent copy. The gate caught and fixed **five real Gmail bugs**: a label write sourced from the folder being removed is accepted and silently ignored (archive now sources from All Mail); a fresh message with no cached All Mail copy is addressed there by a Message-ID search (`imapdrv.FindUID`); filing a draft into Sent tombstoned it (non-implicit label adds now commit locally); Gmail's draft label is `\Draft`, not `\Drafts`; and Gmail delays an SMTP submission whose Message-ID matches the upstream draft, so the submission path expunges that draft before SMTP and restores it on failure. Fixture coverage: per-folder UIDs in `internal/sync/gmail_test.go`, `TestGmailArchiveSearchesAllMailWhenUncached`, `TestGmailSendExpungesDraftBeforeSMTP`) |
| **M5** | FTS5 + search-driven backfill, filter/sort/anchor/collapseThreads correctness, `PREVIEW`/partial-fetch, COMPRESS, 100k soak | jmap-tui live search cases green; cold browse of a 100k mailbox stays responsive; soak within NFR bounds | FR-X.1–.8, FR-S.11, NFR-1, NFR-2, NFR-8 | ✅ done 2026-09-30 (dev Dovecot 100k-message corpus, `test/live TestLiveSoak100k`: warm `Email/query` p95 browse 31.1 ms / collapseThreads 32.0 ms / body-token text 82.8 ms, all < NFR-1's 150 ms; hydration 133.4 bodies/s (≥ 15); idle RSS 29.0 MB (< 150 MB, NFR-8); 3 engine-reconnect cycles flat at the 3-goroutine baseline; cold reopen browse 37.7 ms / collapse 32.6 ms — and the same full workload under the race detector with **zero data races** (the soak relaxes its NFR budgets only under `-race`, which it also enforces verbatim otherwise); jmap-tui cross-client gate `TestLiveSearchVerification` green over loopback: 3000-message text search first page 505 ms, from/subject/hasKeyword/after/before/all-mailbox filters, engine `SearchOpen`/`SearchClose`, and both substring fallbacks (partial-word + fielded-subject scan matched 3000/3000); FTS5 index at ingest + hydration, tombstones, search-driven backfill and batched hydration covered by `internal/store/fts_test.go`, `internal/sync/backfill_search_test.go` and `internal/imapdrv/fetchbodies_test.go`) |
| **M6** | CardDAV §8: discovery, sync-collection, PUT/DELETE, vCard↔JSContact, photo blobs, capability gating | jmap-tui `contacts_live_test` suite green against a real CardDAV server; create/edit/delete contact round-trips | FR-P.1–.13 | ✅ done 2026-09-30 (loopback Radicale 3.8.1 as the real server — `dev/gate/radicale-start.sh`: jmap-tui `TestLiveContactsGate` green over loopback (capability + lazy load, create → SSE ContactCard push < 8 s → rename → destroy, card proven gone from the server); bridge-side `test/live TestLiveContactsGate` green against the same Rig (create/update/move/destroy each re-read from an **independent DAV session**, RFC 6578 sync-token replay + 412/refetch write guard); the gate caught and fixed a real bug — an update re-minted the card UID (Radicale answered 409 no-uid-conflict), now the uid is canonicalized into the stored JSContact on create and reused on update. Fixture-tier coverage: `internal/dav` (sync/getctag/bare tiers, well-known redirect, conditional PUT/DELETE, multiget/GET), `test/fixturecarddav`, `internal/convert` golden pairs (v3+v4, groups, inline photos, malformed), `internal/store/contacts_test.go` (bookkeeping never moves states, cascade tombstones), `internal/jmapapi` set/patch/error-mapping tests, `internal/httpapi` FR-P.3 session gating test)
| **M7** | Packaging: multi-account paths, credential encryption, metrics/health (`[metrics]` + `/metrics`), UIDVALIDITY recovery drill, Docker image + k8s manifests, README deployment verified, `JMAP-TestSuite` run | fresh `docker run` + `kubectl` install works end-to-end from the docs; JMAP-TestSuite subset for the implemented surface green (deliberate gaps enumerated below); all gates green | FR-M.19, FR-J.9–.10, FR-D.2–.12, NFR-3–.7, NFR-9–.10 | ✅ done 2026-10-03 (live k3s `v0.1.5` at https://jmap-bridge.geekify.me — cPanel IMAP + Gmail OAuth2 accounts, jmap-tui connected, `/healthz`=ok `/readyz`=ready; `JMAP-TestSuite` vs loopback dev Dovecot: **208/262 required pass**, 28 skip, 54 required fail — **all** in the documented out-of-scope gaps below, zero in-scope failures; `[metrics]` `/metrics` demonstrated `200` enabled vs `404` disabled; FR-D.12 redaction tests; FR-S.6 UIDVALIDITY drill; all local gates green: build, vet, gofumpt, golangci-lint 0 issues, `go test -race`, version smoke) |
| **M8** | Gmail API track (D-API-8, `GMAIL_API_PLAN.md`): `internal/mailbackend` seam + `imapdrv` adapter, behaviour-preserving | every existing test green; jmap-tui live suite over loopback still green; go-imap types still confined to `imapdrv` | (refactor) | ✅ done 2026-10-03 (all repo tests green incl. `go test ./... -race`; `go build`, `go vet`, `gofumpt`, `golangci-lint` 0 issues; production go-imap imports remain confined to `internal/imapdrv` (its `imapserver` to `test/fixtureimap`); jmap-tui live suite over loopback against the dev Dovecot rig: `TestLiveSessionAndMailboxes`, `TestLiveReaderVerification`, `TestLiveTriageVerification` and `TestLiveSearchVerification` **green** against the new seam. The compose/identity probes fail and the flip-target tests skip **identically on baseline HEAD** (the dev account has no Identity and `JMAP_TUI_TEST_FLIP_*` is unset) — verified by building HEAD in a throwaway worktree, so no regression. `internal/sync` now names no provider: tier dispatch, per-folder cursors and the Gmail label strategy moved into the `imapdrv` adapter behind `mailbackend.Backend`; the store is reached only through `Hooks` function values) |
| **M9** | Gmail API client on the official `google.golang.org/api/gmail/v1` (pinned) + hand-rolled batch/quota/errors + `test/fixturegmail` + golden pairs | fixture-backed client tests incl. quota/backoff and batch correlation; cost table verified | (groundwork; see `GMAIL_API_PLAN.md`) | ✅ done 2026-10-03 (`internal/gmailapi` on `google.golang.org/api v0.300.0` + `golang.org/x/oauth2 v0.37.0`, exact-pinned in `go.mod`; the `x/oauth2` bridge over `internal/oauth.Manager` is proven by a fixture-backed test whose server accepts only the manager-refreshed bearer; token-bucket pacer charges per-method costs; `*googleapi.Error` taxonomy maps 429/403-rate→`ErrThrottled` (honouring `Retry-After`), 401→`ErrAuth`, 404→`ErrNotFound`, 403→`RejectedError`, 5xx/transport→retryable; the hand-rolled `/batch/gmail/v1` multipart endpoint correlates by Content-ID, proven against an out-of-order server and a batch whose sub-responses are all `429` under a `200` envelope; `test/fixturegmail` implements profile/labels/messages/history/threads/drafts/attachments/watch/stop/batch with normal + history-expiry + quota + eventual-visibility + unauthorized tiers. The gate's cost-table re-measurement corrected the plan: `messages.get` is **20**, not 5, and Google's per-user budget is 6 000 units/min, so the default pacer is **100/s**, not 200 — values pinned by `TestCostTable`. Mapping golden pairs cover labels↔keywords, unsupported-keyword refusal, snippet decoding, thread-key seeding and header case-folding. All local gates green: `go build`, `go vet`, `gofumpt`, `golangci-lint` 0 issues, `go test ./... -race`, version smoke) |
| **M10** | `backend`/`[accounts.gmail_api]` config + validation; API discovery, metadata backfill, history incremental, hydration, `/changes` | jmap-tui browses a fixture Gmail account in API mode; live read-only Gmail gate | FR-A.13, FR-S.13, FR-M.1–.8 (new; with code) | ✅ done 2026-10-03 (config `backend = "imap"\|"gmail_api"` with per-mode mutual-exclusion validation, Google-only OAuth2, and a loopback `endpoint`+`token` fixture escape; `internal/gmailapi.Backend` implements the read half of `mailbackend` — labels→mailboxes incl. synthetic implicit All Mail, per-container batched `messages.get(format=metadata)` backfill, per-account `history.list` incremental with `404` full-resync, `messages.get(format=raw)` hydration, snippet previews, `g:` thread keys — reusing the unmodified store/engine via a synthetic-UID `native_ids` mapping (schema v7); `cmd/jmap-bridge` picks the backend per account. **Fixture gate:** `dev/gate/gmailapi-fixture-start.sh` (standalone `test/fixturegmail` + API-mode bridge) — jmap-tui `TestLiveSessionAndMailboxes` green (1 account, 7 mailboxes, 6 roles). **Live read-only gate:** `dev/gate/gmailapi-live-gate.py` against the user's Gmail (consent re-run 2026-10-03 after the M4 token was revoked) — session over API mode, inbox/sent/archive roles, a created test label+message returned by `Email/query`+`Email/get`, an out-of-band Gmail-API `STARRED` change observed through history incremental, test message+label deleted after. Fixture coverage: `internal/gmailapi/adapter_test.go` and `internal/sync/gmailapi_test.go` (`TestGmailAPIEngineBackfillAndForeignFlag`); all local gates green) |
| **M11** | API writes: `Email/set`, `destroy`, drafts, `Mailbox/set` (refusing `onDestroyRemoveEmails=true`) | live write gate on a dedicated throwaway Gmail account, aborting on first `429` | FR-M.9–.13, FR-M.20 (new; with code) | pending |
| **M12** | API submission §8.1 via Gmail API | compose → send → exactly one Sent copy + sink delivery; ambiguous-send reconciliation proven | FR-M.14–.17 | pending |
| **M13** | Pub/Sub push: `users.watch` + renewal + stop, `/gmail/push/{account}`, `idtoken` verification, `watch="poll"` fallback | live foreign change ≤ 2 s through push; watch-renewal expiry simulation; forged push rejected; poll fallback works | FR-S.14, NFR-2 (new; with code) | pending |
| **M14** | API mode docs + re-run M7 packaging/conformance with API mode included | README install works end to end; `JMAP-TestSuite` subset green incl. API mode; all gates green | FR-D.14, NFR-3–.7 (new; with code) | pending |

**M8–M14 are the v0.1 Gmail API track** (D-API-8, `GMAIL_API_PLAN.md`). At the
user's direction (2026-10-03) M8 begins in parallel with M7's remaining
verification, because M7 is near-complete; M7's gate is still owed, the v0.1 tag
waits for both tracks, and M14 re-runs M7's packaging/conformance with API mode
included. FRs marked "new; with code" are added to `REQUIREMENTS.md` in the same
commit as the code that implements them (golden rule 7).

**Verification assets**: jmap-tui's live integration suite pointed at the bridge
(`JMAP_BRIDGE_TEST_*`); its `mockjmap`-derived fixtures seeded M0; Fastmail's
`JMAP-TestSuite` ([`jmapio/jmap-test-suite`](https://github.com/jmapio/jmap-test-suite),
Node/TypeScript) as the external conformance gate at M7.

**Known conformance gaps for that M7 run** (deliberate, and rejected rather
than faked). The external `JMAP-TestSuite` ran 2026-10-03 against the loopback
dev Dovecot rig: **208/262 required tests pass, 28 skip, 54 required fail**, and
every failure is one of the deliberate gaps below — no in-scope failure remains.

*Methods not in v0.1* (roadmap, §15): `Email/parse` (6), `Email/queryChanges`
(5), `Mailbox/queryChanges` (4), `Email/copy` (1), `Blob/copy` (1),
`PushSubscription/get|set` (7 — SSE `eventSourceUrl`, RFC 8620 §7.3, is the push
transport, not the §7.2 PushSubscription surface), `SearchSnippet/get` (6),
`Thread/changes` (4).

*Sub-features beyond the declared FR subset inside implemented methods*:
- `Email/get` header property forms (`header:Name:asRaw|asText|asAddresses|`
  `asGroupedAddresses|asMessageIds|asDate|asURLs`, case-insensitive) — 10; FR-M.4
  models the header subset, not the RFC 8621 §4.1.2 `as*` projections.
- `Email/query` filters outside FR-M.5 (`header`, `minSize`/`maxSize`,
  `notKeyword`, `some`/`noneInThreadHaveKeyword`, and the `AND`/`OR`/`NOT`
  filter operators) — 7.
- `Email/query` sorts outside FR-M.5 (`to`, `sentAt`, `hasKeyword`) — 3.
- `Mailbox/query` `hasAnyRole` (FR-M.2 filters on `parentId`/`role`) — 1.
- `Mailbox/set` `sortOrder` on update — re-derived from the server at discovery
  and refused, so the requested value is never acknowledged — 1.

Earlier deliberate gaps still stand: `Mailbox/set` accepts `name`/`parentId` and
`sortOrder` **on create** (stored in the cache until a discovery pass re-derives
it, the shape jmap-tui seeds fixtures with); `role`/`isSubscribed` on create and
`sortOrder` on update are refused with `invalidProperties`; `/get` ignores
property names it does not model instead of answering `invalidArguments`; and
`Email/get`'s top-level `blobId` names the raw copy the bridge holds and is
absent until it does, because bodies stay lazy.

---

## 13. Risk register

| Risk | Impact | Mitigation |
|---|---|---|
| `kiliant/go-imap` was new (first release 2026-08, single maintainer, low adoption); **hard-forked and self-maintained since 2026-09-30 as `CaffeinatedTech/go-imap`** | a client bug stalls sync | owning the fork removes the upstream-stall risk; the driver interface still isolates it (types never leave `internal/imapdrv`); zero-dependency + frozen v1 API keep the fork cheap to carry; its parser fuzzing and Dovecot interop matrix de-risk the gate; tier-3 fallback always exists. `X-GM-LABELS` (M4) is requestable today via its open-ended FETCH item types. **All three tiers landed and fixture-verified 2026-09-29** (QRESYNC anchor replay incl. VANISHED, CONDSTORE CHANGEDSINCE, baseline flag refetch), **and the QRESYNC tier ran against live Dovecot 2.4 the same day** (tier=qresync, COMPRESS=DEFLATE active). **The risk materialised on a live provider 2026-09-30** (commercial Dovecot): the wire decoder's 8 KiB response-line cap killed a legitimate 8.4 KiB literal-free `BODYSTRUCTURE` line (one large Outlook-msg message) and poisoned the session — every later command on that connection failed as the same opaque `invalid server response`. Fixed in the fork: default raised to 1 MiB as a memory-containment budget (the grammar bounds no untagged response), encoder literalisation budget decoupled, `MaxLineLength` exposed on `imapclient.Options`, protocol errors now carry the wrapped cause. Verified: full header pass + 50-bodies/folder pass clean on the live server (`TestFetchLongResponseLineParses` pins it). |
| M4 needed upstream code: `StoreUID` writes labels as bare atoms (Gmail labels with spaces fail) and the decoder could not capture flag-form values in unmodelled FETCH items | Gmail label writes impossible without a fork | **hard-forked to `~/projects/go-imap` and published as `github.com/CaffeinatedTech/go-imap` v1.2.2 (2026-09-30)**, nested `imapserver` v0.2.1, taken at kiliant's `main`: one capability-gated `StoreUIDGmailLabels` command + a flag-form case in `DiscardValue`; PATCH-NOTES.md records the divergence. The bridge now depends on the fork directly — the `go.mod replace` was removed, so the M7 image is unblocked |
| Gmail eventual consistency after writes | false expunge / duplicate work | grace window before tombstoning own writes (§10) — **landed with M4** (record-and-filter in the reconcile paths, FR-S.12) |
| Gmail's label/UID semantics deviate from the IMAP RFCs: UIDs are per-folder (`X-GM-MSGID` is the account-global id), removing the *selected* folder's own label is answered `OK` and ignored, the draft label is spelled `\Draft`, and an SMTP submission whose Message-ID already exists as a draft is filed as a draft-like Sent item and relayed only after a long delay | archive/send silently lie to the client (golden rule 1) | The write path never trusts those assumptions: label writes source from a copy outside the removed set, preferring implicit All Mail and falling back to a `UID SEARCH HEADER Message-ID` in All Mail when no copy is cached (`gmailLabelSource`/`gmailAllMailCopy`); the draft label uses the flag form; a label add commits its membership locally so filing does not tombstone; and `SubmitEmail` expunges the upstream draft before SMTP and re-APPENDs it on failure. **Verified live 2026-10-01** (`m4gate.py` 14/14) |
| Gmail API mode reuses the UID-shaped store/engine path for opaque string ids, and an unbounded cold backfill of a large real mailbox would burn quota | the mapping could lie, or a first sync could stall for hours | The adapter allocates a stable synthetic UID per `message.id` in `native_ids` (schema v7) so `PutMessages`/`Locations`/`UpdateFlags` are unchanged and server-first truth is preserved; `history.list` advances only after commit; `messages.get` costs 20 units and is paced, so `backfill_query`/`backfill_limit` bound a cold start (used by the live gate). **M10 read path verified live 2026-10-03** (roles/labels/threads/browse + out-of-band flag via history); writes are deliberately not implemented until M11 |
| No Go library for vCard↔JSContact | conversion bugs, data loss | golden-pair fixtures; RFC 9554 as the normative map; Stalwart `calcard` as cross-check reference. **Landed with M6** (2026-09-30): `internal/convert` v3+v4 golden pairs with byte-exact round-trip stability, unmodelled properties preserved via `@vcard-ext`; `go-vcard` carries only the field-level codec, the mapping is ours |
| Radicale answers 409 (not 404/412) for a PUT whose UID collides with another item — an unstable UID turns every bridge edit into a "foreign card" | cards become unupdatable against strict servers | UID stability is a write-path invariant, not a convenience: `prepareContact` canonicalizes the uid into the stored JSContact (create) and falls back to the row's UID (update). Pinned by `TestWriteGeneratedUidStable` + live gate |
| Provider extension roulette (Namecheap/cPanel) | sync failures | tier detection + capability probing at connect; strict-but-tolerant parsing; live tests per provider as they're added |
| OAuth callback requires public HTTPS | deploy friction | documented as a first-class deployment path (D-12); port-forward dev mode for everything else |
| Lazy bodies vs. freetext search expectations | "search misses mail" | backfill is on by default, progress is visible via SSE, `search.backfill=false` documented as a trade |
| SQLite hot rows (large mailbox counts) | slow list queries | narrow `emails` table + `email_mailbox` covering index (schema §4); count fields maintained incrementally. M5 soak (100k corpus, dev Dovecot) measured warm `Email/query` p95 well inside NFR-1's 150 ms |
| Search-driven backfill can become a re-fetch storm: every text query re-enqueues the unhydrated candidates it found, and a just-finished body can be re-enqueued before the next scan sees it | wasted provider fetches | the backfill lane drops ids on overflow (the next query re-enqueues survivors) and `doHydrate` short-circuits ids the store already shows hydrated — hydration progress lives in SQLite, which also makes backfill resumable across restarts (FR-X.6) |
| Cross-folder Message-ID dedupe vs differing IMAP flags: two copies of one self-sent message (`\Seen` in Sent, unflagged in INBOX) cannot share one JMAP object — keywords would flap by whichever folder syncs last, and unread counts with them | unread counts unstable; clients see a delivered self-sent copy born read | dedupe is lossless only: cross-folder, only while the copies have no live membership in the target folder, and only when their flag sets agree (ignoring `\Recent`) — otherwise the copy is its own message (FR-S.3, decided 2026-09-29 after the M2 push gate surfaced the race) |
| SMTP submission is inline: a send that is accepted can no longer be rolled back, and a failure *after* acceptance (filing the Sent copy, applying a patch) cannot be reported without inviting a duplicate send | a sent message with a stale local view | id minted before the send; post-acceptance failures are logged only, and the `onSuccess*` effects run after acceptance in the implicit `Email/set` (§7.2) where a patch failure is visible as `notUpdated` |
| Scope creep toward calendars/sharing | v0.1 slips | REQUIREMENTS out-of-scope list; roadmap (§15) is where those requests land |
| Client-token brute force: the token is the only client credential and, before hardening, a wrong token cost one cheap 401 at full request rate | mailbox compromise | tokens require ≥24 characters of entropy (startup error) and failed authentication is throttled then locked out per source+account (`[rate]`, FR-A.3/NFR-5). Landed 2026-10-02: `internal/ratelimit` + `authorize` lockout, unit-tested; the app-side lockout is the real mitigation (the edge cannot tell a wrong token from a valid one), with the edge OAuth `RateLimit` verified live |
| Source identity behind a proxy/CDN: grouping by the TCP peer lumps every user under the proxy (or a spoofed `X-Forwarded-For` splits one attacker across keys) | lockout misfires or is bypassed | source identity is taken only when the immediate peer is in `rate.trusted_proxies`, with `rate.client_ip_header` naming the provider's single-IP header (`CF-Connecting-IP` for Cloudflare); misconfiguration fails closed to one shared bucket. Landed 2026-10-02 (`ratelimit.ClientIP`, table-tested) |
| Concurrent requests/EventSource connections exhausted goroutines before hardening: `maxConcurrentRequests`/`maxConcurrentUpload` were advertised but unenforced, and SSE subscribers were unbounded | resource exhaustion / capability dishonesty | advertise == enforce: request and upload semaphores plus per-account/total EventSource caps wired from the same `[rate]` numbers (A4, `httpapi`). Landed 2026-10-02, connection-cap test |
| FR-D.1 image never actually built: the dev machine has no docker-daemon access (sudo needs a password) | container problems surface late, at M7 | native binary is the documented dev/test path (AGENTS.md); `docker build` + the README `docker run` are verified on a docker-capable host before M7 sign-off |
| The M8 refactor of the engine's provider calls regresses the shipping IMAP backend | a working account breaks silently | M8 is behaviour-preserving: the provider strategy moved into the `imapdrv` adapter behind `mailbackend.Backend` with no semantic change, and the gate is all repo tests (incl. `-race`) plus the jmap-tui live suite over loopback. **Demonstrated 2026-10-03**: core jmap-tui gates green through the new seam and every remaining live failure/skip reproduces on baseline HEAD in a throwaway worktree |

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
| `google.golang.org/api/gmail/v1` v0.300.0 | Gmail REST API generated client (M9+); package docs <https://pkg.go.dev/google.golang.org/api@v0.300.0/gmail/v1> | Go module, pinned 2026-10-03 |
| `golang.org/x/oauth2` v0.37.0 | OAuth2 client the generated Gmail client is bridged onto (M9+) | Go module, pinned 2026-10-03 |
| Cloud Pub/Sub push | `users.watch` change-notification transport (M13) | vendor docs |
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
- `Email/queryChanges`, `Email/copy`, `Email/parse`, `ContactCard/query`,
  `AddressBook/set`, `Identity/set`, `EmailSubmission/get|query`, `Thread/changes`,
  `SearchSnippet/get`, and `PushSubscription/get|set` (RFC 8620 §7.2; SSE
  `eventSourceUrl` covers push today).
- The conformance surface M7 deliberately excludes, named in §12: the
  `Email/get` `header:Name:as*` property forms, the `Email/query` `header`/
  `minSize`/`maxSize`/`notKeyword`/`some|noneInThreadHaveKeyword` filters and
  `AND`/`OR`/`NOT` operators, the `to`/`sentAt`/`hasKeyword` sorts,
  `Mailbox/query` `hasAnyRole`, and `Mailbox/set` `sortOrder` on update.
- **Microsoft Graph backend** behind the `internal/mailbackend` seam (D-API-2/4;
  M365 basic auth dies December 2026): a second implementation of the interface
  the Gmail API mode introduced, not a new engine.
- WebSocket push (RFC 8620 §7.6) alongside SSE.
- **Backfill steering**: order the sync pass by user intent rather than Go's map
  order (`engine.go` `doPass`) — INBOX first, then the folder a client is actively
  browsing (promote on `Email/query`/`Mailbox/get`, or the IDE'd folder), then the
  rest — so a cold mailbox is usable immediately instead of after every label
  folder has backfilled. Observed 2026-10-01 on the M4 gate run: a 13-message
  INBOX backfilled ~13 min in, behind a 6 795-message label folder, leaving the
  inbox empty in the cache the whole time.
- Sieve/vacation, Quota, MDN — only on demonstrated demand.

---

## 16. Open questions

Resolved 2026-09-28 and locked as D-16…D-19: size caps (32 MiB JSON / 64 MiB
upload) final; `AddressBook/set` stays roadmap; body cache grow-only; NFR-1 /
NFR-8 numbers binding rather than "pin at M5".

No open questions remain for v0.1. M5 measures NFR-1/NFR-8 against the binding
numbers; any revision is a REQUIREMENTS edit at sign-off.

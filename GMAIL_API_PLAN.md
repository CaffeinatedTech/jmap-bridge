# GMAIL API MODE PLAN — jmap-bridge

**Status:** approved at plan review 2026-10-03, implementation starting as
**v0.1** work (D-API-8). This file is the plan; nothing here is claimed built
except where a milestone records it — **M8 (the seam + `imapdrv` adapter)
landed 2026-10-03**, **M9 (the REST client) landed 2026-10-03**, and **M10
(config selection + the read path) landed 2026-10-03** (PLAN §12). API mode is
read-only in the current tree; writes land with M11–M12.
The `REQUIREMENTS.md` / `PLAN.md` deltas the implementation must land **in the
same commit as the code** are listed in §15 (golden rule 7).

**Goal:** let a Google account be served by the **Gmail REST API** instead of
IMAP + XOAUTH2, selected per account by a config option, while every other
account keeps using IMAP. The cache, JMAP surface, store, convert and search
layers are unchanged; only the "talk to the provider" layer gains a second
implementation.

**Why:** the IMAP path to Gmail is workable but expensive and quirky. The M4 gate
(PLAN §12, 2026-10-01) had to work around five real Gmail/IMAP deviations —
per-folder UIDs, a label write sourced from the folder being removed is accepted
and ignored, `[Gmail]/All Mail` membership, `\Draft` spelling, and delayed SMTP
submissions whose Message-ID matches an upstream draft. The Gmail API exposes
the account's actual model (account-global message ids, thread ids, labels,
history) instead of an IMAP emulation of it, and Gmail changes to it are
quota-metered HTTP rather than a shared, mutable session. It also removes the
`OK [THROTTLED]` write-quarantine class of failure: the API answers `429`/`403`
with a `Retry-After` instead of accepting a write and silently dropping it.

**Non-goals:** replacing IMAP for non-Google providers; Gmail push for the IMAP
path; JMAP Calendar (still roadmap only); rewriting the store/search/convert
layers; a generic "any HTTP mail API" abstraction in this pass (the seam is
designed to allow Microsoft Graph later, PLAN §15, but only Gmail implements it
now).

---

## 1. Locked decisions

| # | Decision | Date |
|---|---|---|
| D-API-1 | **Per-account `backend = "imap" \| "gmail_api"`**, default `"imap"`; existing configs are unaffected. IMAP and API are mutually exclusive within one account. | 2026-10-03 |
| D-API-2 | **A `mailbackend` interface** in a new `internal/mailbackend` package is the sync engine's only view of a provider. `internal/imapdrv` and a new `internal/gmailapi` implement it. This preserves D-7/D-21: go-imap library types still never leave `internal/imapdrv`, and the new REST types never leave `internal/gmailapi`. | 2026-10-03 |
| D-API-3 | **Change notification is Cloud Pub/Sub** (`users.watch` → topic → bridge HTTPS push endpoint), because FR-S.7/NFR-2 demand ≤2 s visibility and history polling cannot promise it. A `watch = "poll"` mode is kept for self-hosted/loopback setups and for fixture tests, with FR-S.7 explicitly scoped to the push mode. | 2026-10-03 |
| D-API-4 | **Gmail API only, generic seam.** `internal/mailbackend` is protocol-neutral (no Gmail or IMAP nouns); Microsoft Graph is a later package implementing the same interface, gated by a future REQUIREMENTS entry. | 2026-10-03 |
| D-API-5 | **Unsupported keywords are rejected**, never dropped: Gmail has no `\Answered`/`$answered`, no `\Deleted` state (deletion is permanent or a label move), and no arbitrary IMAP keywords. Only `$seen`, `$flagged`, `$draft` and `$important` are writable. Any other keyword write fails with `invalidProperties` naming it (FR-M.8's never-silently-drop rule, capability honesty). | 2026-10-03 |
| D-API-6 | **Submission uses the Gmail API** (`drafts.send` for an email that is a Gmail draft, `messages.send` otherwise); the account's `[accounts.smtp]` block is rejected in API mode. Gmail files the Sent copy, so the bridge never `APPEND`s a second one. | 2026-10-03 |
| D-API-7 | **Google's official Go packages, pinned.** Use `google.golang.org/api/gmail/v1` (generated client), `google.golang.org/api/option`, and `google.golang.org/api/idtoken` (Pub/Sub push OIDC verification), bridging the existing `internal/oauth.Manager` into `golang.org/x/oauth2`. Pin **exact** versions in `go.mod` so CVE watching and upgrades are deliberate: `google.golang.org/api v0.300.0`, `golang.org/x/oauth2 v0.37.0` at drafting time (2026-10-03). Hand-roll only what the official client lacks: the Gmail **batch** endpoint (the Go client has no batch support — google-api-go-client #179), the quota pacer, error classification/backoff, and the provider adapter. | 2026-10-03 |
| D-API-8 | **API mode is v0.1, starting now.** M7 is near-complete; the API track (milestones **M8–M14**, formerly A0–A6) is v0.1 and begins in parallel with M7's remaining verification. The v0.1 tag waits for both tracks, and M14 re-runs M7's packaging/conformance with API mode included. | 2026-10-03 |
| D-API-9 | **Gmail's `message.id` is the native key** (account-global, immutable, stable across label changes), stored in a new `native_ids` table. `threadId` seeds the existing `g:` thread keys exactly as `X-GM-THRID` does in M4. UIDVALIDITY has no API analogue and is not emulated. | 2026-10-03 |
| D-API-10 | **`size` is Gmail's `sizeEstimate`** while unhydrated and the exact raw length once hydrated; documented as a provider approximation (IMAP's `RFC822.SIZE` is exact; this is the honest best the API offers). `receivedAt` is `internalDate`. | 2026-10-03 |
| D-API-11 | **`[Gmail]/All Mail` remains a synthetic archive mailbox** with implicit membership (`may_remove_items = false`), reusing D-20's semantics. The API does not expose All Mail as a label; the adapter synthesizes it. | 2026-10-03 |

---

## 2. Configuration

### 2.1 Schema

```toml
[[accounts]]
id      = "gmail"
name    = "Gmail"
address = "me@gmail.com"
token   = "…"                 # client-facing token, unchanged
backend = "gmail_api"         # NEW; "imap" (default) | "gmail_api"

  [accounts.oauth2]           # required in API mode; provider must be "google"
  provider      = "google"
  client_id     = "…"
  client_secret = "…"         # or client_secret_file / env, as today

  [accounts.gmail_api]        # NEW; present iff backend = "gmail_api"
  # watch            = "pubsub" | "poll"   default "pubsub" when base_url is
  #                                          https and a topic is configured,
  #                                          else "poll"
  # pubsub_topic     = "projects/P/topics/T"
  # pubsub_audience  = "https://bridge.example.com"   # OIDC audience to verify
  # quota_units_per_second = 100       # pacer; verified 2026-10-03 (6000/min per user)
  # push_allow_plain = false            # refuse non-OIDC push unless true

  # [accounts.carddav] optional and unchanged; consumes the same OAuth2 token
```

`backend` is a new account-level key. The `[accounts.imap]` block stays exactly
as it is for IMAP accounts.

### 2.2 Validation (extends FR-A.1, same strict-decoding rules)

- `backend` absent → `"imap"`. Any value other than `"imap"`/`"gmail_api"` is a
  startup error naming `accounts[i].backend`.
- `backend = "imap"`: `[accounts.imap]` required; `[accounts.gmail_api]`
  forbidden (`unknown key` is produced automatically only for unknown keys —
  this is an explicit cross-field check, so it gets a named error).
- `backend = "gmail_api"`:
  - `[accounts.imap]` forbidden.
  - `[accounts.gmail_api]` required (an empty block is valid; it is the mode
    marker).
  - `[accounts.oauth2]` required, `provider = "google"`. A generic provider is
    rejected: the Gmail API client is Google-shaped.
  - `[accounts.smtp]` forbidden (D-API-6) — submission is via the API.
  - `address` required (Identity/get and the send `From`).
  - `watch = "pubsub"` requires `pubsub_topic` and (unless
    `push_allow_plain = true`) `pubsub_audience`, and an `https` `base_url`
    (FR-D.3 already refuses cleartext non-loopback for OAuth2 accounts).
  - `quota_units_per_second` must be `> 0` when set; `0` selects the default.
- Secrets in the block (client secret, and any future push secret) resolve
  through the existing `resolveSecrets`/`JMAP_BRIDGE_*` machinery; nothing new
  is logged.

### 2.3 Existing deployments

An account that today uses `[accounts.imap]` + `[accounts.oauth2]` continues to
work untouched: `backend` defaults to `"imap"`. Switching to API mode is a
one-line config change plus (for push) the Pub/Sub setup in §11; the `oauth2`
block and the stored refresh token are reused, and the existing
`https://mail.google.com/` scope already authorises the Gmail API including
permanent delete (verified against Google's scope docs 2026-10-03; `gmail.modify`
would not permit JMAP destroy, so the broader scope is correct and no
re-consent is needed).

---

## 3. Architecture

### 3.1 The seam

Today the sync engine depends on the concrete `*imapdrv.Conn` in ~72 call sites
(`internal/sync/{read,write,gmail,hydrate,backfill,idle,submit}.go`). M8 replaces
that with a consumer-side interface in `internal/mailbackend`:

```text
internal/mailbackend        neutral types + Backend interface + Capabilities
internal/imapdrv            adapter over CaffeinatedTech/go-imap (unchanged core)
internal/gmailapi           Gmail REST client + adapter
internal/sync               depends only on mailbackend
```

Representative shape (refined in M8, not frozen here):

```go
type Backend interface {
    Kind() Kind                       // KindIMAP | KindGmailAPI
    Capabilities() Capabilities       // preview, labels, custom keywords, idle/push

    // Session + discovery.
    Connect(ctx context.Context) error
    Close() error
    Folders(ctx context.Context) ([]Folder, error)     // roles, counts, rights
    Watch(ctx context.Context, folder string) (<-chan Change, error) // nil channel: polling only

    // Read (headers never bodies, D-2).
    Backfill(ctx context.Context, folder string, cursor Cursor, batch int) ([]Header, Cursor, error)
    Changes(ctx context.Context, cursor Cursor) (ChangeSet, Cursor, error) // incremental
    FetchPreview(ctx context.Context, ref Ref) (string, error)
    FetchRaw(ctx context.Context, ref Ref) ([]byte, error)  // hydration

    // Write (server-first, D-14).
    StoreKeywords(ctx context.Context, ref Ref, add, remove []string) error
    SetMembership(ctx context.Context, ref Ref, add, remove []string) error
    Destroy(ctx context.Context, ref Ref) error
    Append(ctx context.Context, folder string, raw []byte, keywords []string) (Ref, error)
    CreateMailbox(ctx context.Context, name, parent string) (string, error)
    RenameMailbox(ctx context.Context, id, name, parent string) error
    DestroyMailbox(ctx context.Context, id string, removeEmails bool) error
    Send(ctx context.Context, raw []byte, envelope Envelope) error
}
```

The interface **starts cursor/ref-based** — close to the shape the engine
already consumes — so M8 is a mechanical extraction rather than an engine
rewrite. `Ref` and `Cursor` are opaque, serialized into the store by the
adapter; the IMAP adapter's `Ref` is `(folder, uidvalidity, uid)` and its
`Cursor` is the existing per-folder JSON; the Gmail adapter's `Ref` is the
Gmail `message.id` and its `Cursor` is the per-account `historyId`. Tier policy
(QRESYNC/CONDSTORE/baseline) stays inside `imapdrv`, where it already lives; the
API adapter has exactly one strategy (history + label lists) and reports
`Capabilities` accordingly.

**Deliberate minimalism (ponytail):** do *not* design a grand unified
change-stream model up front. If the cursor/ref interface proves leaky for a
future Graph backend, promote it then. M8's job is to prove one non-IMAP
implementation fits, not to guess at three.

**What M8 actually landed (2026-10-03).** The shape above was refined into
`internal/mailbackend`: provider-neutral `Folder`/`FolderStatus`/`Ref`/`Cursor`/
`Header`/`Delta`/`Copy`/`Mailbox` types, the `Backend` interface, and neutral
`ErrThrottled`/`ErrAuth`/`RejectedError`. `*imapdrv.Conn` gained an adapter
(`imapdrv.Backend`) that now owns tier dispatch (QRESYNC/CONDSTORE/baseline),
per-folder cursor walking, the Gmail label membership strategy (label source
selection, implicit All Mail, trash destroy) and the shared-UID reconcile.
`internal/sync` names no provider: it orchestrates passes and commits the
reported `Delta`. Two pragmatic refinements to the sketch:

- **Full provider abstraction at the driver-operation level, not a grand batch
  API.** The adapter exposes per-container `Backfill`/`Incremental` plus
  ref-addressed fetch/write operations; the engine still decides *when* to sync
  and *what* to commit, which kept M8 behaviour-preserving and its large test
  suite intact.
- **`mailbackend.Hooks` function values** (`KnownUIDs`, `KnownSharedUIDs`,
  `LinkSharedUIDs`) carry the few store reads the adapter's reconcile needs, so
  the dependency arrow stays engine → backend and `imapdrv` never imports
  `internal/store`.

`Send` is *not* on the interface yet: submission is SMTP and stays in
`internal/submit`; the Gmail API submission path (§8.1) will add a `Send`-style
operation in M12. `Watch` is on the interface (IMAP IDLE today; Pub/Sub push in
M13).

### 3.2 Process model

One engine per account, as today. An API account opens no long-lived socket: its
HTTP client is pooled by `net/http`, so the "four IMAP sessions" accounting in
PLAN §3 becomes "four IMAP sessions **or** one HTTP client + one push
subscription". The concurrency and single-writer rules (PLAN §10) are unchanged.

---

## 4. Gmail API client (`internal/gmailapi`)

### 4.1 Transport and auth

- Use the generated client: `gmail.NewService(ctx,
  option.WithHTTPClient(hc))` from `google.golang.org/api/gmail/v1` (package
  docs: <https://pkg.go.dev/google.golang.org/api@v0.300.0/gmail/v1>). `hc` is an
  `*http.Client` whose transport attaches the bearer token from an
  `oauth2.TokenSource` backed by the existing `internal/oauth.Manager`, so
  PKCE, refresh, sealing and `ErrReauthNeeded` stay ours and propagate to the
  engine's auth-failed state (`/readyz`, FR-D.4) exactly as XOAUTH2 does today.
- Every call is `ctx`-aware through the generated `…Do()` methods.
- Pinned in `go.mod` (D-API-7): `google.golang.org/api v0.300.0`,
  `golang.org/x/oauth2 v0.37.0`. Bumps are deliberate and reviewed.
- Generated `*gmail.Message`/`*gmail.Label` types live only inside
  `internal/gmailapi` (D-API-2 seam) and are mapped to `mailbackend` types.
- Base URL is the generated default (`gmail.googleapis.com`); no endpoint is
  configurable (Google-only in this pass).

### 4.2 Batch

The generated Go client has **no batch support** (google-api-go-client #179), so
this is the one hand-rolled piece: `POST
https://gmail.googleapis.com/batch/gmail/v1` (`multipart/mixed`, up to 100
sub-requests, correlated by `Content-ID`), issued through the same authed
`*http.Client`. Backfill of a cold mailbox is list + batch-get-metadata; without
batching it is one HTTP round trip per message. Batching does not reduce quota
cost — it reduces round trips, which is the difference between a usable cold
start and a stalled one. An unbatched fallback is a config switch if the batch
endpoint ever becomes a problem.

### 4.3 Quota pacer and backoff

- A per-account token bucket paces requests at `quota_units_per_second`.
  **Verified at M9 (2026-10-03)**: Google's current per-minute per-user budget is
  **6 000 units**, i.e. **100 units/s**, so the default is **100**, not the 200
  this plan first assumed from an older figure. The value stays configurable and
  the pacer charges each method its real cost; the exact figures move, so the
  gate re-measures.
- Cost table (units), **verified at the M9 gate (2026-10-03)** against
  <https://developers.google.com/workspace/gmail/api/reference/quota>:
  `getProfile 1`, `labels.list 1`, `labels.get 1`, `labels.create 5`,
  `labels.update 5` (also used for `patch`), `labels.delete 5`,
  `history.list 2`, `messages.list 5`, **`messages.get 20`** (was 5 when this
  plan was drafted — the cost table moved), `messages.modify 5`,
  `messages.delete 10`, `messages.batchDelete 50`, `messages.batchModify 50`,
  `messages.attachments.get 20`, `messages.send 100`, `threads.get 40`,
  `drafts.create 10`, `drafts.get 20`, `drafts.send 100`, `drafts.delete 10`,
  `watch 100`, `stop 50`. These live in `internal/gmailapi/costs.go` and are
  pinned by `TestCostTable` so a future change is a deliberate re-measurement.
- `429` and `403 rateLimitExceeded|userRateLimitExceeded` honour `Retry-After`,
  then exponential backoff with jitter, and surface a neutral
  `mailbackend.ErrThrottled` that the engine's existing cooldown/queue logic can
  key off (the IMAP `OK [THROTTLED]` handling becomes backend-neutral). Errors
  are classified from the generated client's `*googleapi.Error` (from the same
  pinned `google.golang.org/api` module).
- `5xx`/network errors retry with jitter; `401`/`invalid_grant` → reauth needed;
  `404` on `messages.get`/`messages.modify` → object gone (see §8).

---

## 5. Data model deltas

One forward-only migration in `internal/store` (v5+), no changes to existing
columns:

```sql
-- Native id ↔ JMAP id, backend-neutral. IMAP keeps using imap_uids; API mode
-- uses this table. One table, kind-discriminated, so a future Graph backend
-- reuses it rather than adding a third.
CREATE TABLE native_ids (
  account   TEXT NOT NULL,
  kind      TEXT NOT NULL,      -- 'message' | 'mailbox' | 'thread'
  native_id TEXT NOT NULL,      -- Gmail message/label/thread id
  jmap_id   TEXT NOT NULL,
  PRIMARY KEY (account, kind, native_id)
);
CREATE INDEX native_ids_jmap ON native_ids(jmap_id);

-- Gmail drafts are a second handle on a message (draft.id) whose message.id
-- changes when the draft is replaced; the mapping must survive that.
CREATE TABLE gmail_drafts (
  account   TEXT NOT NULL,
  jmap_id   TEXT NOT NULL,      -- the Email id
  draft_id  TEXT NOT NULL,
  PRIMARY KEY (account, jmap_id)
);

-- Per-account API sync bookkeeping lives in the existing sync_state JSON:
--   scope='account': {"historyId":"…","watchExpiry":unix,"watchTopic":"…"}
```

- `mailboxes`: add a nullable `native_id TEXT` column (the Gmail label id), set
  in API mode; IMAP leaves it null and keeps mapping by name. Migration is
  additive and backward-compatible.
- Counts: `messagesTotal/messagesUnread/threadsTotal/threadsUnread` from
  `labels.list` seed the mailbox counts at discovery; incremental label changes
  adjust them. Counts are re-derived on every discovery pass.
- `receivedAt` = `internalDate`; `sentAt` = `Date` header (as today).
- `size` = `sizeEstimate` until hydration, then exact raw length (D-API-10).
- `email_content`/blobs/search are untouched: hydration still fetches bytes,
  parses via `internal/convert`, and stores into the same tables.

---

## 6. Synchronisation

### 6.1 Discovery

`users.getProfile` (emailAddress, historyId, messagesTotal, threadsTotal) +
`labels.list`. Map labels → mailboxes:

| Gmail label | JMAP mailbox | role | notes |
|---|---|---|---|
| `INBOX` | yes | `inbox` | `may_delete=false` |
| `SENT` | yes | `sent` | read-only as a label |
| `DRAFT` | yes | `drafts` | drafts also appear as Email objects |
| `TRASH` | yes | `trash` | |
| `SPAM` | yes | `junk` | |
| `IMPORTANT` | keyword only (`$important`) | — | hidden state, not a place |
| `STARRED` | keyword only (`$flagged`) | — | hidden state |
| `UNREAD` | keyword only (`$seen` inverse) | — | hidden state |
| `CATEGORY_*` | yes | — | inbox tabs are real label membership |
| user labels | yes | — | nested names → `parentId` hierarchy |
| `CHAT` | no | — | not mail |
| *(synthetic)* `ALL` | yes | `archive` | implicit membership, `may_remove_items=false` (D-20/D-API-11) |

`may_*` rights: system labels are not deletable/renameable; user labels are.
Mirrors `Mailbox/get`'s existing contract (FR-M.1).

### 6.2 Initial backfill (headers only)

Per label (and once for All Mail implicitly via union): `messages.list` pages
(`maxResults=500`), then batch `messages.get(format=metadata,
metadataHeaders=From,To,Cc,Reply-To,Subject,Date,Message-ID,In-Reply-To,References)`
for ids the account does not already know. `format=metadata` returns `labelIds`,
headers, `snippet` and `sizeEstimate` **without a body** — exactly the
`Header` shape the engine wants, and it satisfies D-2.

- One extra cheap pass: `messages.list(q=has:attachment)` to set
  `hasAttachment` (metadata does not include MIME parts). `q` uses Gmail search
  syntax; this is a *provider-side enumeration for an index flag*, not a change
  to the bridge's local-search architecture (D-6/FR-X are untouched).
- Resumability: page tokens are short-lived, so a restart re-walks the lists and
  skips ids already in `native_ids`. Listing is 5 units/page and fetches no
  bodies, so a re-walk is cheap; exact cursor durability is not worth a guess
  about token lifetime.
- Dedupe: Gmail has one message object with N labels, so there is no
  cross-folder duplicate problem — the M4 lossless-dedupe machinery is not
  needed and the API adapter never emits duplicate copies for one message.

### 6.3 Incremental (history)

`history.list(startHistoryId=cursor, maxResults=500, pageToken=…)`, pages read
to completion, then applied in one store transaction that also advances the
saved `historyId`. Record kinds:

- `messagesAdded` → batch-get metadata, ingest.
- `messagesDeleted` → tombstone (permanent delete).
- `labelsAdded`/`labelsRemoved` → update membership/keywords from the record's
  `labelIds` (or re-get metadata if the record is ambiguous).

Rules:

- History is **chronological and monotonic** (historyId strictly increases).
  The cursor is saved only after the whole page set commits (golden rule 5).
  Crash mid-apply → re-fetch from the last saved cursor; the apply is
  idempotent.
- `404` on `history.list` (start too old — Google guarantees ~a week, sometimes
  only hours) → **full resync**: re-enumerate all label memberships, batch-get
  unknown/changed messages, tombstone cached ids not seen, then re-read
  `historyId`. Clients see a normal create/update/destroy change set; no id is
  ever recycled.
- Our own writes also appear in history. The existing `ownWrites` grace window
  (FR-S.12) is retained so a read-your-write reconcile does not tombstone work
  the API has not surfaced yet.

### 6.4 Push (Pub/Sub, D-API-3)

- `users.watch(topicName)` on connect; response carries an `expiration`.
  Renewal runs **daily** (a new `watch` call resets the 7-day window; daily
  gives a multi-day buffer against transient failures). `users.stop` on
  graceful shutdown.
- Gmail publishes to the topic; the Pub/Sub **push subscription** POSTs to
  `POST {base_url}/gmail/push/{account}`. The body is base64 JSON
  `{"emailAddress":…,"historyId":…}` — only a "something changed" nudge; the
  bridge then runs the §6.3 history pass. Idempotent and safe to receive
  duplicates or out-of-order hints.
- **Push authentication (NFR-5):** the endpoint is on the public origin and
  cannot carry the client token. Verify the Pub/Sub OIDC token in the
  `Authorization: Bearer` header with `idtoken.Validate(ctx, bearer,
  pubsub_audience)` from `google.golang.org/api/idtoken` (D-API-7), then check
  the `email` claim equals the configured push service account. The verifier is
  hidden behind a small `PushVerifier` seam so tests substitute a fake and one
  real-token integration test is skipped unless env creds are set. A
  `push_allow_plain = true` escape hatch accepts a high-entropy path secret for
  self-hosted rigs and the fixture tests; it fails closed otherwise. The OAuth
  consent/`/`, `/privacy` pages and the `rate` middleware keep working; the push
  route is added to the dispatch in `ServeHTTP` alongside the `/oauth/…`
  special-case, and is *not* behind client auth.
- `watch = "poll"`: no Pub/Sub; the engine polls `history.list` on
  `sync.interval` (or a configured shorter interval). FR-S.7's ≤2 s guarantee is
  explicitly a push-mode guarantee; poll mode is `sync.interval` as IMAP's
  fallback already is. This also keeps the fixture suite simple.

### 6.5 Body hydration

`Email/get` requesting body properties with `hydrated_at IS NULL` →
`messages.get(format=raw)` → base64url-decode → `convert.RFC5322ToEmail` →
store content, split attachments into blobs, index FTS5, set `hydrated_at`.
`format=raw` gives exact bytes and full MIME fidelity, so the existing parser is
reused unchanged. `size` is corrected to the exact length at this point.
Single-flight (FR-S.8) and the dedicated hydration worker lane are unchanged.
Previews use the metadata `snippet` (HTML-entity-decoded, invisible padding
stripped) with a body-fetch fallback when empty.

---

## 7. Read mapping (Gmail message → JMAP Email)

| JMAP property | Gmail source |
|---|---|
| `id` | bridge-minted JMAP id (`native_ids`) |
| `threadId` | `native_ids(kind='thread')`, seeded from `threadId` as `g:<threadId>` (same scheme as M4) |
| `mailboxIds` | `labelIds` mapped to mailboxes, plus synthetic All Mail (always true) |
| `keywords` | `UNREAD`→¬`$seen`, `STARRED`→`$flagged`, `DRAFT`→`$draft`, `IMPORTANT`→`$important` |
| `receivedAt` | `internalDate` |
| `sentAt` | `Date` header |
| `size` | `sizeEstimate` (hydrated: exact) |
| `preview` | `snippet` (decoded) |
| `hasAttachment` | `has:attachment` index pass |
| `from/to/cc/bcc/replyTo/subject/…` | metadata headers via the existing header mapper |
| `bodyStructure`/`bodyValues`/`attachments` | `format=raw` → `internal/convert` (unchanged) |
| `blobId` | the bridge's raw blob once hydrated; absent before (M3 rule) |

`Thread/get`, `Email/query`, `/changes`, state strings, FTS, search-driven
backfill: all unchanged — they read the store, and the store is fed the same way.

---

## 8. Write mapping (JMAP → Gmail API)

| JMAP op | Gmail API | notes |
|---|---|---|
| add/remove `mailboxIds/<id>` | `messages.modify{addLabelIds,removeLabelIds}` | one call per message; membership = labels |
| archive (remove INBOX, add All Mail) | remove `INBOX` only | All Mail is implicit; the add is a local no-op (D-20) |
| remove All Mail | refused (`may_remove_items=false`) | the API cannot express it |
| `keywords/$seen` | add/remove `UNREAD` (inverse) | |
| `keywords/$flagged` | add/remove `STARRED` | |
| `keywords/$draft` | `DRAFT` label (guarded) | content edits on a draft go via `drafts.update`; v0.1 keeps the IMAP parity rule that a draft's body is immutable — see §9 |
| `keywords/$important` | add/remove `IMPORTANT` | Gmail-specific, surfaced |
| `keywords/$answered`, `$deleted`, custom | **refused** `invalidProperties` | D-API-5 |
| destroy | `messages.delete` (permanent) | `404` = already gone = no-op success (FR-M.10); bulk via `messages.batchDelete` |
| create draft | `drafts.create{message.raw}` | store both `message.id` and `draft.id`; MIME built by `internal/convert/build.go` |
| `Mailbox/set` create/rename/delete | `labels.create` / `labels.patch` / `labels.delete` | nesting via `/` names; role/isSubscribed re-derived, refused as today |
| `Mailbox/set` destroy `onDestroyRemoveEmails` | see below | |

- **`onDestroyRemoveEmails`:** deleting a Gmail label never deletes messages, so
  `onDestroyRemoveEmails = true` cannot be honoured without permanently deleting
  mail the label API never asked us to touch. API mode **refuses it** with a
  named error (§16.4). `removeEmails = false` keeps the current behaviour: a
  non-empty label → `mailboxHasEmail` (RFC 8621 §2.5); an empty label → delete.
- Server-first ordering is identical to IMAP (D-14): local commit, modseq bump
  and SSE only after the API call returns 2xx; failures map to
  `notUpdated`/`notDestroyed`/`notCreated` with the API reason in
  `serverFail`/`invalidProperties` (FR-M.13).

### 8.1 Submission (§9 detail)

1. Validate the whole create and `onSuccessUpdateEmail` patch before any call.
2. If the Email maps to a Gmail draft (`gmail_drafts`), call
   `drafts.send{draftId}` — Gmail consumes the draft and files Sent. Otherwise
   build raw MIME (Bcc stripped, as today) and call `messages.send`.
3. On 2xx, record the Sent message (it is Gmail's own copy; do **not**
   `APPEND`). Return `created` with `undoStatus: "final"`.
4. The `onSuccess*` effects run as the existing implicit `Email/set`; its
   response follows (RFC 8621 §7.5).
5. **Ambiguous send** (timeout/reset after dispatch): Gmail mints/keeps a
   message id and a retry can duplicate (the same class of bug the M4 IMAP gate
   caught). Because the bridge controls the `Message-ID`, on an ambiguous
   outcome it queries `messages.list(q=rfc822msgid:<id>)`; a hit means accepted
   (record it, report success), a miss after a bounded wait means retryable.
   Never report accepted-after-the-fact work as a failed create.

---

## 9. Semantic gaps and capability honesty

Documented in README's provider matrix and enforced, not faked:

| Gap | Handling |
|---|---|
| no `$answered` | refuse `$answered` writes; never surface it |
| no `$deleted` keyword | deletion is permanent (`messages.delete`) or a Trash label move; `$deleted` refused |
| no arbitrary custom keywords | only `$seen`/`$flagged`/`$draft`/`$important`; the rest refused (FR-M.8 is amended to be backend-conditional) |
| `size` approximate | `sizeEstimate` until hydration (D-API-10) |
| All Mail not a real label | synthetic archive mailbox, implicit membership |
| Gmail counts include provider-only states | counts seeded from `labels.list` and re-derived each discovery; documented |
| drafts are a separate resource | `native_ids` + `gmail_drafts`; content edits on a draft are out of scope in v0.1, matching IMAP draft immutability |
| local search only | unchanged (D-6/FR-X): Gmail query operators are not exposed |
| `attachmentId` is transient | attachment bytes are copied into the bridge blob store at hydration; the JMAP blobId is the bridge's, not Gmail's |
| no IDLE | Pub/Sub push (or poll); `Capabilities` reports push vs poll |

The session continues to advertise `urn:ietf:params:jmap:mail` only. No new JMAP
capability is invented; a Gmail-API account is just another mail account behind
the same surface.

---

## 10. Observability

- Structured logs and metrics gain a `backend` label
  (`imap`|`gmail_api`) and, for API accounts: `history_id`, `watch_expiry`,
  `quota_units_per_second`, `rate_limited_total`, `history_full_resync_total`.
- `/readyz` is unchanged: an API account is ready after its first successful
  full sync; `oauth.ErrReauthNeeded` marks it auth-failed exactly as today.
- Never log tokens, raw messages, or the Pub/Sub OIDC token.

---

## 11. Deployment (new operator work)

API mode with Pub/Sub adds real Google Cloud setup; this is unavoidable if
FR-S.7 is to hold. README §Deployment gains a "Gmail API mode" section:

1. Enable the **Gmail API** in the OAuth client's Cloud project (the IMAP-only
   path did not need it).
2. Create a **Pub/Sub topic**; grant `roles/pubsub.publisher` to
   `gmail-api-push@system.gserviceaccount.com`.
3. Create a **push subscription** to
   `https://<base_url>/gmail/push/<account>` with OIDC authentication (service
   account + audience); the bridge verifies the attached token with
   `idtoken.Validate` and checks the service-account `email`. Or create a pull
   subscription only if `watch = "poll"` is used (no push needed).
4. Scopes: the existing `https://mail.google.com/` consent already covers the
   Gmail API and permanent delete; no re-consent. (`gmail.modify` is *not*
   sufficient because JMAP destroy must be permanent.)
5. Self-hosted without a public HTTPS origin: `watch = "poll"` and accept
   `sync.interval` latency. `push_allow_plain` exists for loopback rigs only.

The OAuth callback, one-origin rule (D-13/D-15) and reverse-proxy/TLS
requirements are unchanged; the push endpoint is one more route on the same
origin.

---

## 12. Testing

- **`test/fixturegmail`** — in-process HTTP server implementing the used subset
  of the Gmail API v1: `users.getProfile`, `users.watch`/`stop`,
  `labels.list/create/patch/delete`, `messages.{list,get,modify,send,delete,
  batchDelete,batchModify,attachments.get}`, `threads.get`, `drafts.*`,
  `history.list`, and the `batch` endpoint. It must impersonate the failure
  tiers the way `test/fixtureimap` impersonates IMAP tiers:
  - normal;
  - **history expiry** (`404` on a stale `startHistoryId` → full resync path);
  - **quota** (`429`/`403` with `Retry-After`, and a batch whose sub-responses
    are all throttled under a 200 envelope);
  - **eventual visibility** (a `modify` accepted before the history record
    appears) to pin the own-writes grace window.
- **Golden pairs** in `internal/gmailapi`: message metadata/raw → JMAP Email
  (labels→mailboxes, snippet decoding, keyword mapping, thread seeding) and
  JMAP/`EmailPatch` → `modify`/`drafts.create` requests, including degenerate
  inputs (empty snippet, missing headers, no labels beyond All Mail).
- **Push verification** unit tests through the `PushVerifier` seam: valid,
  expired, wrong-audience, wrong-issuer and bad-signature tokens; plain-secret
  path; replay/duplicate hints; plus one real-token test that skips unless env
  creds are set (never hardcoded).
- **Interface parity**: the existing `internal/sync` fixture suites run against
  the IMAP adapter after M8 (regression net); a parallel subset runs against the
  Gmail fixture in M10+ via the same interface.
- **Live gates** follow the M4 lesson (dedicated throwaway Gmail account, never
  the personal one; abort on the first `429`; bounded, courtesy-spaced writes).
  `dev/gate/gmailapi-gate.py` is the API sibling of `m4gate.py`.

---

## 13. Milestones and gates

Each milestone ends with a demonstrated gate before the next begins. These are
**v0.1** milestones (D-API-8), numbered **M8–M14** (formerly A0–A6) so they slot
into PLAN §12; M8 begins in parallel with M7's remaining verification, the
M8–M14 rows are already recorded there, and the FRs land with the code. M8 is a
pure refactor; M9–M14 mirror the M1–M4 gating style.

| # | Deliverable | Gate | FRs |
|---|---|---|---|
| **M8** | `internal/mailbackend` seam; `imapdrv` adapter; sync engine depends only on the interface; `ErrThrottled`/auth errors made backend-neutral — **landed 2026-10-03 (PLAN §12 gate ✅ done)** | **no behaviour change**: every existing test green; jmap-tui live suite over loopback still green; go-imap types still confined to `imapdrv` | none new (refactor) |
| **M9** | Gmail REST client on the official `google.golang.org/api/gmail/v1` package + `x/oauth2` bridge, pinned; hand-rolled batch, quota pacer, error taxonomy; `test/fixturegmail` skeleton + golden pairs — **landed 2026-10-03 (PLAN §12 gate ✅ done)** | fixture-backed client tests green incl. quota/backoff and batch correlation; cost table verified against Google's docs (corrected `messages.get` to 20) | (groundwork) |
| **M10** | Config `backend`/`[accounts.gmail_api]` + validation; discovery, initial backfill, incremental history, hydration, `/changes` — **landed 2026-10-03 (PLAN §12 gate ✅ done)** | jmap-tui browses a fixture Gmail account in API mode (`dev/gate/gmailapi-fixture-start.sh`); **live read-only** gate (`dev/gate/gmailapi-live-gate.py`, user's real account, backfill scoped to a test label) — folders/labels/counts/threads correct; an out-of-band Gmail-API flag change appears through `history.list` | FR-A.13, FR-S.13 (new), FR-M.1–.8 |
| **M11** | Write path §8; drafts; `Mailbox/set` (incl. refusing `onDestroyRemoveEmails=true`, §16.4) | live write gate (dedicated account, abort on first `429`): label both ways, archive, star, move, `Mailbox/set`, draft create, destroy; every claim re-read from the Gmail web/API independently | FR-M.9–.13, FR-M.20 (new) |
| **M12** | Submission §8.1 | compose → send → exactly one Sent copy + delivery to a test sink; ambiguous-send reconciliation proven (simulated timeout) | FR-M.14–.17 |
| **M13** | Pub/Sub push: `users.watch` + renewal + stop, `/gmail/push/{account}`, `idtoken` verification, `watch="poll"` fallback | live foreign change visible ≤2 s through push; watch renewal survives an expiry-simulation; forged push rejected; poll fallback works with no Pub/Sub | FR-S.14 (new), NFR-2 |
| **M14** | Docs (README API mode + Pub/Sub setup), config-migration note, and a **re-run of M7's packaging/conformance with API mode included** | documented install works end to end from README on a clean host; `JMAP-TestSuite` subset green incl. API mode; all gates green | FR-D.14 (new), NFR-3–.7 |

**Deferred (roadmap):** Microsoft Graph backend; draft body editing; Gmail
`users.history`-based query acceleration; Pub/Sub pull mode.

---

## 14. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Backfill costs one `messages.get` per message (list returns ids only) at 5 units each; a 100k mailbox is ~500k units against a per-user budget of a few hundred units/s | cold start takes tens of minutes and can trip `429` | batch endpoint (100/request) cuts round trips; per-account pacer + `Retry-After` backoff; backfill is resumable and lowest-priority vs interactive hydration; measure and document at M10 |
| History can expire in hours, not just a week | silent miss of changes | `404` → full resync path, fixture-pinned; cursor advanced only after commit |
| Pub/Sub setup is non-trivial and doubles the deployment story | operators stay on IMAP | `watch="poll"` keeps API mode usable without GCP; README documents both; push is the ≤2 s path only |
| Pub/Sub push verification is security-critical | forged push triggers needless sync (no data write — history is authoritative) or a DoS | `idtoken.Validate` (official package) plus the configured service-account `email`; push only triggers a history read from our own cursor, so a forged push cannot inject state; fail closed; hidden behind a `PushVerifier` seam and unit-tested for all failure modes |
| The official Google dependency tree (`google.golang.org/api` + `golang.org/x/oauth2`) is large and moves fast | supply-chain/CVE exposure and image growth | both modules **exact-pinned** in `go.mod` (`v0.300.0` / `v0.37.0` at drafting, D-API-7), CVE-watched and bumped deliberately; generated client types confined to `internal/gmailapi`; the IMAP path gains no dependency |
| `sizeEstimate` is approximate | JMAP `size` not exact until hydration | documented divergence (D-API-10); exact once hydrated; clients treat size as advisory |
| Ambiguous send can duplicate | double email delivered | bridge-generated `Message-ID` + `rfc822msgid:` reconciliation before any retry; never report accepted work as failed |
| M8 refactor of ~72 call sites regresses IMAP | the shipping backend breaks | M8 is behaviour-preserving and gate is "all existing tests + live jmap-tui suite green"; IMAP tier logic does not move in M8 |
| Gmail API and IMAP disagree on the same account if both are configured | split brain | validation forbids `[accounts.imap]` in API mode; one account = one backend |
| Google deprecates/limits an endpoint (history, batch) | API mode degrades | batch has a flag to fall back to unbatched gets; watch has poll fallback; the IMAP path remains available per account |

---

## 15. Documentation deltas to land with the code

Golden rule 7 (scope) and rule 8 (status). The PLAN §12 M8–M14 rows and the §15
Graph note are recorded now (2026-10-03); everything else lands with the code.

**REQUIREMENTS.md**
- FR-A.13: account `backend` selection; API-mode validation (§2.2).
- FR-S.13: Gmail API synchronisation (discovery, metadata backfill, history
  incremental, full-resync on expiry) and its state/`/changes` guarantees.
- FR-S.14: push notification (`users.watch` + Pub/Sub) with renewal, poll
  fallback, and FR-S.7 scoped to push mode.
- FR-M.20: Gmail API mail semantics (label membership, synthetic All Mail,
  permanent destroy, keyword set, draft resource).
- FR-D.14: Gmail API deployment additions (enable API, Pub/Sub topic +
  subscription + IAM, push endpoint, OIDC).
- FR-M.8: amended to be backend-conditional for custom keywords.
- NFR-2: "IDLE healthy" becomes "IDLE healthy (IMAP) / push healthy
  (Gmail API)".

**PLAN.md**
- §1: add D-API-1…D-API-11 (or reference this file and keep the numbering
  here).
- §3 architecture diagram: add `internal/gmailapi` and `internal/mailbackend`.
- §4 schema: `native_ids`, `gmail_drafts`, `mailboxes.native_id`.
- §5 sync: add the API strategy and the Pub/Sub notifier.
- §12: **already recorded** (2026-10-03) — M8–M14 as `pending` rows; they begin
  in parallel with M7's remaining verification, and the v0.1 tag waits for both
  tracks.
- §13 risk register: the risks in §14.
- §14 spec register: Gmail API v1 (vendor docs), Cloud Pub/Sub push, and the
  pinned `google.golang.org/api` / `golang.org/x/oauth2` versions.
- §15 roadmap: the Gmail API bullet lives in §12 now, not §15; keep a Microsoft
  Graph backend note behind the `mailbackend` seam.

**README.md**
- Provider matrix: Gmail (IMAP+XOAUTH2) and Gmail (API mode) as distinct rows
  with the §9 gaps.
- Deployment: "Gmail API mode (Pub/Sub)" section and the poll alternative.

---

## 16. Decisions resolved at review (2026-10-03)

All five review questions are answered; D-API-7/D-API-8 above carry the two that
changed the plan.

1. **Version placement — v0.1, now.** M7 is near-complete; the API track is v0.1
   work (M8–M14) and starts in parallel with M7's remaining verification.
2. **Push transport — Pub/Sub required for ≤2 s**, with `watch = "poll"` as the
   documented degraded fallback (no GCP setup; `sync.interval` latency).
3. **`sizeEstimate` accepted** until hydration (D-API-10); exact once cached.
4. **`onDestroyRemoveEmails = true` is refused** in API mode with a named error;
   `removeEmails = false` keeps the current `mailboxHasEmail`-if-non-empty
   behaviour. Never acknowledge a bulk message destroy the label API cannot
   express without blasting mail.
5. **Official Google Go packages** are used wherever they exist
   (`google.golang.org/api/gmail/v1`, `.../option`, `.../idtoken`,
   `golang.org/x/oauth2`), exact-pinned for CVE tracking; only the batch
   endpoint, quota pacer, error classification and the adapter are hand-rolled.

No blocking open questions remain. Follow-ups that do **not** block M8: confirm
the live-gate throwaway Gmail account exists, and settle whether M14's
conformance re-run uses Fastmail's `JMAP-TestSuite` from M7's setup unchanged.

# REQUIREMENTS — jmap-bridge

Scope and acceptance criteria for v0.1. **This file is the source of truth for
what the product does.** `PLAN.md` holds architecture and milestones; `README.md`
is user-facing. A feature not listed here is not in scope until this file changes
(same commit as the code).

Requirements are written as testable statements: `FR-<group>.<n>`. Groups:
**A** accounts/auth, **S** synchronisation, **M** mail, **P** contacts,
**X** search, **J** JMAP protocol contract, **D** deployment. Non-functional
requirements are `NFR-<n>`.

---

## 1. Scope

**In v0.1**

- JMAP Mail (RFC 8621) over HTTP for browsing, reading, flagging, moving,
  deleting, threading and drafting against an IMAP backend.
- Sending via `EmailSubmission/set` → SMTP submission, filed to Sent on IMAP.
- JMAP Contacts (RFC 9610) over CardDAV for the same account's provider.
- A local hybrid cache (headers/flags/structure always; bodies lazy) with an
  FTS5 index and search-driven body backfill.
- Docker/Kubernetes deployment behind a TLS reverse proxy; Gmail OAuth2.
- Multi-account, path-prefixed, single origin.

**Explicitly out of scope (v0.1)** — see also PLAN §15 roadmap:

- JMAP Calendar / CalDAV (roadmap), `Principal`/sharing/ACL, `VacationResponse`,
  `MDN`, `Quota`, Sieve management, `SearchSnippet`, `Email/import`, `Email/parse`,
  WebSocket push (SSE only), `Email/queryChanges`, `Email/copy`,
  `ContactCard/query|queryChanges|copy`, `AddressBook/set`, `Identity/set`,
  `EmailSubmission/get|query` (roadmap items are listed in PLAN §15).
- Being an MTA/MDA: the bridge never accepts inbound mail.
- POP3. Multi-instance/high-availability operation (single writer by design).
- Gmail via app passwords (D-5: OAuth2 only).

**Glossary**

| Term | Meaning |
|---|---|
| bridge / this server | the jmap-bridge process |
| account | one configured upstream mailbox (IMAP+SMTP [+CardDAV]) served at `/{account}/…` |
| tier | one of the three IMAP sync strategies (QRESYNC / CONDSTORE / baseline) |
| hydration | fetching and parsing a message body on demand, then caching it |
| backfill | (a) initial header sync, (b) background body hydration triggered by search — context distinguishes them |
| foreign change | a change made by another client directly against the IMAP/CardDAV server |
| origin | scheme + host + port; clients authenticate only to one origin |

---

## 2. FR-A — Accounts, authentication and credentials

- **FR-A.1** Configuration is a TOML file decoded strictly: unknown keys, wrong
  types, and missing required fields are startup errors with the offending key
  named. Validation covers `listen`, `base_url` (absolute http-for-loopback or
  https), `data_dir`, and every account block.
- **FR-A.2** Secrets (IMAP/SMTP passwords, OAuth client secrets, refresh tokens,
  client tokens) may come from config, `JMAP_BRIDGE_*` environment variables, or
  mounted files. Secret values are redacted from logs and error strings, and are
  never written to the data volume unencrypted when a key is configured.
- **FR-A.3** Client-facing authentication is HTTP Basic on **every** endpoint
  (session, `/jmap`, upload, download, eventsource): username is any non-empty
  value, password is the account's token, compared in constant time against a
  stored hash. `auth.mode = "none"` is accepted **only** when `listen` binds a
  loopback address; otherwise startup fails (FR-A.11).
- **FR-A.4** Backend password authentication: accounts with `auth = "password"`
  log in to IMAP and SMTP with the configured credentials (SASL PLAIN / LOGIN),
  over TLS by default; plaintext transport requires an explicit config flag.
- **FR-A.5** OAuth2 bootstrap: the bridge serves `GET /oauth/{account}/start`
  (redirects to the provider consent screen with PKCE S256 and a random `state`)
  and `GET /oauth/{account}/callback` (exchanges the code, stores the refresh
  token, then starts/refreshes that account's sync). Both are served on the
  `base_url` origin, which is the publicly reachable HTTPS endpoint documented in
  README §Deployment.
- **FR-A.6** The callback validates `state`, binds the result to exactly the
  account in the path, shows a minimal success/failure page (no JavaScript
  dependencies), and never logs the authorization code or tokens. A failed or
  forged callback leaves existing credentials untouched.
- **FR-A.7** Refresh tokens are used to mint access tokens: refreshed proactively
  before expiry, and on authentication failure with exactly one retry. XOAUTH2 is
  used for IMAP `AUTHENTICATE` and SMTP `AUTH` on accounts with `auth = "oauth2"`.
- **FR-A.8** Credentials and refresh tokens are encrypted at rest with
  AES-256-GCM using `JMAP_BRIDGE_SECRET_KEY` (32 bytes, env/K8s Secret, never in
  the data volume). Without a key the bridge starts in plaintext mode and logs a
  prominent warning. Decryption auto-detects ciphertext prefixes so keys can be
  rotated by re-encrypting on next write.
- **FR-A.9** Provider profiles: `provider = "google"` supplies Gmail endpoints and
  scopes (`https://mail.google.com/`, plus `…/auth/carddav` when contacts are
  configured); `provider = "generic"` takes `auth_url`, `token_url`, `scopes`,
  `client_id`, `client_secret` from config so other providers work without code
  changes.
- **FR-A.10** An account declared `auth = "oauth2"` has **no** password fallback:
  misconfiguration is a startup error, never a silent downgrade to basic auth.
- **FR-A.11** Multi-account isolation: a token authenticates exactly one account;
  a request under `/{account}/…` is served only for that account's data; cross-
  account access returns 401/404 without revealing whether the other account
  exists. Startup validation rejects duplicate account ids and a `none` auth mode
  on non-loopback listeners.
- **FR-A.12** All of the above fail closed: any authentication ambiguity (missing
  header, unknown token, wrong path/account pairing) results in no credentials
  being used and no data being served.

## 3. FR-S — Synchronisation

- **FR-S.1** On connect the bridge reads `CAPABILITY`, lists folders with
  LIST-EXTENDED + SPECIAL-USE (mapping `\Inbox \Sent \Drafts \Trash \Junk
  \Archive` to JMAP roles), reads NAMESPACE (prefix/hierarchy separator), and
  enables `UTF8=ACCEPT` and `COMPRESS=DEFLATE` when advertised. Results are
  logged and exposed in metrics.
- **FR-S.2** Tier selection: QRESYNC if advertised, else CONDSTORE if advertised,
  else baseline. The selected tier is recorded per folder per session and
  downgrade paths (server loses an extension after re-auth) are handled without
  restart.
- **FR-S.3** Initial backfill fetches per folder, in batches, `UID, FLAGS,
  INTERNALDATE, RFC822.SIZE, ENVELOPE, BODYSTRUCTURE` — **never full bodies**.
  It is resumable across restarts (no re-download of completed ranges) and
  reports progress.
- **FR-S.4** Liveness: `IDLE` on INBOX and watched folders; on disconnect,
  exponential backoff reconnect; when IDLE is unavailable or unreliable, poll on
  `sync.interval`.
- **FR-S.5** Incremental sync per tier: tier 1 uses `SELECT (QRESYNC …)` +
  `FETCH (CHANGEDSINCE)` + `VANISHED`; tier 2 uses `UIDNEXT` scans for new UIDs +
  `UID FETCH (CHANGEDSINCE)` for flag changes with UID-gap rescan for expunges;
  tier 3 diffs UID lists and refetches flags. Expunged messages become
  tombstones, not silent row deletions.
- **FR-S.6** `UIDVALIDITY` change on a folder: remap survivors to fresh JMAP ids,
  drop stale mappings, surface the event as destroy/create in `/changes`. No
  client may ever observe an id that has been silently recycled.
- **FR-S.7** Foreign-change visibility: ≤ 2 s with IDLE healthy; ≤ `sync.interval`
  otherwise (FR-NFR-2).
- **FR-S.8** Body hydration is on demand, single-flight per message (concurrent
  requests for the same body wait on one IMAP fetch), cached permanently in the
  blob store, and re-parseable after a cache clear without data loss (bodies can
  be re-fetched from IMAP).
- **FR-S.9** Optional prefetch: bodies for messages newer than
  `sync.prefetch_window` are hydrated in the background, rate-limited, and can be
  disabled (`0`).
- **FR-S.10** Gmail profile: `X-GM-LABELS` values map to JMAP mailboxes in the
  Gmail label namespace; `[Gmail]/All Mail` membership is preserved so that
  "archive" means "remove from INBOX only"; `X-GM-THRID` seeds local thread ids
  when present; label writes prefer `UID STORE +X-GM-LABELS` over copy storms;
  Gmail's lack of QRESYNC must land on tier 2 and still satisfy FR-S.5.
- **FR-S.11** Compression (`COMPRESS=DEFLATE`) is enabled opportunistically and
  its use is visible in metrics.
- **FR-S.12** Provider courtesy: bounded batch sizes, jittered backoff on errors
  or untagged `BYE`, no tight retry loops, and a grace window before treating an
  absent result as a deletion for changes we ourselves just wrote (Gmail's
  eventual consistency).

## 4. FR-M — Mail

- **FR-M.1 `Mailbox/get`** returns all mailboxes (or by id) with: id, name,
  parent, role, sortOrder, counts (total/unread emails and threads), `may*`
  rights, and the type's state string. Hierarchy is preserved exactly as the
  server exposes it.
- **FR-M.2 `Mailbox/query`** supports filtering (`parentId`, `role`) and sorting
  by `sortOrder` ascending, plus `position`/`limit`/`calculateTotal`.
- **FR-M.3 `Mailbox/changes`** returns created/updated/destroyed ids since
  `sinceState` with `newState`, correct even when only counts changed
  (`updatedProperties` may be omitted in v0.1).
- **FR-M.4 `Email/get`** honours `ids` (or "all"), `properties`, `bodyProperties`
  and `fetchAllBodyValues`: with summary properties it never touches bodies;
  with body properties it hydrates (FR-S.8), returns `bodyStructure`, per-part
  `partId`s, `bodyValues`, `attachments` (with `blobId`), `textBody`/`htmlBody`
  selections, and the correct `state`.
- **FR-M.5 `Email/query`** supports: filters `inMailbox`, `text`, `from`, `to`,
  `subject`, `after`, `before`, `hasKeyword`, `hasAttachment`; sorts on
  `receivedAt`, `subject`, `from`, `size`, `hasAttachment` (asc/desc);
  `collapseThreads`, `position`, `limit`, `anchor`/`anchorOffset`,
  `calculateTotal`, and a stable `queryState`.
- **FR-M.6 `Email/changes`** returns created/updated/destroyed since `sinceState`
  with `newState`; tombstones remain visible for at least the window in NFR-4.
- **FR-M.7 `Thread/get`** returns `{id, emailIds}` per thread, ordered by
  `receivedAt`; thread ids are stable across syncs and derived from
  Message-ID/References chains plus normalised subject (and `X-GM-THRID` where
  available).
- **FR-M.8 Keyword/flag mapping** is bijective and documented: `$seen↔\Seen`,
  `$draft↔\Draft`, `$flagged↔\Flagged`, `$answered↔\Answered`,
  `$deleted↔\Deleted`; other keywords map to IMAP keywords verbatim (server
  permitting). Unknown server flags surface as JMAP keywords; unknown JMAP
  keywords are stored if the server allows custom keywords, otherwise the write
  fails with `invalidArguments` naming the keyword — never silently dropped.
- **FR-M.9 `Email/set update`** applies `keywords/<kw>` (true/false/null) and
  `mailboxIds/<id>` (true/null) patches, **IMAP-first** (D-14): the local
  commit, modseq bump and SSE notification happen only after the server accepts.
  `null` — not `false` — removes, for both patch families.
- **FR-M.10 `Email/set destroy`** removes the message from all mailboxes
  (honouring UIDPLUS `UID EXPUNGE` where available) and creates a tombstone.
  Destroying an already-gone message is a no-op success, not an error.
- **FR-M.11 `Email/set create`** builds an RFC 5322 draft from the JMAP Email
  (headers, `textBody`/`bodyValues`, attachments referenced by `blobId`), `APPEND`s
  it to the Drafts mailbox with `\Draft` + `$draft`, and returns the new id.
- **FR-M.12 `Mailbox/set`** creates, renames and deletes IMAP folders
  (`CREATE`/`RENAME`/`DELETE`), rejects renames that would break hierarchy when
  the server disallows it, and refreshes SPECIAL-USE role detection.
- **FR-M.13 Mutation failure semantics**: any IMAP failure yields
  `notUpdated`/`notDestroyed`/`notCreated` with a `serverFail` (or more specific)
  SetError and **leaves local state unchanged**; a batch is not atomic across
  independent members (per RFC 8620) but each member is all-or-nothing. State
  strings returned after a mutation reflect that mutation (read-your-writes).
- **FR-M.14 `Identity/get`** returns at least one identity per account built from
  `accounts.address` (+ optional configured display name), with `mayDelete: false`
  and a stable id; it is only served when the submission capability is advertised.
- **FR-M.15 `EmailSubmission/set` (create)**: resolves `#reference` or direct
  `emailId`, builds the envelope from the identity and recipients, submits over
  SMTP (password or XOAUTH2), and on acceptance `APPEND`s the message to Sent with
  `\Seen`, applies the caller's `onSuccessUpdateEmail` patches locally, and returns
  `created` with `undoStatus: "final"` (and `sendAt` only when delayed send is
  requested — not in v0.1). On SMTP rejection: `notCreated` carrying the SMTP
  response, and **no** Sent copy. `onSuccessDestroyEmail` is rejected with
  `invalidProperties` (client-side fallback exists in jmap-tui).
- **FR-M.16 Blob upload** (`POST /upload/{account}`) accepts a body up to the
  configured cap, rejects unsupported/oversized content with RFC 8620 error
  codes, stores it, and returns `{accountId, blobId, type, size}`.
- **FR-M.17 Blob download** (`GET /download/{account}/{blobId}/{name}`) serves
  stored blobs with correct media type and `Content-Disposition`, expands all RFC
  8620 template placeholders (`{accountId}`, `{blobId}`, `{name}`, `{type}`), and
  rejects ids from another account.
- **FR-M.18 Gmail mail semantics**: label add/remove behaves as mailbox
  add/remove for clients; archive removes from INBOX only; messages remain listed
  in All Mail; Sent/Drafts use Gmail's special folders (`[Gmail]/Sent Mail`,
  `[Gmail]/Drafts`) discovered by SPECIAL-USE or well-known names; Gmail's
  eventual consistency must not cause FR-M.9/FR-M.10 to report failure when the
  server accepted the command.

## 5. FR-P — Contacts (CardDAV backend)

- **FR-P.1 Discovery**: locate CardDAV services via `/.well-known/carddav`
  (RFC 6764) or the configured URL, resolve the principal (RFC 5397), read
  `addressbook-home-set`, enumerate address books (displayname, sort order,
  description, privileges, `getctag`/`sync-token`). Discovery failures disable
  contacts (FR-P.3) rather than failing the account.
- **FR-P.2 Incremental sync** per address book: `REPORT sync-collection`
  (RFC 6578) with the stored sync-token; when unsupported, fall back to `getctag`
  change → full listing diff by ETag; fetch card bodies via
  `addressbook-multiget` or direct `GET`. Deletes, moves and ETag changes are all
  detected.
- **FR-P.3 Capability gating**: `urn:ietf:params:jmap:contacts` appears in the
  session only when at least one account has CardDAV configured *and* its first
  sync succeeded. Without it, contact methods return `unknownMethod`/`noAccounts`
  shaped errors rather than half-working data — and clients degrade.
- **FR-P.4 `AddressBook/get`** returns all books (or by id) with id, name,
  description, sortOrder, `mayRead`/`mayWrite`/`mayShare`/`mayDelete`, and state.
- **FR-P.5 `AddressBook/changes`** returns created/updated/destroyed books since
  `sinceState`, driven by the per-book sync-tokens.
- **FR-P.6 `ContactCard/get`** returns cards (all or by id) with the summary
  property set (`id`, `addressBookIds`, `kind`, `name`, plus addressing fields),
  and with full properties when requested — including `blobId` for media.
- **FR-P.7 `ContactCard/changes`** returns created/updated/destroyed cards since
  `sinceState` with `newState`, including changes applied by FR-P.2.
- **FR-P.8 `ContactCard/set` create**: builds a vCard from the JSContact object,
  `PUT`s it with `If-None-Match: *` into the target book, and returns the id
  (= vCard UID). A UID collision yields `exists`.
- **FR-P.9 `ContactCard/set` update**: content properties are immutable in JMAP
  (RFC 9610); `update` patches only the mutable set (keywords-style patches are
  limited here — v0.1 supports full-object replacement via create+destroy
  semantics used by clients, and `addressBookIds` membership moves). `PUT` uses
  `If-Match: <etag>`; a 412 triggers one refetch-retry, then `overwritten`.
- **FR-P.10 `ContactCard/set` destroy**: `DELETE` with `If-Match`; already-gone
  is success. `AddressBook/set` is out of scope, but attempting to delete a
  non-empty book through any path returns `addressBookHasContents`.
- **FR-P.11 Media/photos**: `ContactCard/set` with a `blobId` photo stores a
  recognised image type only (JPEG/PNG/WebP/GIF) within the size cap, written as
  vCard `PHOTO`; non-image types are rejected with `invalidProperties`.
- **FR-P.12 Conversion fidelity**: vCard 3.0 (RFC 2426) and 4.0 (RFC 6350) ⇄
  JSContact (RFC 9553) conversion follows RFC 9554 so no registered property is
  silently dropped; unknown/extension properties are preserved where the formats
  allow and round-trip in golden-pair tests.
- **FR-P.13 Groups and membership**: cards with `kind: "group"` and `members`
  map to/from vCard `KIND:group` + `MEMBER`; a card belongs to exactly one
  address book in v0.1 (CardDAV cannot express multi-book membership) — setting
  multiple `addressBookIds` keeps the first and reports the rest in
  `notUpdated`/`notCreated` with `invalidArguments`. Google's missing group
  support is a documented provider gap (README matrix), not an error path.

## 6. FR-X — Search and the local index

- **FR-X.1** An FTS5 index covers subject and address fields for every message,
  and body text as bodies hydrate; the index is rebuilt incrementally (never
  full-rebuild on restart) and stays consistent with tombstones.
- **FR-X.2** `text` queries match subject/from/to/body tokens; address tokens are
  indexed in a way that supports prefix/domain matching for `from`/`to` filters.
- **FR-X.3** Query evaluation covers FR-M.5's filter and sort sets with correct
  `total`, `position`, and stable ordering (ties broken deterministically by id).
- **FR-X.4** `collapseThreads` returns one exemplar per thread (the newest
  matching message), and `anchor`/`anchorOffset` addressing returns the requested
  window with the true starting position.
- **FR-X.5** **Search-driven backfill** (D-6): a query whose candidates include
  non-hydrated messages returns header-matching results immediately, then
  hydrates candidates in the background (`search.concurrency`), updating the
  index and bumping query state so clients pick up late matches via `/changes`
  or SSE. Result correctness never depends on hydration having finished.
- **FR-X.6** Backfill is bounded and controllable: `search.backfill` on/off,
  concurrency limit, resumable across restart, and it never starves interactive
  `Email/get` body requests (hydration queue is shared and prioritised).
- **FR-X.7** `queryState` changes exactly when results may have changed —
  including when hydration adds body tokens — and is stable otherwise.
- **FR-X.8** With `search.backfill = false` the bridge behaves identically except
  that body-only matches are absent; this degradation is documented in README
  and does not error.

## 7. FR-J — JMAP protocol contract

- **FR-J.1** `GET {base}/.well-known/jmap` returns the session resource with
  `apiUrl`, `accounts`, `primaryAccounts` (mail → this account),
  `capabilities`, `username`, and `state` — plus `uploadUrl`, `downloadUrl`
  and `eventSourceUrl` (RFC 8620 §2 URI templates) once those endpoints
  exist (FR-J.5). API responses echo the session's `state` as
  `sessionState` (RFC 8620 §3.4) — all on the configured origin.
- **FR-J.2** `POST {apiUrl}` accepts a batch of method calls and returns
  responses in order with matching `methodCallId`; unknown methods produce an
  `error` invocation of type `unknownMethod`, never a HTTP 500.
- **FR-J.3** Result references (RFC 8620 §3.7) resolve correctly, including `#ids`
  reference arguments (`Email/get` reading `/ids` from an `Email/query` result)
  and `/createdIds` where used.
- **FR-J.4** Set responses use the RFC 8620 §5.3 shapes exactly: absent
  collections are `null` (not `[]`/`{}`), `updated` is an **array of ids**, error
  maps carry per-id SetError objects with `type`/`properties`/`description`.
- **FR-J.5** Capabilities advertised match what is actually implemented and
  configured (mail always; submission when SMTP configured; contacts per FR-P.3;
  core always). `uploadUrl`/`downloadUrl`/`eventSourceUrl` are only advertised
  when the corresponding endpoint exists.
- **FR-J.6** Every endpoint (session, API, upload, download, eventsource, oauth)
  authenticates uniformly through FR-A.3; failures return 401 with
  `WWW-Authenticate`.
- **FR-J.7** State strings (`state` on gets, `sinceState` on `/changes`) are
  monotonic per type per account, never reused after reset, and `/changes`
  correctly reports created/updated/destroyed for Email, Mailbox, Thread,
  AddressBook and ContactCard.
- **FR-J.8** Server-Sent Events push (RFC 8620 §7.3): the eventsource endpoint
  expands `{types}`/`{closeafter}`/`{ping}` placeholders, emits `StateChange`
  events (`type`, `changed[account][Type].newState`) when modseq bumps, honours
  ping intervals and `closeafter`, and survives reverse-proxy buffering when
  configured per README.
- **FR-J.9** Error taxonomy: `invalidArguments`, `invalidProperties`,
  `unknownMethod`, `notFound`, `serverFail`, `tooLarge`, `unsupportedCapability`
  (or equivalent) with human-readable `description`s; HTTP status is 400 for
  malformed requests, 401 for auth, 413 for size caps.
- **FR-J.10** One-origin rule: session, API, upload, download and eventsource
  URLs share `base_url`'s origin; no endpoint ever redirects a client to a
  different origin (the client's credential policy would drop auth there).

## 8. FR-D — Deployment and operations

- **FR-D.1** A container image runs as a non-root user with `/config` and `/data`
  volumes, one entrypoint, and a documented `docker run` that works from the
  README on a clean host.
- **FR-D.2** Session URLs are derived from `base_url`, never from request `Host`
  headers, so operation behind a reverse proxy (Caddy/nginx/Ingress) needs no
  per-request magic; `X-Forwarded-Proto` is honoured only for building absolute
  redirect URLs where required.
- **FR-D.3** The OAuth callback URL is constructed from `base_url` and documented
  as the value to register with the provider; the bridge refuses to start an
  `oauth2` account whose `base_url` is cleartext non-loopback.
- **FR-D.4** `GET /healthz` (process liveness) and `GET /readyz` (every account
  completed a sync pass; false while any account is in auth failure) for probes.
- **FR-D.5** Graceful shutdown on SIGTERM: stop accepting HTTP, cancel IDLE and
  hydration, checkpoint SQLite, exit within a bounded grace period.
- **FR-D.6** Metrics in Prometheus text format (optional endpoint): sync lag per
  account, tier per account, hydration queue depth, method counts/errors, IMAP
  reconnects, SSE clients.
- **FR-D.7** Kubernetes artifacts: Deployment (with probes from FR-D.4), PVC for
  `data_dir`, Service, Ingress example with TLS, Secret examples for
  `JMAP_BRIDGE_SECRET_KEY` and credentials; `kubectl port-forward` documented as
  the cleartext-loopback dev path.
- **FR-D.8** Reverse-proxy documentation covers SSE requirements (buffering off,
  long read timeouts) and upload size limits, for both Caddy and nginx.
- **FR-D.9** Data directory layout is stable and documented (`bridge.db*`,
  `blobs/`, config path); forward-only schema migrations run at startup with a
  version check; backup = copy the directory while stopped (or SQLite backup
  API), documented.
- **FR-D.10** Twelve-factor configuration: every secret has an env-var or file
  alternative; the container runs with no interactive steps after first OAuth
  consent.
- **FR-D.11** Deployment documentation includes the Gmail path end to end: DNS +
  TLS + reverse proxy + `base_url` + Google OAuth client (redirect URI, enabled
  APIs, scopes) + contacts caveats (no groups).
- **FR-D.12** Structured logs (`log/slog`) with request ids and per-account sync
  events, level configurable, and log output containing no secrets or message
  bodies (verified by a redaction test).

## 9. Non-functional requirements

- **NFR-1 Performance (binding; revalidated at M5 per §11):** warm `Email/query`
  over a 100k-message folder answers in < 150 ms p95 on a developer laptop;
  session → first mailbox list in < 2 s warm; interactive body hydration
  ≥ 15 messages/s per account under normal RTT. Metadata footprint ≤ ~5 KB per
  message.
- **NFR-2 Live latency:** foreign changes visible to a connected client within
  2 s (IDLE healthy) and within `sync.interval` otherwise.
- **NFR-3 Storage policy:** the metadata cache is bounded and documented; body
  cache grows only with messages actually read (or within `prefetch_window`) and
  the growth policy is documented in README; no message content in logs/DB rows
  other than `email_content`/blobs.
- **NFR-4 Durability/consistency:** crash at any point leaves SQLite consistent
  (WAL) and sync resumable; no acknowledged JMAP mutation is ever lost; tombstones
  retained ≥ 30 days so `/changes` after a client's absence still works.
- **NFR-5 Security:** TLS terminated at the proxy (documented); tokens hashed and
  compared constant-time; credentials encrypted at rest when keyed; request body
  size caps (JSON ≤ 32 MiB, upload ≤ 64 MiB, confirmed); no path
  traversal via blob ids or download names; no server-side fetch of
  client-supplied URLs; auth failures indistinguishable across accounts.
- **NFR-6 Observability:** health, metrics and structured logs sufficient to
  diagnose a failed sync without a debugger (FR-D.4/6/12).
- **NFR-7 Compatibility:** works against jmap-tui's live suite (session quirks in
  PLAN §2.1) and passes the `JMAP-TestSuite` subset covering implemented methods
  by M7; degrades honestly where a backend lacks an extension.
- **NFR-8 Resource use:** idle RSS target < 150 MB for a 100k-message account
  with 10k hydrated bodies (verify at M5); no goroutine or connection leaks after
  account reconnect cycles (race detector clean in soak).
- **NFR-9 Operability:** restart resumes all work automatically; migrations are
  forward-only and never require manual SQL; no step in normal operation needs a
  shell inside the container.
- **NFR-10 Portability:** single static binary, no cgo requirement, amd64+arm64
  images.

## 10. Traceability — requirements → milestones

| Requirements | Milestone | Gate summary |
|---|---|---|
| FR-J.1–.6, FR-A.1–.4, FR-A.11–.12, FR-D.1 | M0 | jmap-tui browses fixture mail over loopback (FR-A.4 is config-level here: the first live IMAP/SMTP login lands with M1/M3) |
| FR-S.1–.9, FR-M.1–.8, FR-J.7–.8 | M1 | read-only live account; foreign flag change ≤ 2 s |
| FR-M.9–.13 | M2 | triage round-trips; second client sees it |
| FR-M.14–.17 | M3 | compose → send → Sent + delivered; attachments exact |
| FR-A.5–.10, FR-S.10, FR-S.12, FR-M.18 | M4 | live Gmail end-to-end |
| FR-X.1–.8, FR-S.11, NFR-1, NFR-2, NFR-8 | M5 | search correctness + 100k soak |
| FR-P.1–.13 | M6 | contacts live suite green against real CardDAV |
| FR-J.9–.10, FR-D.2–.12, NFR-3–.7, NFR-9–.10 | M7 | documented install works; conformance suite green |

*(If PLAN.md's traceability column ever disagrees with this table, this table
wins.)*

## 11. Open requirements

Resolved items are recorded as locked decisions in PLAN §1 (D-16 size caps,
D-17 `AddressBook/set` stays roadmap, D-18 body cache grow-only, D-19 perf
targets binding). Remaining open:

- NFR-1 and NFR-8 numbers are **binding as written** and revalidated by
  measurement at M5; any revision is a REQUIREMENTS edit in the same commit as
  the milestone sign-off.

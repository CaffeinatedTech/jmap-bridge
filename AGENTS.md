# AGENTS.md — rules for AI agents working on jmap-bridge

This repo is developed with AI agents. These rules exist so agent-generated work
doesn't erode the properties that make this project worth existing: **a cache that
never lies about the server** and **a JMAP surface that behaves like a real JMAP
server**. Humans: these apply to you too.

## Project in one paragraph

jmap-bridge is a containerised JMAP server (Go, MIT) that fronts IMAP/SMTP
accounts — and their CardDAV for contacts — so JMAP-only clients (jmap-tui among
them) can use ordinary mail providers. It keeps a hybrid local cache
(headers/flags/structure always, bodies lazy), writes IMAP-first, sends over
SMTP, and exposes JMAP over one HTTP origin (Docker/K8s, TLS at a reverse proxy).
**Scope is v0.1 = mail + send + contacts. JMAP Calendar is roadmap only — do not
start it.**

Authoritative docs: **REQUIREMENTS.md** = scope (source of truth), **PLAN.md** =
architecture/milestones/decisions, README.md = user-facing. If code and docs
disagree, flag it and fix the docs or the code — don't let them drift.

## Golden rules (violations = rejected work)

1. **IMAP first, then local commit.** Every mutation hits the real server before
   SQLite is touched. The cache never reports a change the server didn't accept,
   and never loses one it did. (D-14, FR-M.9)
2. **Bodies are lazy, headers are never.** No code path may download a full
   message just to render a list. No code path may skip header/flag sync. (D-2)
3. **One origin, loopback-or-https.** Session, API, upload, download and SSE are
   served from `base_url`'s origin; `auth.mode = "none"` starts only on loopback.
   Don't add cross-origin endpoints the client's credential policy would reject.
   (D-13, D-15)
4. **Capability honesty.** Advertise only what is configured and working: no
   CardDAV configured → no `urn:ietf:params:jmap:contacts`. Never fake a
   capability to make a client happy.
5. **State strings are the sync truth.** `/changes` + state strings must be
   monotonic and correct; clients (and our own SSE) rely on them. Never derive
   sync decisions from wall-clock timestamps.
6. **Secrets discipline.** Never log, print, or commit passwords, tokens,
   Authorization headers, OAuth codes. Credentials at rest are encrypted when a
   key is configured; test creds come from env only (below).
7. **No scope drift.** Features not in REQUIREMENTS.md need a REQUIREMENTS edit
   in the same commit. Milestone order per PLAN §12; don't start M(n+1) work
   inside an M(n) change.

## Stack & conventions

- Go (version pinned in `go.mod`). Module: `github.com/CaffeinatedTech/jmap-bridge`.
- IMAP: `github.com/emersion/go-imap/v2` **only inside `internal/imapdrv`** —
  it is alpha and lacks QRESYNC/`X-GM-LABELS`, which is exactly why the driver
  interface exists (D-7). Never let its types escape that package.
- SMTP: `github.com/emersion/go-smtp` (or stdlib `net/smtp` if sufficient).
- CardDAV: `github.com/emersion/go-webdav/carddav` where it fits; hand-roll
  `sync-collection`/multiget inside `internal/dav` if the library falls short.
- DB: SQLite via `modernc.org/sqlite` (no cgo) or `mattn/go-sqlite3` — pick one,
  don't mix. WAL mode, forward-only schema migrations in `internal/store`.
- Config: TOML (`github.com/BurntSushi/toml`), strict decoding — unknown keys are
  errors, like jmap-tui's config.
- Style: standard Go. `gofumpt`, `golangci-lint` clean, errors wrapped with `%w`
  + context, `context.Context` on every network call, no global mutable state,
  no `init()` side effects.
- Comments: explain *why*, not *what*; every exported symbol has a doc comment.

## Commands (gates — run before every commit)

```sh
go build ./...            # build
go vet ./...              # vet
gofumpt -l -w .           # format
golangci-lint run         # lint
go test ./... -race       # unit + fixture-server tests
go run ./cmd/jmap-bridge --version   # smoke
```

There is **no CI** (D-10): these gates are local and manual, and they are the
release bar. Live integration tests run against real providers via
`JMAP_BRIDGE_TEST_*` env creds, agent-run only; they skip when the env is unset.

**Docker is not a gate.** The dev machine has no docker-daemon access (sudo
needs a password), so development and testing always run the native binary
(`go run ./cmd/jmap-bridge --config ./dev/config.toml`). The FR-D.1 image is
verified on demand on a docker-capable host — mandatory before M7 sign-off,
never assumed done.

## Testing rules

- Protocol/sync behaviour ships with tests against an in-process fixture server
  (`test/fixtureimap`, `test/fixturecarddav`) — no real-network unit tests, ever.
- The fixture IMAP server must be able to *impersonate the tiers*: QRESYNC-capable,
  CONDSTORE-only (Gmail-shaped), and bare (baseline) — tier selection and
  fallbacks are the highest-risk code in the repo.
- Conversion (`internal/convert`) is golden-pair tested: RFC 5322 → JMAP Email and
  vCard → JSContact fixtures, including malformed/degenerate inputs.
- Live tests read creds from `JMAP_BRIDGE_TEST_*` (IMAP/SMTP/CardDAV/OAuth).
  **Unset env ⇒ tests skip. Never hardcode, never commit, never echo.**

## Cross-client gate (jmap-tui)

Run the bridge natively and point jmap-tui's suite at it — this is how every
PLAN §12 gate is demonstrated, no Docker involved:

```sh
# jmap-bridge repo: fixture account on loopback
go build -o /tmp/jmap-bridge ./cmd/jmap-bridge
/tmp/jmap-bridge --config ./dev/config.toml &

# jmap-tui repo: the M0 gate test (later milestones: TestLiveN… suites)
JMAP_TUI_TEST_URL=http://127.0.0.1:8080/personal \
JMAP_TUI_TEST_USER=any \
JMAP_TUI_TEST_PASSWORD=<token from dev/config.toml> \
  go test ./internal/jmapclient/ -run TestLiveSessionAndMailboxes -v

go run ./cmd/jmap-tui smoke --config /tmp/no-such-config.toml \
  --url http://127.0.0.1:8080/personal --user any \
  --password-file <chmod-600 file containing the token>
```

The dev token lives in `dev/config.toml` (loopback throwaway only): read it
from there, never echo it into logs or commits. Unset env ⇒ tests skip.

## Live provider rules of engagement

When testing against a real mailbox the user provides:

- Work only inside designated test folders (e.g. `jmap-bridge-test/…`); create them
  if absent, clean up after.
- Allowed: reading anything, CRUD inside test folders, sending between test
  addresses (a handful per run).
- Not allowed without explicit instruction: touching Inbox/Sent/Drafts/Trash of the
  real account, bulk operations, provider account-setting changes.
- Rate courtesy: batched FETCH, no tight loops, respect provider backoff —
  Gmail throttling looks like a bug but isn't.

## Repo chores expected of every agent

- Update PLAN.md milestone table and risk register when you land or verify a tier,
  capability, or provider.
- REQUIREMENTS.md changes accompany scope changes (same commit).
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`), ≤ 50-char subject.
- Don't commit: `.env*`, `data/`, `*.db*`, blobs, binaries, or credentials.
- Don't amend/rebase shared history; don't force-push.

## Relationship to jmap-tui

- jmap-tui is a **sibling project and the primary client**, not a dependency.
  Never import its code; its `test/mockjmap` wire quirks are *requirements*
  (PLAN §2.1) and may be copied as fixtures with attribution.
- Its live suite pointed at this bridge is our best integration signal.
- **Never commit to jmap-tui** from this repo's work: patches found while
  gating (e.g. client-side bugs) stay uncommitted until the user decides.

## When you're unsure

- Scope question → REQUIREMENTS.md; design question → PLAN.md; both silent →
  **ask the user**, don't improvise. The user interviews agents for critical
  decisions (auth, storage, protocol choices, provider support). Err toward
  asking over assuming.

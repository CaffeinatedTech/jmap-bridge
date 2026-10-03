# Security Hardening Plan — JMAP endpoints

Status: edge layer landed 2026-10-02; OAuth edge rate limit attached
2026-10-03; app-side A1–A8 landed. F10 (opaque server errors) deferred — see
§10.
Owner: (assign)
Related: `REQUIREMENTS.md` NFR-5 (security), `PLAN.md` §9 (auth), §11
(observability), §13 (risk register). Golden rules 4, 7 and 8 in `AGENTS.md`
apply: this work needs REQUIREMENTS edits and a PLAN decision/risk-register
update in the same commit, and milestone status changes where a gate is
demonstrated.

## 1. Goal

Close the gaps found in the JMAP endpoint review, in priority order, and make
the advertised limits true. Two layers of defence:

1. **Edge (Traefik)** — volumetric abuse: a stricter request rate for the
   unauthenticated OAuth bootstrap only, applied before the app. Shape
   agnostic. Authenticated JMAP traffic is deliberately not edge-limited
   (2026-10-03).
2. **Application (Go)** — credential-aware protection: per-account failed-auth
   lockout, token entropy, concurrency the session advertises, SSE caps. The
   edge cannot see whether a 401 was a wrong password, so this half must live
   in the app.

Non-goals: WAF, bot management, anomaly detection, per-user billing-style
quotas. Keep the app dependency-light (stdlib token bucket preferred over
`golang.org/x/time/rate`; decide in D-22).

## 2. Findings → workstreams

Legend: P0 = ship before any public exposure; P1 = same release, after P0;
P2 = hardening, opportunistic.

| # | Finding | Location | Priority |
|---|---|---|---|
| F1 | No brute-force protection on the only credential | `internal/httpapi/server.go:116` | P0 |
| F2 | No minimum client-token length/entropy | `internal/config/validate.go:149` | P0 |
| F3 | `maxConcurrentRequests`/`maxConcurrentUpload` advertised but unenforced | `internal/httpapi/server.go:225` | P0 |
| F4 | Unbounded SSE subscribers | `internal/push/hub.go:39`, `internal/httpapi/eventsource.go:78` | P0 |
| F5 | HTTP server missing `ReadTimeout`/`IdleTimeout`/`MaxHeaderBytes` | `cmd/jmap-bridge/main.go:153` | P1 |
| F6 | Account-enumeration timing oracle (short-circuit before constant-time compare) | `internal/httpapi/server.go:122` | P1 |
| F7 | `Email/query` unbounded result window | `internal/jmapapi/methods.go:336`, `internal/store/query.go` | P1 |
| F8 | `eventsource` `ping` overflow can panic | `internal/httpapi/eventsource.go:100` | P1 |
| F9 | `/oauth/{account}/start` unauthenticated and unthrottled | `internal/httpapi/oauth.go:46` | P2 |
| F10 | Internal error text echoed to clients | `internal/jmapapi/methods.go:709` | P2 |
| F11 | Missing `nosniff`/CSP/Referrer-Policy | `internal/httpapi/server.go:375` | P2 |
| F12 | `atoi` JSON-pointer overflow | `internal/jmapapi/dispatch.go:563` | P2 |

## 3. Workstream A — application controls (P0/P1)

### A1. Rate-limit package (F1, F4, F7, F9 — shared machinery)

Add `internal/ratelimit` (stdlib only):

- A keyed token bucket: `map[string]*entry` guarded by a mutex; each entry
  holds `tokens float64`, `last time.Time`, `failures int`,
  `blockedUntil time.Time`.
- API: `Allow(key) bool`, `Fail(key)`, `Succeed(key)`, `Block(key, d)`.
- Bounded memory: lazy sweep of stale entries on write plus a max key count
  (evict oldest) — the limiter must never become the DoS.
- Config-driven, no globals; one instance per server, injected into
  `httpapi.New`.

Suggested `[rate]` config block (strict decoding, so every key must be
documented and validated). Defaults must be safe when the block is absent:

```toml
[rate]
enabled            = true
trusted_proxies    = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
# failed client auth
auth_failures      = 10       # failures per window before lockout
auth_window        = "5m"
auth_block         = "15m"
# general request budget per source
requests_per_second = 20
burst              = 40
# concurrency (mirrors the core capability; see A3)
max_concurrent_requests = 8
max_concurrent_uploads  = 4
# SSE
max_eventsource_per_account = 8
max_eventsource_total       = 128
```

**Client-source derivation (critical behind a proxy).** `r.RemoteAddr` is the
Traefik pod, not the user. Derive the source IP from the rightmost hop of
`X-Forwarded-For` that is *not* in `trusted_proxies`; never trust XFF for the
leftmost value. If `trusted_proxies` is empty, fall back to `RemoteAddr` and
document that per-source limiting is then effectively global.

### A2. Failed-auth lockout (F1)

In `authorize` (`internal/httpapi/server.go:116`):

1. Compute the source key `(ip, account)`.
2. If the key is currently blocked, return `429` + `Retry-After` with the
   same JSON/problem shape as other refusals, **before** credential work.
3. On any auth failure (unknown account, missing header, wrong token) call
   `Fail(key)` and return the existing `401` + `WWW-Authenticate`
   byte-for-byte unchanged.
4. On success call `Succeed(key)`.

Do not change the `401` body or add failure detail — FR-A.11/.12 keeps
failures indistinguishable. The `429` must not reveal whether the account
exists: once a source is blocked, apply it to all accounts from that source.

### A3. Token entropy (F2)

In `Config.validate`, require token mode tokens to be at least 32 bytes of
entropy (e.g. reject < 24 chars and obvious low-entropy values), naming the
key but never the value (FR-A.2). Update `secret.example.yaml` guidance and
the README token-generation snippet. This is a REQUIREMENTS edit (see §6).

### A4. Enforce advertised concurrency (F3) and SSE caps (F4)

- Wrap the mux in a semaphore middleware: N = `max_concurrent_requests`.
  Over the limit → `429` (or JMAP `urn:ietf:params:jmap:error:limit` where a
  JMAP body is appropriate).
- Upload path takes from a separate, smaller semaphore of
  `max_concurrent_uploads`.
- SSE: acquire `max_eventsource_per_account` (a per-account counter) and
  `max_eventsource_total` before `hub.Subscribe`; over → `429` + `Retry-After`
  and return without subscribing. Release in the existing `defer`.
- The numbers enforced must equal the numbers advertised in the session
  (`server.go:223`), or the capability is dishonest (golden rule 4). Prefer
  deriving both from one source so they cannot drift.

### A5. Server timeouts (F5)

In `cmd/jmap-bridge/main.go`:

```go
srv := &http.Server{
    Addr:              cfg.Listen,
    Handler:           handler,
    ReadHeaderTimeout: 10 * time.Second,
    ReadTimeout:       30 * time.Second, // whole request read; safe for SSE (no request body)
    IdleTimeout:       120 * time.Second,
    MaxHeaderBytes:    1 << 20,
    // WriteTimeout stays 0: it would kill long-lived SSE responses.
}
```

If a per-route write deadline is wanted later, use `http.ResponseController`
inside non-SSE handlers rather than a global `WriteTimeout`.

### A6. Timing side channel (F6)

In `authorize`, evaluate the constant-time `Verify` unconditionally (unknown
account compares against the zero digest), then combine:

```go
user, pass, ok := r.BasicAuth()
verified := s.tokens.Verify(accountID, pass) // always runs
if acct == nil || !ok || user == "" || !verified { ... 401 ... }
```

Add a test that an unknown account and a known account with a wrong token take
the same code path.

### A7. Query result cap (F7)

Add a server-side maximum query window (e.g. `maxObjectsInGet`-derived, or a
new `maxQueryResults`, mirrored in the core capability). Reject or clamp
`limit` above it. Validate `position`/`anchorOffset` as non-negative and
bounded. Document the cap in the session capability so clients can page.

### A8. eventsource input validation (F8)

Clamp `ping` to `0` or `[min, max]` (e.g. 10–300s) before constructing the
ticker; reject anything that would overflow `time.Duration`. Guard
`time.NewTicker` against a non-positive duration defensively.

## 4. P2 hardening

- **F9:** tricky case for A1's limiter on `/oauth/` (unauthenticated), e.g. 1
  start per 10s per source, 10/min; protects the `maxPending` map from
  eviction spam.
- **F10:** keep `serverFail` opaque to the client; log the wrapped error
  server-side with a correlation id, return a generic description.
- **F11:** add `X-Content-Type-Options: nosniff`, a strict
  `Content-Security-Policy` and `Referrer-Policy` in `writeJSON`/download and
  on the OAuth pages.
- **F12:** bound the digit count in `atoi`, or use `strconv.Atoi` and reject
  out-of-range.

## 5. Edge layer — Traefik (k8s)

Traefik covers this natively with CRD middlewares: `RateLimit` (requests per
source over time) and `InFlightReq` (concurrent requests per source). No plugin
needed on Traefik v2.5+/v3.

**Only the unauthenticated OAuth bootstrap is limited at the edge.** An edge
limit cannot see the client token, so on the authenticated JMAP surface it
punishes exactly the clients that did authenticate: marking a mailbox read, a
first `/changes` sync or a reconnect storm is legitimately many requests. Both
the general `jmap-ratelimit` and the `jmap-inflight` concurrency cap were
therefore removed (2026-10-03); the main Ingress now carries only the HTTP ->
HTTPS redirect. The app's credential-aware lockout (A1/A2) is unaffected and
still owns abuse of authenticated traffic.

`deploy/k8s/middleware-ratelimit.yaml` (v3 API group; on v2 use
`traefik.containo.us/v1alpha1`):

```yaml
# Stricter budget for the unauthenticated OAuth bootstrap.
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: jmap-oauth-ratelimit
  namespace: jmap-bridge
spec:
  rateLimit:
    average: 1
    burst: 5
    period: 10s
    sourceCriterion:
      requestHeaderName: CF-Connecting-IP
```

The main Ingress (`deploy/k8s/ingress.yaml`) attaches only the redirect:

```yaml
annotations:
  traefik.ingress.kubernetes.io/router.middlewares: >-
    default-default-redirect-https@kubernetescrd
```

The `/oauth/` budget is a second Ingress for the same host with `path: /oauth`
(Traefik routes by longest matching rule) carrying
`jmap-bridge-jmap-oauth-ratelimit@kubernetescrd` — see
`deploy/k8s/ingress-oauth.yaml`. Add the middleware and Ingress files to
`deploy/k8s/kustomization.yaml` under `resources`.

### Traefik caveats (must be in the plan, not just the manifest)

- **Volumetric only.** Traefik cannot tell a wrong token from a valid one, so
  it cannot rate-limit *failed logins*. A1/A2 stay app-level. (fail2ban-style
  log scraping is an alternative but couples to log format.)
- **Source identity.** Default grouping is the connection remote address. If
  anything terminates before Traefik (cloud LB, another proxy), set
  `ipStrategy.excludedIPs`/`depth` to the real client, or all traffic groups
  as one source and the limit becomes global. This is the same trust decision
  as `trusted_proxies` in A1 — document it once.
- **SSE.** EventSource streams are long-lived; the edge no longer caps
  concurrency or rate-limits them (2026-10-03), so reconnect storms (jmap-tui's
  60s poll fallback) cannot be throttled. The app-level SSE caps (A4) still
  bound them.
- **Buffering/timeouts.** The ingress comment already notes Traefik streams
  SSE without buffering.
- **Traefik version.** k3s ships Traefik v2 in some releases, v3 in others.
  Confirm `kubectl -n kube-system get deploy traefik -o jsonpath='{.spec.template.spec.containers[0].image}'`
  and pick the matching API group.

## 6. Documentation and process changes (golden rules 7 & 8)

These are mandatory companion edits, same commit as the code:

- **REQUIREMENTS.md:**
  - Extend FR-A.3 with failed-auth throttling and a minimum token-entropy
    requirement (F1, F2).
  - Extend FR-A.5/F (or add an FR) covering per-source limiting of the OAuth
    bootstrap (F9).
  - Extend NFR-5 with server timeouts, enforced concurrency, SSE caps, and
    query-window caps (F3–F8).
  - Add a traceability row in §10 mapping the new FRs to the milestone that
    demonstrates them.
- **PLAN.md:**
  - Add decision **D-22**: rate limiting is two-layer (Traefik at the edge for
    volume, `internal/ratelimit` in-process for auth failures); source
    identity derived from trusted proxies; limits advertised == limits
    enforced.
  - Add risk-register rows: brute-force on client tokens (mitigated by A1/A2
    once landed), and edge limit misconfiguration treating all traffic as one
    source (mitigated by `trusted_proxies`/`excludedIPs` docs).
  - Update §11 if request ids/metrics are added alongside (they are currently
    promised but absent).
  - Set the M7 status appropriately only when its gate is demonstrated; do not
    mark hardening "done" on code alone.

## 7. Test plan

Unit / fixture tests (no real network), per AGENTS.md:

- `internal/ratelimit`: bucket math, window/block boundaries, memory bound,
  concurrent access under `-race`.
- `internal/httpapi`:
  - N failed auths → `429` with `Retry-After`; corrected credentials succeed
    after the block elapses; `401` body unchanged.
  - `auth.mode="none"` path unaffected.
  - unknown vs known-account timing: assert both call `Verify` (e.g. a
    counting/`testing` seam), not a wall-clock assertion.
  - concurrency: `max_concurrent_requests+1` in-flight → last gets `429`; same
    for uploads; SSE cap per account and total, with release on disconnect.
  - `eventsource`: `ping` values at the clamp boundaries and an overflow-sized
    value answer without panicking.
  - `Email/query`: `limit` above the cap is clamped/rejected; response shape
    stays valid.
- `internal/config`: token entropy rejects weak tokens but never prints the
  value; `[rate]` defaults and validation.
- Traefik: not unit-testable here. Verify on the docker-capable host (or the
  user's cluster) that a burst above the OAuth `average`/`burst` returns `429`,
  and that authenticated traffic is not throttled.

Gates before commit (AGENTS.md): `go build ./...`, `go vet ./...`,
`gofumpt -l -w .`, `golangci-lint run`, `go test ./... -race`,
`go run ./cmd/jmap-bridge --version`. Local `SECURITY-PLAN.md`, `REQUIREMENTS.md`
and `PLAN.md` changes ship together.

## 8. Rollout order

1. A5 (timeouts) — one-line, no behaviour risk.
2. A6 (timing), A8 (ping clamp), A3 (token entropy) — small, self-contained.
3. A1 + A2 (limiter + failed-auth lockout), A9/F9 OAuth throttle.
4. A3 advertised-vs-enforced concurrency + A4 SSE caps.
5. A7 query cap.
6. F10/F11/F12 hardening.
7. Traefik middlewares + kustomization + README/`deploy/README.md` docs.
8. REQUIREMENTS/PLAN updates in the same commits as the corresponding code.

## 9. Open questions

- Exact default numbers for `[rate]` (above are proposals; the user should
  confirm against expected client behaviour and NFR-1).
- Minimum token entropy threshold (proposed 32 bytes / ≥ 24 chars).
- Whether to add `golang.org/x/time/rate` or keep the stdlib bucket (D-22).
- Whether the edge `RateLimit` should be enabled by default in the shipped
  kustomize stack, or left commented as an opt-in. **Resolved 2026-10-03:**
  the general limit was removed (it punished authenticated bulk operations
  it could not see); only the OAuth bootstrap is edge-rate-limited in the
  shipped stack.

## 10. Progress (2026-10-02)

Landed:

- **A5** server `ReadTimeout`/`IdleTimeout`/`MaxHeaderBytes` (`cmd/jmap-bridge`).
- **A6** `authorize` always runs the constant-time compare, removing the
  account-existence timing oracle.
- **A1** `internal/ratelimit`: bounded per-source failed-auth lockout with
  trusted-proxy-aware `ClientIP` (unit-tested).
- **A2** failed-auth lockout wired into `authorize` (`429` + `Retry-After`,
  `401` shape unchanged).
- **A3** 24-character minimum client-token entropy (FR-A.3; test fixtures and
  `dev/*.toml` migrated).
- **A4** request/upload semaphores and per-account/total EventSource caps;
  advertised `maxConcurrentRequests`/`maxConcurrentUpload` now equal what is
  enforced.
- **A7** `/query` window capped at `maxQueryResults`.
- **A8** EventSource `ping` clamped against `time.Duration` overflow.
- **F11** `nosniff`/CSP/`X-Frame-Options`/`Referrer-Policy` on every response.
- **F12** JSON-pointer index length-bounded.
- **Traefik** OAuth `RateLimit` middleware keyed on `CF-Connecting-IP`;
  live-verified (burst → `429`, `retry-after`).
- **F9** OAuth `/start` throttle — `jmap-oauth-ratelimit` (1/10s, burst 5)
  attached to a dedicated `/oauth` Ingress (`deploy/k8s/ingress-oauth.yaml`),
  2026-10-03. The general `jmap-ratelimit` and `jmap-inflight` middlewares were
  removed the same day: authenticated JMAP traffic is not edge-limited (see
  §5).

Deferred (P2, not blocking):

- **F10** opaque `serverFail` responses — the client still sees the wrapped
  server error string; logging the detail behind a correlation id is a
  follow-up.
- App-side general request-rate limiting is intentionally absent (D-22, amended
  2026-10-03): the edge rate-limits only the OAuth bootstrap, and the
  credential-aware lockout (A1/A2) covers the authenticated surface.

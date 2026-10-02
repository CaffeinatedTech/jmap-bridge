// Package httpapi serves the JMAP HTTP surface (PLAN §3): the session
// resource and the API endpoint, both under the /{account}/ path prefix
// (D-13), both on the configured base_url origin (FR-D.2, FR-J.10), and
// both behind the same Basic authentication on every endpoint (FR-A.3,
// FR-J.6).
package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
	"github.com/CaffeinatedTech/jmap-bridge/internal/ratelimit"
)

// Request size caps (NFR-5, locked as D-16). maxSizeRequest mirrors the
// value advertised in the core capability object.
const (
	maxSizeRequest = 32 << 20 // 32 MiB
	maxSizeUpload  = 64 << 20 // 64 MiB: the session cap and the upload endpoint's limit (NFR-5, D-16)
)

// Server is the bridge's HTTP handler.
type Server struct {
	cfg      *config.Config
	tokens   *auth.Tokens
	store    jmapapi.Store
	backends map[string]jmapapi.Backend
	hub      *push.Hub
	jmap     *jmapapi.Handler
	log      *slog.Logger
	mux      *http.ServeMux
	// oauth maps an account id to its OAuth2 client; accounts without
	// one have no /oauth/ endpoints at all (FR-A.5).
	oauth map[string]*oauth.Manager
	// kick nudges an account's sync engine after credentials land.
	kick func(account string)
	// contactsReady reports whether an account's first CardDAV sync has
	// succeeded — the second half of the contacts capability gate
	// (FR-P.3: configured *and* working, never advertised on hope).
	contactsReady func(account string) bool

	// limit is the in-process failed-auth lockout (NFR-5, A2); nil when
	// rate limiting is disabled.
	limit *ratelimit.Limiter
	// reqSem/uploadSem bound concurrent work; nil means unbounded.
	reqSem    chan struct{}
	uploadSem chan struct{}
	// maxReq/maxUpload are what the session advertises; enforcement uses
	// the same numbers so capability and behaviour cannot drift.
	maxReq, maxUpload int

	// SSE connection caps (A4). sseMu guards the live counters.
	sseMu         sync.Mutex
	ssePerAcct    map[string]int
	sseTotal      int
	maxSSEPerAcct int
	maxSSETotal   int
}

// New wires a Server: routing, auth, dispatch and push. store serves
// every configured account; backends maps an account id to the object
// that mutates it (nil for a cache-only account, which then refuses
// every write); hub is the change signal source for the eventsource
// endpoint (FR-J.8); oauth carries the per-account OAuth2 clients
// (FR-A.5) and kick is called after a successful consent.
func New(cfg *config.Config, tokens *auth.Tokens, store jmapapi.Store,
	backends map[string]jmapapi.Backend, hub *push.Hub, log *slog.Logger,
	oauth map[string]*oauth.Manager, kick func(account string),
	contactsReady func(account string) bool,
) *Server {
	s := &Server{
		cfg:           cfg,
		tokens:        tokens,
		store:         store,
		backends:      backends,
		hub:           hub,
		jmap:          jmapapi.NewHandler(store),
		log:           log,
		mux:           http.NewServeMux(),
		oauth:         oauth,
		kick:          kick,
		contactsReady: contactsReady,
	}
	if s.log == nil {
		// Tests build Servers without logging; the error paths must not
		// segfault for it.
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s.applyRateLimits(cfg.Rate)
	s.mux.HandleFunc("GET /{account}/.well-known/jmap", s.handleSession)
	s.mux.HandleFunc("POST /{account}/jmap", s.handleAPI)
	s.mux.HandleFunc("GET /{account}/eventsource/", s.handleEventSource)
	// Subtree pattern: the session's {accountId} expands into the
	// path prefix, and anything after it is the client's business
	// (RFC 8620 §6.1 only requires the template to carry accountId).
	s.mux.HandleFunc("POST /{account}/upload/", s.handleUpload)
	s.mux.HandleFunc("GET /{account}/download/{blobId}/{name}", s.handleDownload)
	// FR-D.13: the OAuth consent screen's public home page and privacy
	// policy. Both are on this origin and neither carries account data.
	s.mux.HandleFunc("GET /{$}", s.handleHome)
	s.mux.HandleFunc("GET /privacy", s.handlePrivacy)
	return s
}

// ServeHTTP implements http.Handler. The OAuth2 bootstrap subtree is
// dispatched before the mux (FR-A.5): its "/oauth/…" paths collide
// structurally with the "/{account}/…" wildcards above — ServeMux would
// refuse to register both — and it is deliberately NOT behind the client
// token: the operator reaches it from a browser before any client is
// configured, and the flow's own state is the credential (FR-A.6).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Defence-in-depth response headers (NFR-5). The pages carry no
	// scripts or styles, so a deny-all CSP costs nothing and stops a
	// download or content-sniffing quirk from turning into script on
	// this origin.
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/oauth/") {
		s.handleOAuth(w, r)
		return
	}
	// EventSource streams are long-lived and governed by their own caps;
	// every other request takes a slot so the advertised
	// maxConcurrentRequests is real (golden rule 4, A4).
	if !isEventSource(r) {
		if !s.acquire(s.reqSem) {
			s.tooManyRequests(w, "maxConcurrentRequests")
			return
		}
		defer s.release(s.reqSem)
	}
	s.mux.ServeHTTP(w, r)
}

// applyRateLimits builds the failed-auth limiter and concurrency gates
// from configuration (NFR-5). Disabled leaves them nil (unbounded),
// which is an explicit operator choice.
func (s *Server) applyRateLimits(rate config.Rate) {
	s.maxReq, s.maxUpload = 8, 4
	if rate.MaxConcurrentRequests > 0 {
		s.maxReq = rate.MaxConcurrentRequests
	}
	if rate.MaxConcurrentUploads > 0 {
		s.maxUpload = rate.MaxConcurrentUploads
	}
	if !rate.Enabled {
		return
	}
	s.limit = ratelimit.New(ratelimit.Config{
		AuthFailures:   rate.AuthFailures,
		AuthWindow:     rate.AuthWindow.Std(),
		AuthBlock:      rate.AuthBlock.Std(),
		ClientIPHeader: rate.ClientIPHeader,
		TrustedProxies: rate.TrustedProxies,
	})
	s.reqSem = make(chan struct{}, s.maxReq)
	s.uploadSem = make(chan struct{}, s.maxUpload)
	s.maxSSEPerAcct = rate.MaxEventsourcePerAccount
	s.maxSSETotal = rate.MaxEventsourceTotal
	s.ssePerAcct = map[string]int{}
}

// isEventSource reports whether r targets the SSE endpoint, which is
// exempt from the request concurrency gate.
func isEventSource(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/eventsource/")
}

// acquire takes a slot from sem without blocking; nil is unbounded.
func (s *Server) acquire(sem chan struct{}) bool {
	if sem == nil {
		return true
	}
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a slot taken by acquire.
func (s *Server) release(sem chan struct{}) {
	if sem != nil {
		<-sem
	}
}

// acquireSSE reserves an EventSource slot for account, refusing when the
// per-account or total cap is reached (A4).
func (s *Server) acquireSSE(account string) bool {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	if s.maxSSETotal > 0 && s.sseTotal >= s.maxSSETotal {
		return false
	}
	if s.maxSSEPerAcct > 0 && s.ssePerAcct[account] >= s.maxSSEPerAcct {
		return false
	}
	s.ssePerAcct[account]++
	s.sseTotal++
	return true
}

// releaseSSE frees an EventSource slot.
func (s *Server) releaseSSE(account string) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	if s.ssePerAcct[account] > 0 {
		s.ssePerAcct[account]--
		if s.ssePerAcct[account] == 0 {
			delete(s.ssePerAcct, account)
		}
	}
	if s.sseTotal > 0 {
		s.sseTotal--
	}
}

// tooManyRequests answers a refused request with Retry-After and the
// registered JMAP limit problem type (RFC 8620 §3.6.1).
func (s *Server) tooManyRequests(w http.ResponseWriter, limit string) {
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"type":   "urn:ietf:params:jmap:error:limit",
		"status": http.StatusTooManyRequests,
		"limit":  limit,
		"detail": "too many requests",
	}, nil)
}

// authorize resolves the path account and checks the request
// credentials. Every failure in token mode collapses to the same 401
// with WWW-Authenticate — unknown account, missing header, empty
// username, or wrong token are indistinguishable (FR-A.11, FR-A.12,
// FR-J.6). auth.mode = "none" serves without credentials and is only
// reachable on loopback (FR-A.3), enforced when the config loads.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, accountID string) *config.Account {
	acct := s.cfg.Account(accountID)
	if s.cfg.Auth.Mode == "none" {
		return acct // nil → caller 404s: no credentials are in play to hide behind
	}
	user, pass, ok := r.BasicAuth()
	// Verify runs unconditionally: an unknown account must cost the same
	// constant-time compare as a known one, or the response time leaks
	// which account ids exist (FR-A.11, FR-A.12).
	verified := s.tokens.Verify(accountID, pass)

	key := ""
	if s.limit != nil {
		key = "auth:" + s.limit.ClientIP(r) + ":" + accountID
		if s.limit.Blocked(key) {
			s.tooManyRequests(w, "authFailures")
			return nil
		}
	}
	if acct == nil || !ok || user == "" || !verified {
		if s.limit != nil {
			s.limit.Fail(key)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap-bridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	if s.limit != nil {
		s.limit.Succeed(key)
	}
	return acct
}

// username extracts the Basic username for session display; in none mode
// there is none, so the account address stands in.
func username(r *http.Request, acct *config.Account) string {
	if user, _, ok := r.BasicAuth(); ok && user != "" {
		return user
	}
	return acct.Address
}

// sessionState derives the session's state string (RFC 8620 §2): a
// short hash over every property of the session object, so any change to
// them changes it. Deterministic across restarts, so clients do not
// refetch the session needlessly.
func (s *Server) sessionState(acct *config.Account, user string) string {
	h := sha256.New()
	for _, part := range []string{
		s.cfg.BaseURL,
		acct.ID,
		acct.Name,
		acct.Address,
		user,
		strings.Join(s.accountCapabilities(acct), ","),
	} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// handleSession serves GET /{account}/.well-known/jmap (FR-J.1). URLs
// are built from base_url, never the request Host header (FR-D.2), and
// every endpoint advertised here exists — uploadUrl and downloadUrl
// since M3, the submission capability only for an account with SMTP
// behind it (FR-J.5, FR-M.14).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	acct := s.authorize(w, r, r.PathValue("account"))
	if acct == nil {
		if s.cfg.Auth.Mode == "none" {
			http.NotFound(w, r)
		}
		return
	}
	user := username(r, acct)
	name := acct.Name
	if name == "" {
		name = acct.Address
	}
	if name == "" {
		name = acct.ID
	}

	base := strings.TrimRight(s.cfg.BaseURL, "/")
	// RFC 8621 §1.3.2: the session-level capability is an empty object,
	// the account-level one carries maxDelayedSend (0 — v0.1 does not
	// do delayed send). It appears only when this account has an SMTP
	// server to submit through (FR-J.5, FR-M.14).
	capabilities := map[string]any{
		// RFC 8621 §1.3.1: the session-level mail capability is an
		// empty object; the limits live on the account.
		"urn:ietf:params:jmap:mail": map[string]any{},
	}
	accountCapabilities := map[string]any{
		"urn:ietf:params:jmap:mail": map[string]any{
			"maxMailboxesPerEmail":       100,
			"maxMailboxDepth":            20,
			"maxSizeMailboxName":         255,
			"maxSizeAttachmentsPerEmail": maxSizeUpload,
			"emailQuerySortOptions": []string{
				"receivedAt", "subject", "from", "size", "hasAttachment",
			},
			// Mailbox/set exists as of M2 (FR-M.12), so top-level
			// creation may be advertised honestly (FR-J.5).
			"mayCreateTopLevelMailbox": true,
		},
	}
	if canSend(acct) {
		capabilities[jmapapi.SubmissionURN] = map[string]any{}
		accountCapabilities[jmapapi.SubmissionURN] = map[string]any{"maxDelayedSend": 0}
	}
	// FR-P.3: the contacts capability appears only when CardDAV is
	// configured *and* the first sync succeeded; until then the
	// primaryAccounts entry and every contacts method are absent, and
	// clients degrade exactly as they do for an unknown server.
	if s.canContacts(acct) {
		capabilities[jmapapi.ContactURN] = map[string]any{}
		accountCapabilities[jmapapi.ContactURN] = map[string]any{
			"maxAddressBooks":      100,
			"maxContactsPerBook":   0, // 0 = no server-imposed limit (RFC 9610 §1.5)
			"maxCardsInSet":        512,
			"mayCreateAddressBook": false, // AddressBook/set is out of v0.1 scope (FR-P.10)
			"mayDeleteAddressBook": false,
		}
	}
	capabilities["urn:ietf:params:jmap:core"] = map[string]any{
		"maxSizeUpload":         maxSizeUpload,
		"maxConcurrentUpload":   s.maxUpload,
		"maxSizeRequest":        maxSizeRequest,
		"maxConcurrentRequests": s.maxReq,
		"maxCallsInRequest":     256,
		"maxObjectsInGet":       512,
		"maxObjectsInSet":       512,
	}
	session := map[string]any{
		"capabilities": capabilities,
		"accounts": map[string]any{
			acct.ID: map[string]any{
				"name":                name,
				"isPersonal":          true,
				"accountCapabilities": accountCapabilities,
			},
		},
		"primaryAccounts": primaryAccounts(s, acct),
		"username":        user,
		"apiUrl":          base + "/" + acct.ID + "/jmap",
		// The eventsource exists as of M1 (FR-J.8), upload/download as
		// of M3 (FR-M.16, FR-M.17); all three are RFC 8620 §2 URI
		// templates this server expands exactly as written.
		"eventSourceUrl": base + "/" + acct.ID +
			"/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
		// {accountId} leads both URLs: the account path prefix *is* the
		// variable RFC 8620 §6.1/§6.2 require, so expanding the
		// template produces this server's own route with nothing left
		// over (D-13: one origin, account-prefixed paths).
		"uploadUrl":   base + "/{accountId}/upload/",
		"downloadUrl": base + "/{accountId}/download/{blobId}/{name}?type={type}",
		"state":       s.sessionState(acct, user),
	}
	writeJSON(w, http.StatusOK, session, map[string]string{
		"Cache-Control": "no-cache, no-store, must-revalidate",
	})
}

// handleAPI serves POST /{account}/jmap (FR-J.2…FR-J.6).
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	acct := s.authorize(w, r, r.PathValue("account"))
	if acct == nil {
		if s.cfg.Auth.Mode == "none" {
			http.NotFound(w, r)
		}
		return
	}

	mediaType := r.Header.Get("Content-Type")
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	if strings.TrimSpace(mediaType) != "application/json" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"type":   "urn:ietf:params:jmap:error:notJSON",
			"status": 400,
			"detail": "Content-Type must be application/json",
		}, nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSizeRequest+1))
	if err != nil {
		http.Error(w, "could not read request", http.StatusBadRequest)
		return
	}
	if len(body) > maxSizeRequest {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"type":   "urn:ietf:params:jmap:error:limit",
			"status": http.StatusRequestEntityTooLarge,
			"limit":  "maxSizeRequest",
			"detail": "request body exceeds maxSizeRequest",
		}, nil)
		return
	}

	user := username(r, acct)
	jacct := &jmapapi.Account{
		ID:           acct.ID,
		Store:        s.store,
		Backend:      s.backends[acct.ID],
		Capabilities: s.accountCapabilities(acct),
		Identity:     identityFor(acct),
		SessionState: s.sessionState(acct, user),
	}
	status, resp := s.jmap.Dispatch(r.Context(), jacct, body)
	writeJSON(w, status, resp, nil)
}

// canSend reports whether this account may be offered submission
// (FR-J.5): it needs an SMTP server to send through *and* an IMAP
// backend, because the engine that submits is the same one that files
// the sent message and applies the caller's patches — an account with
// SMTP but no IMAP has no engine behind it (main skips those).
func canSend(acct *config.Account) bool {
	return acct.SMTP != nil && acct.IMAP != nil
}

// canContacts applies the FR-P.3 gate: CardDAV configured and the first
// sync succeeded. nil readiness (a server built without engines) means
// "never" — the honest default.
func (s *Server) canContacts(acct *config.Account) bool {
	return acct != nil && acct.CardDAV != nil && acct.CardDAV.URL != "" &&
		s.contactsReady != nil && s.contactsReady(acct.ID)
}

// primaryAccounts names the account for every capability it offers
// (FR-J.5): mail always, submission with SMTP, contacts once the gate
// passes.
func primaryAccounts(s *Server, acct *config.Account) map[string]any {
	out := map[string]any{"urn:ietf:params:jmap:mail": acct.ID}
	if canSend(acct) {
		out[jmapapi.SubmissionURN] = acct.ID
	}
	if s.canContacts(acct) {
		out[jmapapi.ContactURN] = acct.ID
	}
	return out
}

// accountCapabilities is what this account offers (FR-J.5): core and
// mail always, submission only once it can actually send, contacts only
// once the FR-P.3 gate passes. The session advertises this set and
// dispatch accepts `using` entries from it, so the two can never drift
// apart.
func (s *Server) accountCapabilities(acct *config.Account) []string {
	caps := []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}
	if canSend(acct) {
		caps = append(caps, jmapapi.SubmissionURN)
	}
	if s.canContacts(acct) {
		caps = append(caps, jmapapi.ContactURN)
	}
	return caps
}

// identityFor builds the account's sendable identity (FR-M.14): the
// configured address, signed with the account's display name, under an
// id that is stable across restarts. An account that cannot send has
// none — and then neither the capability nor Identity/get exist.
func identityFor(acct *config.Account) *jmapapi.Identity {
	if !canSend(acct) || acct.Address == "" {
		return nil
	}
	return &jmapapi.Identity{
		ID:    jmapapi.IdentityID(acct.ID),
		Name:  acct.Name,
		Email: acct.Address,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any, extra map[string]string) {
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

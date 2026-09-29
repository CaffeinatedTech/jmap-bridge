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

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
)

// Request size caps (NFR-5, locked as D-16). maxSizeRequest mirrors the
// value advertised in the core capability object.
const (
	maxSizeRequest = 32 << 20 // 32 MiB
	maxSizeUpload  = 64 << 20 // 64 MiB (advertised; upload endpoint lands in M3)
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
}

// New wires a Server: routing, auth, dispatch and push. store serves
// every configured account; backends maps an account id to the object
// that mutates it (nil for a cache-only account, which then refuses
// every write); hub is the change signal source for the eventsource
// endpoint (FR-J.8).
func New(cfg *config.Config, tokens *auth.Tokens, store jmapapi.Store,
	backends map[string]jmapapi.Backend, hub *push.Hub, log *slog.Logger,
) *Server {
	s := &Server{
		cfg:      cfg,
		tokens:   tokens,
		store:    store,
		backends: backends,
		hub:      hub,
		jmap:     jmapapi.NewHandler(store),
		log:      log,
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /{account}/.well-known/jmap", s.handleSession)
	s.mux.HandleFunc("POST /{account}/jmap", s.handleAPI)
	s.mux.HandleFunc("GET /{account}/eventsource/", s.handleEventSource)
	// Subtree pattern: the session's {accountId} expands into the
	// path prefix, and anything after it is the client's business
	// (RFC 8620 §6.1 only requires the template to carry accountId).
	s.mux.HandleFunc("POST /{account}/upload/", s.handleUpload)
	s.mux.HandleFunc("GET /{account}/download/{blobId}/{name}", s.handleDownload)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

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
	if acct == nil || !ok || user == "" || !s.tokens.Verify(accountID, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap-bridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
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
		strings.Join(jmapapi.Capabilities(), ","),
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
	capabilities["urn:ietf:params:jmap:core"] = map[string]any{
		"maxSizeUpload":         maxSizeUpload,
		"maxConcurrentUpload":   4,
		"maxSizeRequest":        maxSizeRequest,
		"maxConcurrentRequests": 8,
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
		"primaryAccounts": map[string]any{
			"urn:ietf:params:jmap:mail": acct.ID,
		},
		"username": user,
		"apiUrl":   base + "/" + acct.ID + "/jmap",
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
		Capabilities: capabilitiesFor(acct),
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

// capabilitiesFor is what this account offers (FR-J.5): core and mail
// always, submission only once it can actually send. The session
// advertises this set and dispatch accepts `using` entries from it, so
// the two can never drift apart.
func capabilitiesFor(acct *config.Account) []string {
	caps := []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}
	if canSend(acct) {
		caps = append(caps, jmapapi.SubmissionURN)
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

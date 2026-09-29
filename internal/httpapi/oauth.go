package httpapi

import (
	"net/http"
	"strings"
)

// The OAuth2 bootstrap endpoints (FR-A.5, FR-A.6). They are served on
// the base_url origin like everything else, are reached from a browser
// rather than by a JMAP client, and are therefore the one surface the
// per-account token does not guard: the flow's own state is the
// credential. Handlers never log query strings, codes or tokens.

// handleOAuth serves the GET /oauth/ subtree (FR-A.5, FR-A.6). A single
// subtree route with internal dispatch avoids a ServeMux pattern
// conflict with the account-prefixed routes: "/oauth/personal/start"
// would otherwise match both "/oauth/{account}/start" and
// "/{account}/eventsource/" without either being more specific.
//
// The endpoints are served on the base_url origin like everything else,
// are reached from a browser rather than by a JMAP client, and are
// therefore the one surface the per-account token does not guard: the
// flow's own state is the credential. Handlers never log query strings,
// codes or tokens.
func (s *Server) handleOAuth(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/oauth/")
	account, action, ok := strings.Cut(rest, "/")
	if !ok || account == "" {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "start":
		s.handleOAuthStart(w, r, account)
	case "callback":
		s.handleOAuthCallback(w, r, account)
	default:
		http.NotFound(w, r)
	}
}

// handleOAuthStart answers /oauth/{account}/start: it begins a consent
// flow and redirects the browser to the provider. An unknown account —
// or one without an OAuth2 client — 404s without saying whether the
// other kind of account exists (FR-A.11).
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request, account string) {
	m := s.oauth[account]
	if m == nil {
		http.NotFound(w, r)
		return
	}
	consent, err := m.Start()
	if err != nil {
		s.log.Error("oauth: start failed", "account", account, "err", err)
		http.Error(w, "could not start the consent flow", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, consent, http.StatusFound)
}

// handleOAuthCallback answers /oauth/{account}/callback: it validates
// state, binds the result to exactly the account in the path, stores the
// credentials, wakes that account's sync engine, and answers with a
// minimal static page (FR-A.6: no JavaScript dependencies). The
// provider may also redirect back with an error parameter — consent
// denied, expired — which is a failed callback like any other.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request, account string) {
	m := s.oauth[account]
	if m == nil {
		http.NotFound(w, r)
		return
	}
	// A provider refusal arrives as error/error_description; treat it
	// exactly like a missing code. The values are never logged.
	code, state := r.FormValue("code"), r.FormValue("state")
	if r.FormValue("error") != "" {
		code = ""
	}
	if err := m.Callback(r.Context(), code, state); err != nil {
		s.log.Error("oauth: callback failed", "account", account, "err", err)
		oauthPage(w, http.StatusBadRequest, "Authorization failed",
			"The provider refused or the flow expired. Existing credentials were left untouched. Start again from /oauth/"+account+"/start.")
		return
	}
	if s.kick != nil {
		s.kick(account)
	}
	oauthPage(w, http.StatusOK, "Authorization complete",
		"The account's credentials were stored and synchronisation has been started. You can close this page.")
}

// oauthPage writes the minimal result page: static HTML, no scripts,
// no styling dependencies (FR-A.6).
func oauthPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>` + title + `</title></head>
<body><h1>` + title + `</h1><p>` + body + `</p></body></html>
`))
}

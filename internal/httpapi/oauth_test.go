package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
)

// memTokens is a minimal sealed-value TokenStore for the endpoint tests.
type memTokens struct {
	refresh map[string]string
}

func (m *memTokens) LoadToken(_ context.Context, account string) (string, string, time.Time, error) {
	return m.refresh[account], "", time.Time{}, nil
}

func (m *memTokens) SaveToken(_ context.Context, account, refresh, _ string, _ time.Time) error {
	m.refresh[account] = refresh
	return nil
}

func (m *memTokens) ClearToken(_ context.Context, account string) error {
	delete(m.refresh, account)
	return nil
}

func oauthTestServer(t *testing.T, tokenServer *httptest.Server) (*Server, *memTokens, *[]string) {
	t.Helper()
	cfg := &config.Config{
		Listen:  "127.0.0.1:0",
		BaseURL: "http://127.0.0.1:8080",
		DataDir: t.TempDir(),
		Auth:    config.Auth{Mode: "token"},
		Accounts: []config.Account{{
			ID:      "personal",
			Address: "me@example.test",
			Token:   "tok",
			IMAP: &config.IMAP{
				Host: "127.0.0.1", Port: 143, Auth: "oauth2",
				Username: "me@example.test",
			},
			OAuth2: &config.OAuth2{
				Provider:     "generic",
				ClientID:     "cid",
				ClientSecret: "csec",
				AuthURL:      tokenServer.URL + "/auth",
				TokenURL:     tokenServer.URL + "/token",
				Scopes:       []string{"mail"},
			},
		}},
	}
	ts := &memTokens{refresh: map[string]string{}}
	cipher, err := auth.NewCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := oauth.NewManager("personal", oauthConfigFor(cfg.Accounts[0]), cfg.BaseURL, ts, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	var kicked []string
	s := New(cfg, nil, nil, nil, push.New(), nil,
		map[string]*oauth.Manager{"personal": mgr},
		func(account string) { kicked = append(kicked, account) }, nil)
	return s, ts, &kicked
}

func oauthConfigFor(a config.Account) oauth.Config {
	o := a.OAuth2
	return oauth.Config{
		Provider: o.Provider, ClientID: o.ClientID, ClientSecret: o.ClientSecret,
		AuthURL: o.AuthURL, TokenURL: o.TokenURL, Scopes: o.Scopes,
	}
}

const consentGranted = `{"access_token":"at-1","expires_in":3600,"refresh_token":"rt-1"}`

func TestOAuthStartRedirectsToProvider(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(consentGranted))
	}))
	t.Cleanup(provider.Close)
	s, _, _ := oauthTestServer(t, provider)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/personal/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), provider.URL+"/auth") {
		t.Fatalf("redirect = %q", loc)
	}
	if loc.Query().Get("client_id") != "cid" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("consent params = %v", loc.Query())
	}
	if loc.Query().Get("redirect_uri") != "http://127.0.0.1:8080/oauth/personal/callback" {
		t.Fatalf("redirect_uri = %q", loc.Query().Get("redirect_uri"))
	}
}

func TestOAuthCallbackStoresAndKicks(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(consentGranted))
	}))
	t.Cleanup(provider.Close)
	s, ts, kicked := oauthTestServer(t, provider)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/personal/start", nil))
	state, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	stateParam := state.Query().Get("state")

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET",
		"/oauth/personal/callback?code=abc&state="+url.QueryEscape(stateParam), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "complete") {
		t.Error("success page missing")
	}
	// Sealed at rest, and the engine was woken for exactly this account.
	stored := ts.refresh["personal"]
	if stored == "rt-1" || stored == "" {
		t.Fatalf("stored refresh token = %q", stored)
	}
	if len(*kicked) != 1 || (*kicked)[0] != "personal" {
		t.Fatalf("kicked = %v", *kicked)
	}

	// The state is single-use: replay fails and renders the failure page.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET",
		"/oauth/personal/callback?code=abc&state="+url.QueryEscape(stateParam), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay = %d, want 400", rec.Code)
	}
}

func TestOAuthCallbackForgedStateAndUnknownAccount(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(consentGranted))
	}))
	t.Cleanup(provider.Close)
	s, ts, _ := oauthTestServer(t, provider)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/personal/callback?code=abc&state=forged", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d, want 400", rec.Code)
	}
	if _, ok := ts.refresh["personal"]; ok {
		t.Fatal("forged callback stored credentials")
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/ghost/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account start = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/ghost/callback?code=a&state=b", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account callback = %d, want 404", rec.Code)
	}
}

func TestOAuthCallbackProviderRefusal(t *testing.T) {
	// The provider redirects back with error=access_denied: a failed
	// callback that leaves existing credentials untouched.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(consentGranted))
	}))
	t.Cleanup(provider.Close)
	s, ts, _ := oauthTestServer(t, provider)
	ts.refresh["personal"] = "existing-sealed"

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET",
		"/oauth/personal/callback?error=access_denied&state=whatever", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("provider refusal = %d, want 400", rec.Code)
	}
	if ts.refresh["personal"] != "existing-sealed" {
		t.Fatal("provider refusal disturbed existing credentials")
	}
}

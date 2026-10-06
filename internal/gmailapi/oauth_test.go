package gmailapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// memTokenStore is a minimal oauth.TokenStore for the bridge test.
type memTokenStore struct {
	mu      sync.Mutex
	refresh string
	access  string
	expiry  time.Time
}

func (s *memTokenStore) LoadToken(_ context.Context, _ string) (string, string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh, s.access, s.expiry, nil
}

func (s *memTokenStore) SaveToken(_ context.Context, _, refresh, access string, expiry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh, s.access, s.expiry = refresh, access, expiry
	return nil
}

func (s *memTokenStore) ClearToken(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh, s.access, s.expiry = "", "", time.Time{}
	return nil
}

// TestTokenSourceFromManagerAttachesBearer proves the oauth.Manager bridge
// yields a bearer the fixture accepts: the fixture rejects any token other than
// "at-bridge", so a successful call is proof the token came from the manager.
func TestTokenSourceFromManagerAttachesBearer(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-bridge","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	store := &memTokenStore{refresh: "rt-1"}
	m, err := oauth.NewManager("acct", oauth.Config{
		Provider:     "generic",
		ClientID:     "id",
		ClientSecret: "secret",
		AuthURL:      "https://provider.example/auth",
		TokenURL:     tokenSrv.URL,
		Scopes:       []string{"mail"},
	}, "http://127.0.0.1:8080", store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	fx := fixturegmail.Start(t, fixturegmail.Options{Token: "at-bridge"})
	c, err := New(context.Background(), Options{
		TokenSource:         TokenSourceFromManager(m),
		Endpoint:            fx.URL(),
		QuotaUnitsPerSecond: -1,
		Clock:               newFakeClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.profile(context.Background()); err != nil {
		t.Fatalf("profile through oauth bridge: %v", err)
	}
	store.mu.Lock()
	saved := store.access
	store.mu.Unlock()
	if saved != "at-bridge" {
		t.Fatalf("manager did not persist the refreshed access token: %q", saved)
	}
}

// TestManagerRefreshReachesRetriedRequest is the regression for the API-mode
// wedge where a forced refresh (Options.Reauthorize) updated the manager but
// the HTTP transport kept replaying the token it had cached: the client is
// built on oauth2.Transport (which consults the source on every request), not
// oauth2.NewClient's ReuseTokenSource, so after Google invalidates a token the
// retried call carries the fresh bearer and the account self-heals in-process.
func TestManagerRefreshReachesRetriedRequest(t *testing.T) {
	// Token endpoint: the first refresh issues tok-1, every later one tok-2.
	var tokenMu sync.Mutex
	refreshes := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenMu.Lock()
		refreshes++
		n := refreshes
		tokenMu.Unlock()
		tok := "tok-2"
		if n == 1 {
			tok = "tok-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + tok + `","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	// API server: tok-1 is accepted only on the very first request, then the
	// mailbox rejects it as Google does when a token is revoked before its
	// nominal expiry. tok-2 is always accepted.
	var apiMu sync.Mutex
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiMu.Lock()
		requests++
		n := requests
		apiMu.Unlock()
		auth := r.Header.Get("Authorization")
		firstCall := n == 1 && auth == "Bearer tok-1"
		if auth != "Bearer tok-2" && !firstCall {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid Credentials","errors":[{"reason":"authError"}]}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"emailAddress":"fixture@example.test","historyId":"1000"}`))
	}))
	defer api.Close()

	store := &memTokenStore{refresh: "rt-1"}
	m, err := oauth.NewManager("acct", oauth.Config{
		Provider:     "generic",
		ClientID:     "id",
		ClientSecret: "secret",
		AuthURL:      "https://provider.example/auth",
		TokenURL:     tokenSrv.URL,
		Scopes:       []string{"mail"},
	}, "http://127.0.0.1:8080", store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), Options{
		TokenSource:         TokenSourceFromManager(m),
		Endpoint:            api.URL + "/",
		QuotaUnitsPerSecond: -1,
		Clock:               newFakeClock(),
		Reauthorize: func(ctx context.Context) error {
			_, err := m.AccessToken(ctx, true)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.profile(context.Background()); err != nil {
		t.Fatalf("first profile: %v", err)
	}
	if _, err := c.profile(context.Background()); err != nil {
		t.Fatalf("profile after token invalidation did not self-heal: %v", err)
	}
}

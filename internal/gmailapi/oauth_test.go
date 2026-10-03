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

package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
)

// memStore is an in-memory TokenStore that records exactly what it was
// handed, so tests can assert the sealing happened before persistence.
type memStore struct {
	mu      sync.Mutex
	refresh map[string]string
	access  map[string]string
	expiry  map[string]time.Time
}

func newMemStore() *memStore {
	return &memStore{
		refresh: map[string]string{},
		access:  map[string]string{},
		expiry:  map[string]time.Time{},
	}
}

func (s *memStore) LoadToken(_ context.Context, account string) (string, string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refresh[account], s.access[account], s.expiry[account], nil
}

func (s *memStore) SaveToken(_ context.Context, account, refresh, access string, expiry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh[account], s.access[account], s.expiry[account] = refresh, access, expiry
	return nil
}

func (s *memStore) ClearToken(_ context.Context, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refresh, account)
	delete(s.access, account)
	delete(s.expiry, account)
	return nil
}

func (s *memStore) has(account string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.refresh[account]
	return ok
}

func testConfig(tokenURL string) Config {
	return Config{
		Provider:     "generic",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthURL:      "https://provider.example/auth",
		TokenURL:     tokenURL,
		Scopes:       []string{"mail"},
	}
}

func newManager(t *testing.T, tokenURL string, ts TokenStore, cipher *auth.Cipher) *Manager {
	t.Helper()
	m, err := NewManager("personal", testConfig(tokenURL), "http://127.0.0.1:8080", ts, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// tokenServer answers one or two token requests, recording the form.
func tokenServer(t *testing.T, responses ...string) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		n := len(forms)
		forms = append(forms, r.PostForm)
		mu.Unlock()
		resp := responses[min(n, len(responses)-1)]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &forms
}

const granted = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`

func TestGoogleProfile(t *testing.T) {
	p := GoogleProfile(false)
	if p.TokenURL != "https://oauth2.googleapis.com/token" || len(p.Scopes) != 1 {
		t.Fatalf("profile = %#v", p)
	}
	if p.Scopes[0] != "https://mail.google.com/" {
		t.Fatalf("mail scope = %q", p.Scopes[0])
	}
	p = GoogleProfile(true)
	if len(p.Scopes) != 2 || !strings.HasSuffix(p.Scopes[1], "/auth/carddav") {
		t.Fatalf("carddav scope missing: %#v", p.Scopes)
	}
}

func TestStartCarriesPKCEAndState(t *testing.T) {
	m := newManager(t, "http://unused/token", newMemStore(), nil)
	consent, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(consent)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "client-id" {
		t.Fatalf("consent params = %v", q)
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:8080/oauth/personal/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("PKCE challenge = %q", q.Get("code_challenge"))
	}
	if q.Get("state") == "" || len(q.Get("state")) < 32 {
		t.Fatalf("state = %q", q.Get("state"))
	}
	// First start (no stored refresh token) asks for consent explicitly;
	// later starts should not re-show it.
	if q.Get("prompt") != "consent" {
		t.Fatalf("first start must set prompt=consent, got %q", q.Get("prompt"))
	}
	state := q.Get("state")
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, first := m.pending[state]
	n := len(m.pending)
	m.mu.Unlock()
	if !first || n != 2 {
		t.Fatalf("pending flows after two starts: first=%v n=%d", first, n)
	}
}

func TestCallbackExchangesAndStoresSealed(t *testing.T) {
	srv, forms := tokenServer(t, granted)
	ts := newMemStore()
	cipher, err := auth.NewCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, srv.URL, ts, cipher)
	consent, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	state := mustQuery(t, consent, "state")
	verifier := m.pendingVerifier(state)

	if err := m.Callback(context.Background(), "auth-code", state); err != nil {
		t.Fatal(err)
	}
	if len(*forms) != 1 {
		t.Fatalf("token endpoint called %d times", len(*forms))
	}
	form := (*forms)[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "auth-code" {
		t.Fatalf("exchange form = %v", form)
	}
	if form.Get("code_verifier") != verifier {
		t.Fatalf("PKCE verifier not carried: %q vs %q", form.Get("code_verifier"), verifier)
	}
	// The store holds sealed values, never the plaintext (FR-A.8).
	stored, _, _, err := ts.LoadToken(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if stored == "rt-1" || !strings.HasPrefix(stored, auth.SealPrefix) {
		t.Fatalf("refresh token stored unsealed: %q", stored)
	}
	open, err := cipher.Open(stored)
	if err != nil || open != "rt-1" {
		t.Fatalf("sealed refresh token does not open: %q %v", stored, err)
	}

	// The exchange bound the state: replaying it fails and touches
	// nothing.
	if err := m.Callback(context.Background(), "again", state); err == nil {
		t.Fatal("state replay accepted")
	}
}

func TestCallbackUnknownStateLeavesCredentials(t *testing.T) {
	srv, _ := tokenServer(t, granted)
	ts := newMemStore()
	m := newManager(t, srv.URL, ts, nil)
	if err := ts.SaveToken(context.Background(), "personal", "rt-old", "at-old", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Callback(context.Background(), "code", "forged-state"); err == nil {
		t.Fatal("forged state accepted")
	}
	refresh, _, _, err := ts.LoadToken(context.Background(), "personal")
	if err != nil || refresh != "rt-old" {
		t.Fatalf("forged callback disturbed credentials: %q %v", refresh, err)
	}
}

func TestAccessTokenRefreshesAndCaches(t *testing.T) {
	srv, forms := tokenServer(t, `{"access_token":"at-2","expires_in":3600,"refresh_token":"rt-2"}`)
	ts := newMemStore()
	m := newManager(t, srv.URL, ts, nil)
	if err := ts.SaveToken(context.Background(), "personal", "rt-old", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	tok, err := m.AccessToken(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-2" {
		t.Fatalf("token = %q", tok)
	}
	// Cached now: a second call costs no round trip.
	if _, err := m.AccessToken(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(*forms) != 1 {
		t.Fatalf("token endpoint called %d times", len(*forms))
	}
	form := (*forms)[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" {
		t.Fatalf("refresh form = %v", form)
	}
	// force skips the cache (the authenticate-failed retry, FR-A.7).
	tok, err = m.AccessToken(context.Background(), true)
	if err != nil || tok != "at-2" {
		t.Fatalf("forced refresh = %q, %v", tok, err)
	}
	if len(*forms) != 2 {
		t.Fatalf("forced refresh did not hit the endpoint")
	}
	// The provider kept the same refresh token; the store must not lose it.
	refresh, _, _, err := ts.LoadToken(context.Background(), "personal")
	if err != nil || refresh != "rt-old" {
		t.Fatalf("refresh without rotation lost the stored token: %q %v", refresh, err)
	}
}

func TestAccessTokenInvalidGrantClears(t *testing.T) {
	srv, _ := tokenServer(t, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	ts := newMemStore()
	m := newManager(t, srv.URL, ts, nil)
	if err := ts.SaveToken(context.Background(), "personal", "rt-dead", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, err := m.AccessToken(context.Background(), false)
	if err == nil {
		t.Fatal("dead refresh token accepted")
	}
	if !errors.Is(err, ErrReauthNeeded) {
		t.Fatalf("error is not reauth-shaped: %v", err)
	}
	if ts.has("personal") {
		t.Fatal("dead credentials not cleared")
	}
}

func TestAccessTokenWithoutCredentialsNamesTheStartURL(t *testing.T) {
	m := newManager(t, "http://unused/token", newMemStore(), nil)
	_, err := m.AccessToken(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "/oauth/personal/start") {
		t.Fatalf("error should point at the start URL: %v", err)
	}
}

func TestPendingFlowExpires(t *testing.T) {
	m := newManager(t, "http://unused/token", newMemStore(), nil)
	state, err := randomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.pending[state] = pendingFlow{verifier: "v", created: time.Now().Add(-2 * flowTTL)}
	m.mu.Unlock()
	if err := m.Callback(context.Background(), "code", state); err == nil {
		t.Fatal("expired flow accepted")
	}
}

// --- small helpers ---

func mustQuery(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query().Get(key)
	if v == "" {
		t.Fatalf("%s missing from %q", key, raw)
	}
	return v
}

func (m *Manager) pendingVerifier(state string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[state].verifier
}

// urlParse is url.Parse under test-local aliasing so the test file stays
// free of a second import block entry.

// Package oauth runs one account's OAuth2 lifecycle against its
// provider (FR-A.5–.9): the consent bootstrap with PKCE S256, the
// callback exchange, proactive access-token refresh with exactly one
// forced retry on authentication failure, and sealed persistence of the
// refresh token (FR-A.8). Provider profiles keep the provider-specific
// endpoints and scopes out of the flow code, so a new provider is
// configuration, not code (FR-A.9).
//
// Everything here fails closed on ambiguity and never logs credentials:
// authorization codes, tokens, verifiers and states are values the
// package handles but never formats into a message.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
)

// Profile is a provider's endpoints and scopes (FR-A.9).
type Profile struct {
	AuthURL  string
	TokenURL string
	Scopes   []string
}

// GoogleProfile returns the Gmail profile: full mailbox access, plus the
// CardDAV scope when contacts are configured (FR-A.9).
func GoogleProfile(carddav bool) Profile {
	scopes := []string{"https://mail.google.com/"}
	if carddav {
		scopes = append(scopes, "https://www.googleapis.com/auth/carddav")
	}
	return Profile{
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		Scopes:   scopes,
	}
}

// Config is one account's OAuth2 client, straight from the [accounts.oauth2]
// block after config validation (FR-A.1).
type Config struct {
	Provider     string
	ClientID     string
	ClientSecret string
	AuthURL      string // provider = "generic"
	TokenURL     string
	Scopes       []string
	HasCardDAV   bool // google only: request the CardDAV scope too (FR-A.9)
}

// Profile resolves the provider profile (FR-A.9). Validation has already
// pinned provider to "google" or "generic"; a generic profile missing its
// URLs is a programming error upstream of this package.
func (c Config) Profile() (Profile, error) {
	switch c.Provider {
	case "google":
		return GoogleProfile(c.HasCardDAV), nil
	case "generic":
		if c.AuthURL == "" || c.TokenURL == "" || len(c.Scopes) == 0 {
			return Profile{}, errors.New("oauth: generic provider needs auth_url, token_url and scopes")
		}
		return Profile{AuthURL: c.AuthURL, TokenURL: c.TokenURL, Scopes: c.Scopes}, nil
	default:
		return Profile{}, fmt.Errorf("oauth: unknown provider %q", c.Provider)
	}
}

// TokenStore persists credential material exactly as handed over —
// sealed when a key is configured (FR-A.8). The oauth package owns the
// sealing; the store stays an opaque table.
type TokenStore interface {
	// LoadToken returns the persisted credentials, or empty strings with
	// no error when the account has none.
	LoadToken(ctx context.Context, account string) (refresh, access string, expiry time.Time, err error)
	// SaveToken replaces the account's credentials.
	SaveToken(ctx context.Context, account, refresh, access string, expiry time.Time) error
	// ClearToken drops the account's credentials (the provider revoked
	// them; only fresh consent helps).
	ClearToken(ctx context.Context, account string) error
}

// ErrReauthNeeded reports that the stored refresh token no longer works:
// the operator (or user) must visit the start URL again. It is distinct
// from transport errors because retrying cannot help.
var ErrReauthNeeded = errors.New("oauth: provider rejected the refresh token, consent required again")

// Manager is one account's OAuth2 client.
type Manager struct {
	account  string
	cfg      Config
	profile  Profile
	redirect string // the callback URL on base_url (FR-A.5)
	store    TokenStore
	cipher   *auth.Cipher
	log      *slog.Logger
	client   *http.Client

	// mu guards the in-memory flow state and the cached access token.
	mu      sync.Mutex
	pending map[string]pendingFlow
	access  string
	expiry  time.Time
}

type pendingFlow struct {
	verifier string
	created  time.Time
}

// flowTTL bounds how long a started consent may sit unfinished; the
// provider's own code TTL is shorter, so this only bounds our memory.
const flowTTL = 10 * time.Minute

// maxPending bounds the pending-flow map; starting more flows than this
// drops the oldest.
const maxPending = 16

// NewManager builds one account's client. redirectBase is the bridge's
// base_url; the callback path is appended here so the HTTP layer and the
// provider agree on it (FR-A.5).
func NewManager(account string, cfg Config, redirectBase string, ts TokenStore, cipher *auth.Cipher, log *slog.Logger) (*Manager, error) {
	profile, err := cfg.Profile()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	return &Manager{
		account:  account,
		cfg:      cfg,
		profile:  profile,
		redirect: strings.TrimRight(redirectBase, "/") + "/oauth/" + account + "/callback",
		store:    ts,
		cipher:   cipher,
		log:      log,
		client:   client,
		pending:  map[string]pendingFlow{},
	}, nil
}

// Start begins a consent flow: it mints state and a PKCE verifier,
// remembers the pairing, and returns the provider consent URL to
// redirect the operator to (FR-A.5).
func (m *Manager) Start() (string, error) {
	state, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("oauth: state: %w", err)
	}
	verifier, err := randomToken(64)
	if err != nil {
		return "", fmt.Errorf("oauth: pkce verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", m.cfg.ClientID)
	q.Set("redirect_uri", m.redirect)
	q.Set("scope", strings.Join(m.profile.Scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// A refresh token only comes with offline access. prompt=consent is
	// needed when Google has already issued one we no longer hold (or
	// lost); asking always would re-show the consent screen on every
	// bootstrap, which is noise, not safety.
	q.Set("access_type", "offline")
	refresh, _, _, err := m.loadSealed()
	if err != nil {
		return "", err
	}
	if refresh == "" {
		q.Set("prompt", "consent")
	}

	m.mu.Lock()
	if len(m.pending) >= maxPending {
		m.dropOldestPendingLocked()
	}
	m.pending[state] = pendingFlow{verifier: verifier, created: time.Now()}
	m.prunePendingLocked(time.Now())
	m.mu.Unlock()

	return m.profile.AuthURL + "?" + q.Encode(), nil
}

// Callback completes a consent flow (FR-A.6): the state must name a flow
// this manager started, the code is exchanged for tokens bound to
// exactly this account's redirect URI, and the refresh token is stored
// sealed. A failed or forged callback leaves any existing credentials
// untouched — the store is written only after a successful exchange.
func (m *Manager) Callback(ctx context.Context, code, state string) error {
	if code == "" || state == "" {
		return errors.New("oauth: callback is missing code or state")
	}
	m.mu.Lock()
	flow, ok := m.pending[state]
	if ok {
		delete(m.pending, state)
	}
	m.mu.Unlock()
	if !ok || time.Since(flow.created) > flowTTL {
		// Unknown, expired, or already used: never exchange, never log
		// the value (FR-A.6).
		return errors.New("oauth: unknown or expired state")
	}

	toks, err := m.exchange(ctx, code, flow.verifier)
	if err != nil {
		return err
	}
	if toks.RefreshToken == "" {
		// access_type=offline makes this a provider bug rather than a
		// user choice; storing an access token alone would break the
		// next refresh, so fail loudly here instead.
		return errors.New("oauth: provider granted no refresh token")
	}
	expiry := time.Now().Add(time.Duration(toks.ExpiresIn) * time.Second)
	if err := m.saveSealed(toks.RefreshToken, toks.AccessToken, expiry); err != nil {
		return err
	}
	m.mu.Lock()
	m.access = toks.AccessToken
	m.expiry = expiry
	m.mu.Unlock()
	m.log.Info("oauth: consent completed", "account", m.account)
	return nil
}

// AccessToken returns a bearer token for IMAP/SMTP (FR-A.7). It is
// refreshed proactively before expiry; force marks a retry after the
// server rejected the previous token, which is the "exactly one retry"
// half of FR-A.7 — the caller retries the authenticate once with the
// fresh token and then reports failure.
func (m *Manager) AccessToken(ctx context.Context, force bool) (string, error) {
	m.mu.Lock()
	if !force && m.access != "" && time.Now().Before(m.expiry.Add(-refreshMargin)) {
		tok := m.access
		m.mu.Unlock()
		return tok, nil
	}
	m.mu.Unlock()

	refresh, _, _, err := m.loadSealed()
	if err != nil {
		return "", err
	}
	if refresh == "" {
		return "", fmt.Errorf("oauth: account %q has no stored credentials; visit %s",
			m.account, strings.TrimSuffix(m.redirect, "/callback")+"/start")
	}
	toks, err := m.refresh(ctx, refresh)
	if err != nil {
		var rejected *providerError
		if errors.As(err, &rejected) && rejected.Code == "invalid_grant" {
			// The refresh token is dead: keeping it would loop forever.
			if err := m.store.ClearToken(ctx, m.account); err != nil {
				m.log.Warn("oauth: could not clear dead credentials", "account", m.account, "err", err)
			}
			return "", ErrReauthNeeded
		}
		return "", err
	}
	expiry := time.Now().Add(time.Duration(toks.ExpiresIn) * time.Second)
	if err := m.saveSealed(refresh, toks.AccessToken, expiry); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.access = toks.AccessToken
	m.expiry = expiry
	m.mu.Unlock()
	m.log.Debug("oauth: refreshed access token", "account", m.account)
	return toks.AccessToken, nil
}

// refreshMargin is how long before expiry a cached token is considered
// spent: a margin large enough to cover a slow IMAP handshake, small
// enough not to waste refreshes.
const refreshMargin = 60 * time.Second

// tokenResult is the subset of the provider's token response this
// package uses (RFC 6749 §5.1).
type tokenResult struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// providerError is the provider's structured refusal (RFC 6749 §5.2).
type providerError struct {
	Code string
	Desc string
}

func (e *providerError) Error() string {
	// The description is provider text, not a credential; the code alone
	// would be too thin to debug from.
	return "oauth: provider refused: " + e.Code + ": " + e.Desc
}

// exchange trades the authorization code (plus the flow's verifier) for
// tokens (FR-A.5).
func (m *Manager) exchange(ctx context.Context, code, verifier string) (tokenResult, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
		"redirect_uri":  {m.redirect},
		"code_verifier": {verifier},
	}
	return m.tokenPOST(ctx, form)
}

// refresh trades the stored refresh token for a fresh access token
// (FR-A.7). Providers that rotate refresh tokens name the new one; the
// caller keeps the old value when the response carries none.
func (m *Manager) refresh(ctx context.Context, refreshToken string) (tokenResult, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {m.cfg.ClientID},
		"client_secret": {m.cfg.ClientSecret},
	}
	return m.tokenPOST(ctx, form)
}

func (m *Manager) tokenPOST(ctx context.Context, form url.Values) (tokenResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.profile.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResult{}, fmt.Errorf("oauth: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return tokenResult{}, fmt.Errorf("oauth: token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResult{}, fmt.Errorf("oauth: token response: %w", err)
	}
	var toks tokenResult
	if err := json.Unmarshal(body, &toks); err != nil {
		return tokenResult{}, fmt.Errorf("oauth: token response is not JSON (status %d)", resp.StatusCode)
	}
	if toks.Error != "" {
		return tokenResult{}, &providerError{Code: toks.Error, Desc: toks.ErrorDesc}
	}
	if resp.StatusCode != http.StatusOK || toks.AccessToken == "" {
		return tokenResult{}, fmt.Errorf("oauth: token endpoint answered %d", resp.StatusCode)
	}
	return toks, nil
}

// loadSealed reads the stored credentials and opens them. The refresh
// token is the one value that must survive an access-token-less row.
func (m *Manager) loadSealed() (refresh, access string, expiry time.Time, err error) {
	refresh, access, expiry, err = m.store.LoadToken(context.Background(), m.account)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if m.cipher != nil && refresh != "" {
		if refresh, err = m.cipher.Open(refresh); err != nil {
			return "", "", time.Time{}, fmt.Errorf("oauth: stored refresh token: %w", err)
		}
		if access, err = m.cipher.Open(access); err != nil {
			return "", "", time.Time{}, fmt.Errorf("oauth: stored access token: %w", err)
		}
	}
	return refresh, access, expiry, nil
}

// saveSealed seals and persists the credential pair (FR-A.8).
func (m *Manager) saveSealed(refresh, access string, expiry time.Time) error {
	if m.cipher != nil {
		refresh, access = m.cipher.Seal(refresh), m.cipher.Seal(access)
	}
	return m.store.SaveToken(context.Background(), m.account, refresh, access, expiry)
}

// prunePendingLocked drops flows older than flowTTL.
func (m *Manager) prunePendingLocked(now time.Time) {
	for state, flow := range m.pending {
		if now.Sub(flow.created) > flowTTL {
			delete(m.pending, state)
		}
	}
}

// dropOldestPendingLocked makes room for a new flow.
func (m *Manager) dropOldestPendingLocked() {
	oldestState := ""
	var oldest time.Time
	for state, flow := range m.pending {
		if oldestState == "" || flow.created.Before(oldest) {
			oldestState, oldest = state, flow.created
		}
	}
	if oldestState != "" {
		delete(m.pending, oldestState)
	}
}

// randomToken returns n random bytes as unpadded base64url — the PKCE
// and state alphabets both.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

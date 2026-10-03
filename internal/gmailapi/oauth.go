package gmailapi

import (
	"context"

	"golang.org/x/oauth2"

	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
)

// TokenSourceFromManager adapts an account's oauth.Manager to an
// oauth2.TokenSource for the generated Gmail client (D-API-7). Consent, PKCE,
// proactive refresh, sealed persistence and the ErrReauthNeeded/ErrNoCredentials
// signals all stay in internal/oauth; this package only asks the manager for a
// bearer token.
//
// The oauth2.TokenSource interface carries no context, so the manager is called
// with context.Background(). That is safe because Manager.AccessToken serves its
// in-memory cache without a round trip while the token is fresh; a refresh
// happens on the manager's own schedule, and the engine's caller context still
// bounds the HTTP request that triggered it. The returned token is deliberately
// marked unexpired-but-not-valid (no Expiry) so oauth2 consults the manager on
// every request rather than caching a token the manager might rotate.
func TokenSourceFromManager(m *oauth.Manager) oauth2.TokenSource {
	return managerSource{m: m}
}

type managerSource struct{ m *oauth.Manager }

// Token implements oauth2.TokenSource.
func (s managerSource) Token() (*oauth2.Token, error) {
	tok, err := s.m.AccessToken(context.Background(), false)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: tok}, nil
}

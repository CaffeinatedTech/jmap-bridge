package gmailapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

// PushConfig configures how the bridge authenticates a Gmail Pub/Sub push
// (GMAIL_API_PLAN §6.4, FR-S.14). Exactly one mode is active: when PlainSecret
// is non-empty, a shared secret is checked instead of an OIDC token — the
// loopback/fixture escape hatch; otherwise the request must carry a valid
// Google-signed OIDC token for Audience whose `email` claim equals
// ServiceAccount.
type PushConfig struct {
	// Audience is the OIDC audience the Pub/Sub push subscription mints
	// for (typically the bridge's public base_url).
	Audience string
	// ServiceAccount is the `email` claim the token must carry.
	ServiceAccount string
	// PlainSecret, when set, switches to shared-secret mode (loopback
	// rigs); the account's client token is used for this in production.
	PlainSecret string
}

// PushVerifier authenticates the `Authorization: Bearer` token on a
// `POST /gmail/push/{account}` request. It is the seam the HTTP layer and
// tests depend on; the concrete type is built by NewPushVerifier.
type PushVerifier struct {
	cfg      PushConfig
	validate idTokenValidator
}

// idTokenValidator validates a Google OIDC token and returns its claims. It is
// a function value so tests substitute a fake without minting real tokens;
// production wraps idtoken.Validate.
type idTokenValidator func(ctx context.Context, token, audience string) (map[string]any, error)

// NewPushVerifier builds the production verifier (idtoken.Validate).
func NewPushVerifier(cfg PushConfig) *PushVerifier {
	return newPushVerifier(cfg, validateGoogleIDToken)
}

// newPushVerifier builds a verifier with an injected token validator.
func newPushVerifier(cfg PushConfig, validate idTokenValidator) *PushVerifier {
	return &PushVerifier{cfg: cfg, validate: validate}
}

// validateGoogleIDToken is the production idtoken.Validate wrapper; the
// official package checks the signature, issuer, expiry and audience, and its
// claims carry the `email` of the signing service account.
func validateGoogleIDToken(ctx context.Context, token, audience string) (map[string]any, error) {
	payload, err := idtoken.Validate(ctx, token, audience)
	if err != nil {
		return nil, err
	}
	return payload.Claims, nil
}

// Verify authenticates one push request. It fails closed: any missing,
// malformed or mismatched credential is an error and the caller answers
// non-2xx.
func (v *PushVerifier) Verify(ctx context.Context, r *http.Request) error {
	if v.cfg.PlainSecret != "" {
		return v.verifyPlain(r)
	}
	if v.cfg.Audience == "" || v.cfg.ServiceAccount == "" {
		return errors.New("gmailapi: push verifier misconfigured")
	}
	return v.verifyOIDC(ctx, r)
}

func (v *PushVerifier) verifyOIDC(ctx context.Context, r *http.Request) error {
	token := bearerToken(r)
	if token == "" {
		return errors.New("gmailapi: push missing bearer token")
	}
	claims, err := v.validate(ctx, token, v.cfg.Audience)
	if err != nil {
		return fmt.Errorf("gmailapi: push token invalid: %w", err)
	}
	email, _ := claims["email"].(string)
	if !strings.EqualFold(email, v.cfg.ServiceAccount) {
		return fmt.Errorf("gmailapi: push token email %q is not the configured service account", email)
	}
	if verified, ok := claims["email_verified"].(bool); ok && !verified {
		return errors.New("gmailapi: push token email is not verified")
	}
	return nil
}

func (v *PushVerifier) verifyPlain(r *http.Request) error {
	got := r.Header.Get("X-Push-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(v.cfg.PlainSecret)) != 1 {
		return errors.New("gmailapi: push plain secret mismatch")
	}
	return nil
}

// bearerToken extracts the token from an `Authorization: Bearer …` header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

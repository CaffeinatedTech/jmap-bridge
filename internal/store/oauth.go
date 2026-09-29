package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OAuthToken is one account's persisted provider tokens (FR-A.5, FR-A.7).
// Values are opaque to the store: the OAuth layer seals them before they
// are saved and opens them after they are read (FR-A.8), so plaintext
// credentials never rest in this table when a key is configured.
type OAuthToken struct {
	RefreshToken string
	AccessToken  string
	AccessExpiry time.Time // zero: unknown, refresh before use
}

// OAuthToken returns the account's persisted tokens, or the zero value
// when the account has none (never authenticated, or the credentials
// were cleared after the provider revoked them).
func (s *Store) OAuthToken(ctx context.Context, account string) (OAuthToken, error) {
	var out OAuthToken
	var expiry int64
	err := s.db.QueryRowContext(ctx,
		`SELECT refresh_token, access_token, access_expiry FROM oauth_tokens WHERE account = ?`,
		account).Scan(&out.RefreshToken, &out.AccessToken, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthToken{}, nil
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("store: oauth token: %w", err)
	}
	if expiry != 0 {
		out.AccessExpiry = time.Unix(expiry, 0)
	}
	return out, nil
}

// HasOAuthToken reports whether any token row exists for the account.
func (s *Store) HasOAuthToken(ctx context.Context, account string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM oauth_tokens WHERE account = ?`, account).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: oauth token check: %w", err)
	}
	return true, nil
}

// SaveOAuthToken upserts the account's tokens. A refresh token is only
// ever replaced, never merged: the provider's latest word is the truth.
func (s *Store) SaveOAuthToken(ctx context.Context, account string, tok OAuthToken) error {
	if tok.RefreshToken == "" {
		return errors.New("store: oauth token: empty refresh token")
	}
	var expiry int64
	if !tok.AccessExpiry.IsZero() {
		expiry = tok.AccessExpiry.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_tokens(account, refresh_token, access_token, access_expiry, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(account) DO UPDATE SET
		   refresh_token = excluded.refresh_token,
		   access_token = excluded.access_token,
		   access_expiry = excluded.access_expiry,
		   updated_at = excluded.updated_at`,
		account, tok.RefreshToken, tok.AccessToken, expiry, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: save oauth token: %w", err)
	}
	return nil
}

// ClearOAuthToken drops the account's tokens: the provider refused the
// refresh (invalid_grant), so the only way forward is a fresh consent,
// and stale credentials must not keep being retried (FR-A.7).
func (s *Store) ClearOAuthToken(ctx context.Context, account string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM oauth_tokens WHERE account = ?`, account); err != nil {
		return fmt.Errorf("store: clear oauth token: %w", err)
	}
	return nil
}

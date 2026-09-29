package store

import (
	"context"
	"testing"
	"time"
)

// The OAuth token table round-trips per account and clears cleanly
// (FR-A.5, FR-A.7). The store treats the values as opaque: sealing is
// the oauth layer's business (FR-A.8).
func TestOAuthTokenPersistence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok, err := s.OAuthToken(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "" {
		t.Fatal("token row materialised out of nothing")
	}
	if ok, err := s.HasOAuthToken(ctx, "acct"); err != nil || ok {
		t.Fatalf("HasOAuthToken = %v %v", ok, err)
	}

	expiry := time.Unix(1800000000, 0)
	if err := s.SaveOAuthToken(ctx, "acct", OAuthToken{
		RefreshToken: "sealed-rt", AccessToken: "sealed-at", AccessExpiry: expiry,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasOAuthToken(ctx, "acct"); err != nil || !ok {
		t.Fatalf("HasOAuthToken after save = %v %v", ok, err)
	}
	tok, err = s.OAuthToken(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "sealed-rt" || tok.AccessToken != "sealed-at" ||
		!tok.AccessExpiry.Equal(expiry) {
		t.Fatalf("round trip = %#v", tok)
	}

	// The provider's latest word replaces the row wholesale.
	if err := s.SaveOAuthToken(ctx, "acct", OAuthToken{RefreshToken: "rt2"}); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.OAuthToken(ctx, "acct")
	if tok.RefreshToken != "rt2" || tok.AccessToken != "" {
		t.Fatalf("replacement = %#v", tok)
	}

	if err := s.SaveOAuthToken(ctx, "acct", OAuthToken{}); err == nil {
		t.Fatal("empty refresh token accepted")
	}
	if err := s.ClearOAuthToken(ctx, "acct"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasOAuthToken(ctx, "acct"); ok {
		t.Fatal("row survived ClearOAuthToken")
	}
}

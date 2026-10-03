package config

import (
	"strings"
	"testing"
)

// FR-D.12 / FR-A.2: secret values never reach logs or error strings.
// Validation failures name the offending key, never the value, even when
// several secrets are present in the same account block.
func TestValidationErrorsNeverCarrySecretValues(t *testing.T) {
	const (
		tokenSentinel    = "tok-SENTINEL-0123456789abcdef" // ≥24 bytes
		passwordSentinel = "imap-password-SENTINEL-4f3a"
		clientSentinel   = "oauth-client-secret-SENTINEL"
		carddavSentinel  = "carddav-password-SENTINEL"
	)
	// The account is deliberately misconfigured (auth = "oauth2" with a
	// password, FR-A.10) so validation fails while every secret is in
	// play; the error must name the key and leak none of the values.
	toml := strings.Replace(minimalLoopback,
		`token = "t0ken-0123456789abcdef0123456789"`,
		`token = "`+tokenSentinel+`"

  [accounts.imap]
  host = "imap.example.com"
  port = 993
  auth = "oauth2"
  username = "me@example.com"
  password = "`+passwordSentinel+`"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid.apps.googleusercontent.com"
  client_secret = "`+clientSentinel+`"

  [accounts.carddav]
  url = "https://dav.example.com"
  username = "me@example.com"
  password = "`+carddavSentinel+`"`, 1)

	_, err := load(t, toml)
	if err == nil {
		t.Fatal("expected a validation error for oauth2 + password")
	}
	if !strings.Contains(err.Error(), "accounts[0].imap.password") {
		t.Fatalf("error %q does not name the offending key", err)
	}
	for name, secret := range map[string]string{
		"token":         tokenSentinel,
		"imap password": passwordSentinel,
		"client secret": clientSentinel,
		"carddav pass":  carddavSentinel,
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s leaked into the error string: %q", name, err)
		}
	}
}

// Package auth holds client-facing credentials (D-15): per-account
// bearer tokens verified over HTTP Basic on every endpoint (FR-A.3),
// compared in constant time against a stored hash, failing closed on any
// ambiguity (FR-A.12). Backend credentials (IMAP/SMTP passwords, OAuth2)
// live in config and, from M4, encrypted at rest (FR-A.8).
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// Tokens maps account ids to the SHA-256 hash of their client token. The
// plaintext token is never stored (FR-A.3).
type Tokens struct {
	hashes map[string][sha256.Size]byte
}

// NewTokens hashes the given account→token pairs (account id → plaintext
// token). Tokens must be non-empty; config validation guarantees that
// before this is called.
func NewTokens(tokens map[string]string) *Tokens {
	hashes := make(map[string][sha256.Size]byte, len(tokens))
	for id, tok := range tokens {
		hashes[id] = sha256.Sum256([]byte(tok))
	}
	return &Tokens{hashes: hashes}
}

// Verify reports whether password is the token for accountID. It is a
// constant-time comparison against the stored hash; an unknown account
// compares against the zero digest — a lookup miss costs the same
// compare as a hit and still fails, so misses do not leak which accounts
// exist (FR-A.11, FR-A.12).
func (t *Tokens) Verify(accountID, password string) bool {
	want, ok := t.hashes[accountID] // zero digest when the account is unknown
	got := sha256.Sum256([]byte(password))
	match := subtle.ConstantTimeCompare(got[:], want[:])
	return ok && match == 1
}

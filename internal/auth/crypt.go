package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// The Cipher encrypts credentials and refresh tokens at rest with
// AES-256-GCM (FR-A.8). The key comes from JMAP_BRIDGE_SECRET_KEY and
// never from the data volume; without a key the bridge runs in plaintext
// mode, which the caller is expected to warn about prominently at
// startup (PLAN §9).
//
// Sealed values carry a version prefix, and Open accepts unprefixed
// values, so a plaintext credential from before a key was configured —
// or sealed under a previous key — upgrades on the next write: reads
// never fail because of a rotation, and re-sealing happens whenever the
// value is written again (FR-A.8).
type Cipher struct {
	aead cipher.AEAD
	// key stays in memory only (it is already there as the config value)
	// so Fingerprint can tell two keys apart in logs without printing one.
	key []byte
}

// SealPrefix marks every value this build sealed; Open treats anything
// else as plaintext and passes it through.
const SealPrefix = "jbe1:"

// NewCipher builds a Cipher from the raw JMAP_BRIDGE_SECRET_KEY value.
// Accepted key forms: exactly 32 raw bytes, base64 (standard or URL
// alphabet, padded or not) decoding to 32 bytes, or hex decoding to 32
// bytes. Anything else is a startup error — a weak key silently accepted
// is worse than a failed start.
func NewCipher(key string) (*Cipher, error) {
	raw, err := decodeKey(key)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("auth: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: gcm: %w", err)
	}
	return &Cipher{aead: aead, key: raw}, nil
}

// decodeKey accepts the documented key forms and rejects everything
// else, naming the form rather than the value.
func decodeKey(key string) ([]byte, error) {
	if len(key) == 32 {
		return []byte(key), nil
	}
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := hex.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	return nil, fmt.Errorf("secret key must be 32 bytes (raw, base64, or hex)")
}

// Enabled reports whether anything this Cipher seals is actually
// encrypted; false means plaintext mode (no key configured).
func (c *Cipher) Enabled() bool { return c != nil && c.aead != nil }

// Fingerprint returns a short non-reversible tag of the key, for logs
// that need to tell two keys apart without ever printing one.
func (c *Cipher) Fingerprint() string {
	if !c.Enabled() {
		return ""
	}
	sum := sha256.Sum256(c.key)
	return hex.EncodeToString(sum[:4])
}

// Seal encrypts plain. An empty input stays empty (there is nothing to
// protect); in plaintext mode the value passes through unchanged.
func (c *Cipher) Seal(plain string) string {
	if plain == "" || !c.Enabled() {
		return plain
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		// Crypto/rand failing is not recoverable and must not silently
		// downgrade to plaintext.
		panic("auth: seal: " + err.Error())
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return SealPrefix + base64.RawURLEncoding.EncodeToString(sealed)
}

// Open reverses Seal. A value without the seal prefix is returned as
// plaintext: it predates a configured key, or the bridge is running
// without one. A prefixed value that does not decrypt is an error — the
// wrong key must never silently yield garbage credentials.
func (c *Cipher) Open(sealed string) (string, error) {
	if sealed == "" || !strings.HasPrefix(sealed, SealPrefix) {
		return sealed, nil
	}
	if !c.Enabled() {
		return "", fmt.Errorf("auth: sealed credential without a configured key")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, SealPrefix))
	if err != nil {
		return "", fmt.Errorf("auth: sealed credential: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize+c.aead.Overhead() {
		return "", fmt.Errorf("auth: sealed credential: too short")
	}
	plain, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("auth: sealed credential: %w", err)
	}
	return string(plain), nil
}

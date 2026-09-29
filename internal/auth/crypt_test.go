package auth

import (
	"strings"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	sealed := c.Seal("refresh-token-secret")
	if sealed == "refresh-token-secret" || !strings.HasPrefix(sealed, SealPrefix) {
		t.Fatalf("seal produced %q", sealed)
	}
	open, err := c.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if open != "refresh-token-secret" {
		t.Fatalf("round trip = %q", open)
	}
}

func TestCipherKeyForms(t *testing.T) {
	raw := "0123456789abcdef0123456789abcdef"
	b64 := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // std, padded
	hexKey := "3031323334353637383961626364656630313233343536373839616263646566"
	for _, form := range []string{raw, b64, hexKey} {
		if _, err := NewCipher(form); err != nil {
			t.Errorf("NewCipher(%q…): %v", form[:8], err)
		}
	}
	if _, err := NewCipher("short"); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewCipher(""); err == nil {
		t.Error("empty key accepted")
	}
}

func TestCipherPlaintextPassthrough(t *testing.T) {
	// Without a key the cipher is nil: values pass through both ways,
	// which is what keeps a pre-key plaintext database readable.
	var c *Cipher
	if got := c.Seal("plain"); got != "plain" {
		t.Fatalf("seal without key = %q", got)
	}
	if open, err := c.Open("plain"); err != nil || open != "plain" {
		t.Fatalf("open without key = %q, %v", open, err)
	}
	// But a sealed value without a key is an error, never garbage.
	if _, err := c.Open(SealPrefix + "AAAA"); err == nil {
		t.Fatal("sealed value opened without a key")
	}
}

func TestCipherEmptyAndWrongKey(t *testing.T) {
	c, _ := NewCipher("0123456789abcdef0123456789abcdef")
	if got := c.Seal(""); got != "" {
		t.Fatalf("seal of empty = %q", got)
	}
	if open, err := c.Open("untouched"); err != nil || open != "untouched" {
		t.Fatalf("open of plaintext = %q, %v", open, err)
	}
	other, _ := NewCipher("fedcba9876543210fedcba9876543210")
	if _, err := other.Open(c.Seal("secret")); err == nil {
		t.Fatal("wrong key decrypted a sealed value")
	}
}

func TestCipherRotationUpgrade(t *testing.T) {
	// The rotation contract: a plaintext (pre-key) value reads fine under
	// any key and is re-sealed on its next write, while a value sealed
	// under a different key is an error, never garbage credentials.
	withKey, _ := NewCipher("0123456789abcdef0123456789abcdef")
	plain, err := withKey.Open("legacy-plaintext")
	if err != nil || plain != "legacy-plaintext" {
		t.Fatalf("plaintext read under a key = %q, %v", plain, err)
	}
	old, _ := NewCipher("0123456789abcdef0123456789abcdef")
	fresh, _ := NewCipher("fedcba9876543210fedcba9876543210")
	if _, err := fresh.Open(old.Seal("token")); err == nil {
		t.Fatal("rotation did not reject the old key's ciphertext")
	}
}

func TestCipherFingerprint(t *testing.T) {
	c, _ := NewCipher("0123456789abcdef0123456789abcdef")
	if fp := c.Fingerprint(); len(fp) != 8 {
		t.Fatalf("fingerprint = %q", fp)
	}
	var none *Cipher
	if none.Fingerprint() != "" {
		t.Fatal("fingerprint without a key is not empty")
	}
}

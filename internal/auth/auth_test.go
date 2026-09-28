package auth

import "testing"

func newTestTokens() *Tokens {
	return NewTokens(map[string]string{"personal": "tok-a", "work": "tok-b"})
}

func TestVerify(t *testing.T) {
	tokens := newTestTokens()
	cases := []struct {
		account, password string
		want              bool
	}{
		{"personal", "tok-a", true},
		{"work", "tok-b", true},
		{"personal", "tok-b", false}, // token from another account
		{"work", "tok-a", false},
		{"personal", "", false},
		{"personal", "tok-a ", false},
		{"unknown", "tok-a", false},
		{"unknown", "", false},
	}
	for _, tc := range cases {
		if got := tokens.Verify(tc.account, tc.password); got != tc.want {
			t.Errorf("Verify(%q, %q) = %v, want %v", tc.account, tc.password, got, tc.want)
		}
	}
}

func TestVerifyEmptyRegistry(t *testing.T) {
	// auth.mode = "none" registers no tokens; Verify must fail closed.
	tokens := NewTokens(nil)
	if tokens.Verify("personal", "anything") {
		t.Error("Verify with no registered tokens must fail")
	}
}

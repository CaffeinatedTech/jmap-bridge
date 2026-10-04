package gmailapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeValidator is a stand-in for idtoken.Validate: it accepts exactly the
// token "good" for the expected audience and returns the supplied claims.
// No real Google token is needed, so every verification branch is testable.
func fakeValidator(expectedAudience string, claims map[string]any) idTokenValidator {
	return func(_ context.Context, token, audience string) (map[string]any, error) {
		if token != "good" {
			return nil, errors.New("bad signature")
		}
		if audience != expectedAudience {
			return nil, errors.New("wrong audience")
		}
		return claims, nil
	}
}

func pushRequest(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://bridge.example/gmail/push/gmail", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestPushVerifierOIDC(t *testing.T) {
	const audience = "https://bridge.example"
	const sa = "push@project.iam.gserviceaccount.com"
	claims := map[string]any{"email": sa, "email_verified": true}
	v := newPushVerifier(PushConfig{Audience: audience, ServiceAccount: sa},
		fakeValidator(audience, claims))

	if err := v.Verify(context.Background(), pushRequest("good")); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestPushVerifierOIDCFailures(t *testing.T) {
	const audience = "https://bridge.example"
	const sa = "push@project.iam.gserviceaccount.com"
	cases := []struct {
		name   string
		claims map[string]any
		bearer string
	}{
		{"missing bearer", map[string]any{"email": sa}, ""},
		{"bad signature", map[string]any{"email": sa}, "forged"},
		{"wrong email", map[string]any{"email": "attacker@example.test", "email_verified": true}, "good"},
		{"unverified email", map[string]any{"email": sa, "email_verified": false}, "good"},
		{"missing email claim", map[string]any{"email_verified": true}, "good"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newPushVerifier(PushConfig{Audience: audience, ServiceAccount: sa},
				fakeValidator(audience, tc.claims))
			if err := v.Verify(context.Background(), pushRequest(tc.bearer)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestPushVerifierMisconfiguredFailsClosed(t *testing.T) {
	v := newPushVerifier(PushConfig{}, fakeValidator("", map[string]any{"email": "x"}))
	if err := v.Verify(context.Background(), pushRequest("good")); err == nil {
		t.Fatal("empty OIDC config must fail closed")
	}
}

func TestPushVerifierPlainSecret(t *testing.T) {
	const secret = "t0ken-0123456789abcdef0123456789"
	v := newPushVerifier(PushConfig{PlainSecret: secret}, nil)

	// Header form.
	r := pushRequest("")
	r.Header.Set("X-Push-Token", secret)
	if err := v.Verify(context.Background(), r); err != nil {
		t.Fatalf("header secret rejected: %v", err)
	}
	// Query form.
	r = httptest.NewRequest(http.MethodPost, "https://bridge.example/gmail/push/gmail?token="+secret, nil)
	if err := v.Verify(context.Background(), r); err != nil {
		t.Fatalf("query secret rejected: %v", err)
	}
	// Wrong and missing secrets fail closed.
	if err := v.Verify(context.Background(), pushRequest("")); err == nil {
		t.Fatal("missing secret must fail")
	}
	r = pushRequest("")
	r.Header.Set("X-Push-Token", "wrong")
	if err := v.Verify(context.Background(), r); err == nil {
		t.Fatal("wrong secret must fail")
	}
}

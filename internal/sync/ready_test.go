package sync

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CaffeinatedTech/go-imap"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"imap auth failed code", &imap.Error{Code: imap.CodeAuthenticationFailed}, true},
		{"imap authz failed code", &imap.Error{Code: imap.CodeAuthorizationFailed}, true},
		{"imap NO with no code", &imap.Error{Type: imap.ErrorTypeNo}, false},
		{"throttled is not auth", fmt.Errorf("wrapped: %w", imapdrv.ErrThrottled), false},
		{"oauth reauth sentinel", oauth.ErrReauthNeeded, true},
		{"oauth no-credentials sentinel", fmt.Errorf("dial: %w", oauth.ErrNoCredentials), true},
		{"plain transport error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthFailure(tc.err); got != tc.want {
				t.Fatalf("isAuthFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A healthy engine reports ready after its first pass and is never
// marked auth-failed (FR-D.4).
func TestReadyAfterFirstPass(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	waitUntil(t, 5*time.Second, "engine ready", env.eng.Ready)
	if env.eng.AuthFailed() {
		t.Fatal("healthy engine reported an auth failure")
	}
}

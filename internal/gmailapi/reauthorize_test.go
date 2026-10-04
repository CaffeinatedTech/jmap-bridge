package gmailapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/api/googleapi"

	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// stubCall is a minimal apiCall whose Do behaviour the test controls.
type stubCall struct {
	fn     func() (string, error)
	header http.Header
}

func (s *stubCall) Do(...googleapi.CallOption) (string, error) { return s.fn() }

func (s *stubCall) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}

func newReauthTestClient(reauthorize func(context.Context) error) *Client {
	return &Client{
		pacer:       NewPacer(-1, realClock{}),
		maxRetries:  DefaultMaxRetries,
		reauthorize: reauthorize,
		clock:       realClock{},
	}
}

// TestInvokeForcesRefreshOn401 pins FR-A.7 for the API path: a rejected
// access token is refreshed once and the call retried, so a token Google
// invalidated before its nominal expiry self-heals in-process.
func TestInvokeForcesRefreshOn401(t *testing.T) {
	calls, refreshed := 0, 0
	c := newReauthTestClient(func(context.Context) error { refreshed++; return nil })
	call := &stubCall{fn: func() (string, error) {
		calls++
		if calls == 1 {
			return "", &googleapi.Error{Code: http.StatusUnauthorized, Message: "Invalid Credentials"}
		}
		return "ok", nil
	}}
	got, err := invoke(context.Background(), c, 0, call)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got != "ok" {
		t.Fatalf("invoke = %q, want ok", got)
	}
	if calls != 2 {
		t.Fatalf("call.Do ran %d times, want 2 (initial + retry)", calls)
	}
	if refreshed != 1 {
		t.Fatalf("reauthorize ran %d times, want 1", refreshed)
	}
}

// TestInvokeAuthRetriedOnlyOnce pins "exactly one retry": a second 401 is
// final, not a refresh loop.
func TestInvokeAuthRetriedOnlyOnce(t *testing.T) {
	calls, refreshed := 0, 0
	c := newReauthTestClient(func(context.Context) error { refreshed++; return nil })
	call := &stubCall{fn: func() (string, error) {
		calls++
		return "", &googleapi.Error{Code: http.StatusUnauthorized, Message: "Invalid Credentials"}
	}}
	_, err := invoke(context.Background(), c, 0, call)
	if !errors.Is(err, mailbackend.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if calls != 2 {
		t.Fatalf("call.Do ran %d times, want 2", calls)
	}
	if refreshed != 1 {
		t.Fatalf("reauthorize ran %d times, want 1", refreshed)
	}
}

// TestInvokeReauthFailureSurfaces pins that a dead refresh token is
// reported as-is (so the engine marks the account auth-failed) rather than
// being classified as a retryable transport error.
func TestInvokeReauthFailureSurfaces(t *testing.T) {
	reErr := errors.New("oauth: provider rejected the refresh token, consent required again")
	calls := 0
	c := newReauthTestClient(func(context.Context) error { return reErr })
	call := &stubCall{fn: func() (string, error) {
		calls++
		return "", &googleapi.Error{Code: http.StatusUnauthorized}
	}}
	_, err := invoke(context.Background(), c, 0, call)
	if !errors.Is(err, reErr) {
		t.Fatalf("err = %v, want %v", err, reErr)
	}
	if calls != 1 {
		t.Fatalf("call.Do ran %d times, want 1 (no retry after a failed refresh)", calls)
	}
}

// TestInvokeNoReauthorizeIsFinal pins the static/fixture bearer path: with
// no refresher a 401 is returned immediately.
func TestInvokeNoReauthorizeIsFinal(t *testing.T) {
	calls := 0
	c := newReauthTestClient(nil)
	call := &stubCall{fn: func() (string, error) {
		calls++
		return "", &googleapi.Error{Code: http.StatusUnauthorized}
	}}
	_, err := invoke(context.Background(), c, 0, call)
	if !errors.Is(err, mailbackend.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if calls != 1 {
		t.Fatalf("call.Do ran %d times, want 1", calls)
	}
}

// TestInvokeOnceRefreshesOn401 pins the same recovery for submission: a 401
// never reached message processing, so one retry is safe.
func TestInvokeOnceRefreshesOn401(t *testing.T) {
	calls, refreshed := 0, 0
	c := newReauthTestClient(func(context.Context) error { refreshed++; return nil })
	call := &stubCall{fn: func() (string, error) {
		calls++
		if calls == 1 {
			return "", &googleapi.Error{Code: http.StatusUnauthorized}
		}
		return "sent", nil
	}}
	got, err := invokeOnce(context.Background(), c, 0, call)
	if err != nil || got != "sent" {
		t.Fatalf("invokeOnce = %q, %v; want sent, nil", got, err)
	}
	if calls != 2 || refreshed != 1 {
		t.Fatalf("calls=%d refreshed=%d, want 2, 1", calls, refreshed)
	}
}

package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
)

// fakePushVerifier is the PushVerifier seam stand-in: it accepts requests
// carrying the right push token and rejects everything else.
type fakePushVerifier struct{ accept string }

func (f fakePushVerifier) Verify(_ context.Context, r *http.Request) error {
	if r.Header.Get("X-Push-Token") != f.accept {
		return errors.New("forged push")
	}
	return nil
}

// pushRecorder captures the kick callback.
type pushRecorder struct {
	mu       sync.Mutex
	accounts []string
}

func (p *pushRecorder) kick(account string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = append(p.accounts, account)
}

func (p *pushRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

func newPushServer(t *testing.T, verifiers map[string]PushVerifier) (http.Handler, *pushRecorder) {
	t.Helper()
	cfg, err := config.LoadReader(strings.NewReader(testConfig))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rec := &pushRecorder{}
	opts := []Option{}
	if verifiers != nil {
		opts = append(opts, WithGmailPush(verifiers))
	}
	h := New(cfg, auth.NewTokens(nil), fixture.New(), nil, push.New(), nil, nil,
		rec.kick, nil, opts...)
	return h, rec
}

func pushPost(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/gmail/push/personal", strings.NewReader(body))
}

func TestGmailPushAcceptedNudgesEngine(t *testing.T) {
	h, rec := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	// The real Gmail payload sends historyId as a JSON number.
	r := pushPost(`{"emailAddress":"me@example.test","historyId":1234567890123}`)
	r.Header.Set("X-Push-Token", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.count() != 1 {
		t.Fatalf("engine kicked %d times, want 1", rec.count())
	}
}

func TestGmailPushEnvelopeFormAccepted(t *testing.T) {
	h, rec := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	data := base64.StdEncoding.EncodeToString([]byte(`{"emailAddress":"me@example.test","historyId":123}`))
	r := pushPost(`{"message":{"data":"` + data + `"}}`)
	r.Header.Set("X-Push-Token", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.count() != 1 {
		t.Fatalf("engine not nudged")
	}
}

// TestGmailPushHistoryIDStringAccepted keeps the loopback/fixture string
// form working: json.Number accepts both a number and a quoted string.
func TestGmailPushHistoryIDStringAccepted(t *testing.T) {
	h, rec := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	r := pushPost(`{"emailAddress":"me@example.test","historyId":"123"}`)
	r.Header.Set("X-Push-Token", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if rec.count() != 1 {
		t.Fatalf("engine not nudged")
	}
}

func TestGmailPushForgedRejected(t *testing.T) {
	h, rec := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	for _, tok := range []string{"", "forged"} {
		r := pushPost(`{"emailAddress":"me@example.test","historyId":"123"}`)
		if tok != "" {
			r.Header.Set("X-Push-Token", tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", tok, w.Code)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("forged pushes nudged the engine %d times", rec.count())
	}
}

func TestGmailPushUnknownAccountNotFound(t *testing.T) {
	h, _ := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	r := httptest.NewRequest(http.MethodPost, "/gmail/push/nobody", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGmailPushDisabledIsNotFound(t *testing.T) {
	h, _ := newPushServer(t, nil)
	r := pushPost(`{"historyId":"1"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when push is not configured", w.Code)
	}
}

func TestGmailPushBadBody(t *testing.T) {
	h, rec := newPushServer(t, map[string]PushVerifier{"personal": fakePushVerifier{accept: "secret"}})
	r := pushPost(`{"message":{"data":"not-base64!!"}}`)
	r.Header.Set("X-Push-Token", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if rec.count() != 0 {
		t.Fatal("bad body nudged the engine")
	}
}

package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// rateConfig exercises the failed-auth lockout with a short block so the
// test need not wait minutes.
const rateConfig = `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/jmap-bridge-test"

[rate]
auth_failures = 3
auth_block = "500ms"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-personal-0123456789abcdef"
`

func TestFailedAuthLockout(t *testing.T) {
	s := newTestServerCfg(t, rateConfig)

	for i := 0; i < 3; i++ {
		resp := s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "wrong", "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, resp.StatusCode)
		}
	}

	// The next attempt is refused before the credential is even checked.
	resp := s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "wrong", "", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after lockout = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	// Even the correct token is locked out for the block window.
	resp = s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal-0123456789abcdef", "", "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("correct token during lockout = %d, want 429", resp.StatusCode)
	}

	// After the block elapses the correct token works again.
	time.Sleep(600 * time.Millisecond)
	resp = s.do(t, http.MethodGet, "/personal/.well-known/jmap", "alice", "tok-personal-0123456789abcdef", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after block expiry = %d, want 200", resp.StatusCode)
	}
}

func TestAcquireSemaphore(t *testing.T) {
	s := &Server{reqSem: make(chan struct{}, 1)}
	if !s.acquire(s.reqSem) {
		t.Fatal("first acquire failed")
	}
	if s.acquire(s.reqSem) {
		t.Fatal("second acquire succeeded on a full semaphore")
	}
	s.release(s.reqSem)
	if !s.acquire(s.reqSem) {
		t.Fatal("acquire after release failed")
	}
}

func TestNilSemaphoreIsUnbounded(t *testing.T) {
	s := &Server{}
	if !s.acquire(s.reqSem) {
		t.Fatal("nil semaphore must allow")
	}
	s.release(s.reqSem) // must not panic
}

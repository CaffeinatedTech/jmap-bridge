package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/metrics"
	"github.com/CaffeinatedTech/jmap-bridge/internal/sync"
)

// The FR-D.4 probe endpoints are unauthenticated and answer ahead of the
// JMAP surface; a kubelet carries no client token.
func TestWithHealth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "jmap")
	})

	get := func(t *testing.T, handler http.Handler, path string) (int, string) {
		t.Helper()
		srv := httptest.NewServer(handler)
		defer srv.Close()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(body))
	}

	t.Run("healthz always ok", func(t *testing.T) {
		if code, body := get(t, withHealth(next, nil), "/healthz"); code != http.StatusOK || body != "ok" {
			t.Fatalf("healthz = %d %q, want 200 ok", code, body)
		}
	})
	t.Run("readyz ok when ready", func(t *testing.T) {
		if code, body := get(t, withHealth(next, func() bool { return true }), "/readyz"); code != http.StatusOK || body != "ready" {
			t.Fatalf("readyz = %d %q, want 200 ready", code, body)
		}
	})
	t.Run("readyz 503 when not ready", func(t *testing.T) {
		if code, body := get(t, withHealth(next, func() bool { return false }), "/readyz"); code != http.StatusServiceUnavailable || body != "not ready" {
			t.Fatalf("readyz = %d %q, want 503 not ready", code, body)
		}
	})
	t.Run("jmap surface passes through", func(t *testing.T) {
		if code, body := get(t, withHealth(next, nil), "/personal/jmap"); code != http.StatusTeapot || body != "jmap" {
			t.Fatalf("passthrough = %d %q, want 418 jmap", code, body)
		}
	})
}

func TestReadinessWithoutEngines(t *testing.T) {
	// A bridge with no IMAP engines (cache-only) has nothing to wait for.
	if got := readiness(map[string]*sync.Engine{}, nil)(); !got {
		t.Fatal("readiness with no engines should be true")
	}
}

// The FR-D.6 endpoint is served only when wired in; any other path falls
// through to the JMAP surface untouched.
func TestWithMetrics(t *testing.T) {
	reg := metrics.New()
	reg.Counter("jmap_bridge_method_calls_total", "JMAP method calls.", "account", "method").
		With("personal", "Email/get").Inc()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "jmap")
	})
	srv := httptest.NewServer(withMetrics(next, reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("/metrics Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body),
		`jmap_bridge_method_calls_total{account="personal",method="Email/get"} 1`) {
		t.Fatalf("/metrics body missing series:\n%s", body)
	}

	passthrough, err := http.Get(srv.URL + "/personal/jmap")
	if err != nil {
		t.Fatalf("get passthrough: %v", err)
	}
	defer func() { _ = passthrough.Body.Close() }()
	if passthrough.StatusCode != http.StatusTeapot {
		t.Fatalf("passthrough status = %d, want 418", passthrough.StatusCode)
	}
}

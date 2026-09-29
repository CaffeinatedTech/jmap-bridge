package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// sseEnv is a server backed by a *real* store (states must be able to
// move) and the hub that store publishes to.
type sseEnv struct {
	ts    *httptest.Server
	st    *store.Store
	hub   *push.Hub
	token string
}

func newSSEEnv(t *testing.T) *sseEnv {
	t.Helper()
	cfg := loadTestConfig(t)
	hub := push.New()
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Publish: hub.Publish,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tokens := auth.NewTokens(map[string]string{cfg.Accounts[0].ID: cfg.Accounts[0].Token})
	ts := httptest.NewServer(New(cfg, tokens, st, nil, hub, nil))
	t.Cleanup(ts.Close)
	return &sseEnv{ts: ts, st: st, hub: hub, token: cfg.Accounts[0].Token}
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadReader(strings.NewReader(testConfig))
	if err != nil {
		t.Fatalf("test config: %v", err)
	}
	return cfg
}

// open connects to the eventsource and streams lines until cancelled.
func (e *sseEnv) open(t *testing.T, query string, header map[string]string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.ts.URL+"/personal/eventsource/?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("any", e.token)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eventsource status = %d", resp.StatusCode)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	stop := func() {
		cancel()
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return lines, stop
}

// waitLine collects until a line contains want or the budget expires.
func waitLine(t *testing.T, lines <-chan string, want string, budget time.Duration) []string {
	t.Helper()
	var seen []string
	deadline := time.After(budget)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q (saw %v)", want, seen)
			}
			seen = append(seen, line)
			if strings.Contains(line, want) {
				return seen
			}
		case <-deadline:
			t.Fatalf("no line containing %q within %v (saw %v)", want, budget, seen)
		}
	}
}

// mutate stores a mailbox so Mailbox/Thread-independent states move and
// the hub publishes.
func (e *sseEnv) mutate(t *testing.T) {
	t.Helper()
	_, err := e.st.SyncFolders(context.Background(), "personal", []store.Folder{
		{Name: "INBOX", Delim: '/', Role: "inbox", UIDValidity: 1},
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
}

func TestSessionAdvertisesEventSourceTemplate(t *testing.T) {
	env := newSSEEnv(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/personal/.well-known/jmap", nil)
	req.SetBasicAuth("any", env.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var session map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	tmpl, _ := session["eventSourceUrl"].(string)
	for _, placeholder := range []string{"{types}", "{closeafter}", "{ping}"} {
		if !strings.Contains(tmpl, placeholder) {
			t.Errorf("eventSourceUrl %q missing %s (RFC 8620 §2)", tmpl, placeholder)
		}
	}
}

func TestStateEventOnChange(t *testing.T) {
	env := newSSEEnv(t)
	lines, stop := env.open(t, "types=*&closeafter=no&ping=0", nil)
	defer stop()

	// Let the subscription land, then change state.
	time.Sleep(100 * time.Millisecond)
	env.mutate(t)

	seen := waitLine(t, lines, "StateChange", 2*time.Second)
	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, "id: ") || !strings.Contains(joined, "event: state") {
		t.Errorf("state event framing wrong: %v", seen)
	}
	data := dataOf(t, seen, "event: state")
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(data), &sc); err != nil {
		t.Fatalf("state payload %q: %v", data, err)
	}
	if sc.Type != "StateChange" {
		t.Errorf("@type = %q", sc.Type)
	}
	mailboxState := sc.Changed["personal"]["Mailbox"]
	if mailboxState == "" {
		t.Errorf("changed = %v, want personal.Mailbox state", sc.Changed)
	}
	// Only types that actually moved ride along — Email did not change
	// here, and an unchanged type would only make the client refetch.
	for typ := range sc.Changed["personal"] {
		if typ != "Mailbox" {
			t.Errorf("unexpected type %q in %v", typ, sc.Changed)
		}
	}
}

func TestCloseAfterStateEndsStream(t *testing.T) {
	env := newSSEEnv(t)
	lines, stop := env.open(t, "types=*&closeafter=state&ping=0", nil)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	env.mutate(t)
	waitLine(t, lines, "event: state", 2*time.Second)
	// The stream must now end: EOF closes the channel.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return // closed: server ended the response
			}
		case <-deadline:
			t.Fatal("stream still open after closeafter=state")
		}
	}
}

func TestPingEvent(t *testing.T) {
	env := newSSEEnv(t)
	lines, stop := env.open(t, "types=*&closeafter=no&ping=1", nil)
	defer stop()
	seen := waitLine(t, lines, `"interval":1`, 2500*time.Millisecond)
	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, "event: ping") {
		t.Errorf("ping event missing: %v", seen)
	}
	if strings.Contains(joined, "id: ") {
		t.Errorf("ping must not set an event id: %v", seen)
	}
}

func TestStaleLastEventIDTriggersCatchUp(t *testing.T) {
	env := newSSEEnv(t)
	// Move state first so "stale" definitely differs from current.
	env.mutate(t)
	lines, stop := env.open(t, "types=*&closeafter=no&ping=0",
		map[string]string{"Last-Event-ID": "stale-id"})
	defer stop()
	waitLine(t, lines, "event: state", 2*time.Second) // no mutation needed
}

func TestEventSourceRejectsBadParamsAndMissingAuth(t *testing.T) {
	env := newSSEEnv(t)
	for _, tc := range []struct{ query, user, pass string }{
		{query: "types=%2C&closeafter=no&ping=0", user: "any", pass: env.token},
		{query: "types=*&closeafter=maybe&ping=0", user: "any", pass: env.token},
		{query: "types=*&closeafter=no&ping=-4", user: "any", pass: env.token},
		{query: "types=*&closeafter=no&ping=0", user: "any", pass: "wrong"},
		{query: "types=*&closeafter=no&ping=0", user: "", pass: ""},
	} {
		req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/personal/eventsource/?"+tc.query, nil)
		if tc.user != "" || tc.pass != "" {
			req.SetBasicAuth(tc.user, tc.pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if tc.pass == "wrong" || tc.user == "" {
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401 (FR-J.6)", tc.query, resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.query, resp.StatusCode)
		}
	}
}

// dataOf returns the data payload of the first event named name.
func dataOf(t *testing.T, lines []string, name string) string {
	t.Helper()
	inEvent := false
	for _, line := range lines {
		if strings.HasPrefix(line, "event:") {
			inEvent = strings.Contains(line, name)
			continue
		}
		if inEvent && strings.HasPrefix(line, "data:") {
			return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	t.Fatalf("no data line for %s in %v", name, lines)
	return ""
}

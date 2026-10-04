// Package pushgate is the M13 end-to-end gate: it wires a real sync engine,
// the HTTP push endpoint, and the in-process Gmail API fixture together and
// proves that a verified Pub/Sub push makes a foreign change visible within
// FR-S.7's two-second budget, that a forged push is rejected, that the watch
// renews, and that poll mode still converges (FR-S.14).
package pushgate

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/gmailapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/httpapi"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/sync"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

const (
	account      = "gapi"
	clientToken  = "fixture-client-token-0123456789"
	pushTopic    = "projects/p/topics/t"
	foreignSubj  = "foreign push message"
	foreignMsgID = "<push1@x>"
)

// storeNative adapts the store's native_ids table to gmailapi's NativeIndex
// (mirrors cmd's adapter).
type storeNative struct {
	st      *store.Store
	account string
}

func (n storeNative) NativeUID(ctx context.Context, native string) (uint32, bool, error) {
	return n.st.NativeUID(ctx, n.account, native)
}

func (n storeNative) AllocNativeUIDs(ctx context.Context, natives []string) (map[string]uint32, error) {
	return n.st.AllocNativeUIDs(ctx, n.account, natives)
}

func (n storeNative) NativeByUID(ctx context.Context, uid uint32) (string, bool, error) {
	return n.st.NativeByUID(ctx, n.account, uid)
}

func (n storeNative) NativeByUIDs(ctx context.Context, uids []uint32) (map[uint32]string, error) {
	return n.st.NativeByUIDs(ctx, n.account, uids)
}

func (n storeNative) KnownMemberUIDs(ctx context.Context, container string, uids []uint32) (map[uint32]bool, error) {
	return n.st.KnownMemberUIDs(ctx, n.account, container, uids)
}

func (n storeNative) SaveDraft(ctx context.Context, uid uint32, draftID string) error {
	return n.st.SaveDraft(ctx, n.account, uid, draftID)
}

func (n storeNative) DraftID(ctx context.Context, uid uint32) (string, error) {
	return n.st.DraftIDByUID(ctx, n.account, uid)
}

// seedFixture brings up a Gmail API fixture with one inbox message and the
// system labels discovery needs.
func seedFixture(t *testing.T, watchExpiration time.Duration) *fixturegmail.Server {
	t.Helper()
	fx := fixturegmail.Start(t, fixturegmail.Options{WatchExpiration: watchExpiration})
	fx.SeedLabel(fixturegmail.LabelInbox, "INBOX", "system")
	fx.SeedLabel(fixturegmail.LabelSent, "SENT", "system")
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "m1", ThreadID: "thr1", LabelIDs: []string{fixturegmail.LabelInbox},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "Alice <alice@example.test>"},
			{Name: "To", Value: "me@example.test"},
			{Name: "Subject", Value: "seed"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:04:05 -0700"},
			{Name: "Message-ID", Value: "<seed@x>"},
		},
		Raw: []byte("Subject: seed\r\nMessage-ID: <seed@x>\r\n\r\nbody"),
	})
	return fx
}

// startEngine opens a store and runs an engine against the fixture.
func startEngine(t *testing.T, fx *fixturegmail.Server, mode string, interval time.Duration) (*store.Store, *sync.Engine) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	client, err := gmailapi.New(context.Background(), gmailapi.Options{
		Endpoint:            fx.URL(),
		HTTPClient:          oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: fx.Token()})),
		QuotaUnitsPerSecond: -1,
	})
	if err != nil {
		t.Fatalf("gmailapi.New: %v", err)
	}
	ni := storeNative{st: st, account: account}
	eng := sync.New(sync.Config{
		Account: account,
		NewBackend: func() mb.Backend {
			return gmailapi.NewBackend(gmailapi.Config{
				Account: account, Client: client, Native: ni,
				WatchMode: mode, PubSubTopic: pushTopic,
			})
		},
		Interval:    interval,
		BatchSize:   50,
		Concurrency: 2,
	}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go eng.Run(ctx)
	waitUntil(t, 5*time.Second, "engine ready", eng.Ready)
	return st, eng
}

// emailCount reports how many emails the cache holds for the account.
func emailCount(t *testing.T, st *store.Store) int {
	t.Helper()
	emails, _, _, err := st.EmailsByID(context.Background(), account, nil, false)
	if err != nil {
		t.Fatalf("EmailsByID: %v", err)
	}
	return len(emails)
}

func newPushHandler(t *testing.T, st *store.Store, eng *sync.Engine) http.Handler {
	t.Helper()
	cfg, err := config.LoadReader(strings.NewReader(`
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/pushgate"

[[accounts]]
id = "` + account + `"
address = "fixture@example.test"
token = "` + clientToken + `"
backend = "gmail_api"

  [accounts.gmail_api]
  endpoint = "http://127.0.0.1:1/"
  token = "fixture-token"
  watch = "pubsub"
  pubsub_topic = "` + pushTopic + `"
  push_allow_plain = true
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	tokens := auth.NewTokens(map[string]string{account: clientToken})
	verifier := gmailapi.NewPushVerifier(gmailapi.PushConfig{PlainSecret: clientToken})
	return httpapi.New(cfg, tokens, st, nil, push.New(), nil, nil,
		func(string) { eng.Kick() }, nil,
		httpapi.WithGmailPush(map[string]httpapi.PushVerifier{account: verifier}))
}

// postPush sends one Pub/Sub envelope to the running handler with the given
// shared secret ("" = forged).
func postPush(t *testing.T, h http.Handler, secret string) int {
	t.Helper()
	data := base64.StdEncoding.EncodeToString([]byte(`{"emailAddress":"fixture@example.test","historyId":"9999"}`))
	body := `{"message":{"data":"` + data + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/gmail/push/"+account, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Push-Token", secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestGmailPushEndToEnd(t *testing.T) {
	// A short watch window proves renewal re-arms within the test.
	fx := seedFixture(t, 1500*time.Millisecond)
	st, eng := startEngine(t, fx, "pubsub", 30*time.Second)
	h := newPushHandler(t, st, eng)

	waitUntil(t, 5*time.Second, "seed ingested", func() bool { return emailCount(t, st) == 1 })

	// A foreign change arrives in Gmail; a verified push must surface it
	// within the two-second budget (FR-S.7/FR-S.14).
	fx.ForeignAddMessage(fixturegmail.SeedMessage{
		ID: "m2", ThreadID: "thr2", LabelIDs: []string{fixturegmail.LabelInbox},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "Subject", Value: foreignSubj},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:05:05 -0700"},
			{Name: "Message-ID", Value: foreignMsgID},
		},
		Raw: []byte("Subject: " + foreignSubj + "\r\nMessage-ID: " + foreignMsgID + "\r\n\r\nbody"),
	})
	start := time.Now()
	if code := postPush(t, h, clientToken); code != http.StatusOK {
		t.Fatalf("verified push status = %d, want 200", code)
	}
	waitUntil(t, 2*time.Second, "pushed change visible <= 2s", func() bool { return emailCount(t, st) == 2 })
	t.Logf("push -> visible in %v", time.Since(start))

	// A forged push is rejected and does not nudge the engine.
	before := emailCount(t, st)
	if code := postPush(t, h, ""); code != http.StatusUnauthorized {
		t.Fatalf("forged push status = %d, want 401", code)
	}
	if got := emailCount(t, st); got != before {
		t.Fatalf("forged push changed the cache: %d -> %d", before, got)
	}

	// The watch renews before its window lapses.
	waitUntil(t, 5*time.Second, "watch renewed", func() bool { return fx.WatchCalls() >= 2 })
	if fx.WatchTopic() != pushTopic {
		t.Fatalf("watch topic = %q, want %q", fx.WatchTopic(), pushTopic)
	}
}

func TestGmailPollFallbackConverges(t *testing.T) {
	fx := seedFixture(t, 7*24*time.Hour)
	st, _ := startEngine(t, fx, "poll", 300*time.Millisecond)

	waitUntil(t, 5*time.Second, "seed ingested", func() bool { return emailCount(t, st) == 1 })

	// Poll mode never arms a watch; the ticker is the path.
	if fx.WatchCalls() != 0 {
		t.Fatalf("poll mode armed %d watches", fx.WatchCalls())
	}

	fx.ForeignAddMessage(fixturegmail.SeedMessage{
		ID: "m3", ThreadID: "thr3", LabelIDs: []string{fixturegmail.LabelInbox},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "Subject", Value: "polled"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:06:05 -0700"},
			{Name: "Message-ID", Value: "<poll1@x>"},
		},
		Raw: []byte("Subject: polled\r\nMessage-ID: <poll1@x>\r\n\r\nbody"),
	})
	waitUntil(t, 3*time.Second, "polled change visible", func() bool { return emailCount(t, st) == 2 })
}

// waitUntil polls cond until it holds or the budget expires.
func waitUntil(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

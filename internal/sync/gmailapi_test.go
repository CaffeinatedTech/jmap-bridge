package sync

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/CaffeinatedTech/jmap-bridge/internal/gmailapi"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// storeNative adapts the store's native_ids table to gmailapi's
// NativeIndex for the Gmail API engine tests (mirrors cmd's adapter).
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

func TestGmailAPIEngineBackfillAndForeignFlag(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	fx.SeedLabel(fixturegmail.LabelInbox, "INBOX", "system")
	fx.SeedLabel(fixturegmail.LabelSent, "SENT", "system")
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "m1", ThreadID: "thr1", LabelIDs: []string{fixturegmail.LabelInbox, fixturegmail.LabelUnread},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "Alice <alice@example.test>"},
			{Name: "To", Value: "me@example.test"},
			{Name: "Subject", Value: "Hello API"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:04:05 -0700"},
			{Name: "Message-ID", Value: "<api1@x>"},
		},
		Raw: raw("Hello API", "<api1@x>", "body"),
	})
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "m2", ThreadID: "thr2", LabelIDs: []string{fixturegmail.LabelInbox, fixturegmail.LabelStarred},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "Subject", Value: "Second"},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:04:05 -0700"},
			{Name: "Message-ID", Value: "<api2@x>"},
		},
		Raw: raw("Second", "<api2@x>", "body2"),
	})

	changes := make(chan struct{}, 64)
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Publish: func(string) {
			select {
			case changes <- struct{}{}:
			default:
			}
		},
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
	ni := storeNative{st: st, account: "gapi"}
	cfg := Config{
		Account: "gapi",
		NewBackend: func() mb.Backend {
			return gmailapi.NewBackend(gmailapi.Config{Account: "gapi", Client: client, Native: ni})
		},
		Interval:    150 * time.Millisecond,
		BatchSize:   50,
		Concurrency: 2,
	}
	eng := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)

	waitUntil(t, 5*time.Second, "engine ready", eng.Ready)

	mbs, _, err := st.Mailboxes(context.Background(), "gapi")
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	byName := map[string]bool{}
	for _, m := range mbs {
		byName[m.Name] = true
	}
	for _, want := range []string{"INBOX", "Sent", "All Mail"} {
		if !byName[want] {
			t.Fatalf("mailbox %q missing; have %v", want, byName)
		}
	}

	waitUntil(t, 5*time.Second, "two emails ingested", func() bool {
		emails, _, _, err := st.EmailsByID(context.Background(), "gapi", nil, false)
		return err == nil && len(emails) == 2
	})

	// A foreign flag change (web UI starring m1) must appear via the
	// history incremental pass.
	fx.ForeignModifyLabels("m1", []string{fixturegmail.LabelStarred}, nil)
	waitUntil(t, 5*time.Second, "starred flag visible", func() bool {
		emails, _, _, err := st.EmailsByID(context.Background(), "gapi", nil, false)
		if err != nil {
			return false
		}
		for _, e := range emails {
			if e.Keywords["$flagged"] {
				return true
			}
		}
		return false
	})
}

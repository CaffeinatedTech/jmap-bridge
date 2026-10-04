package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/CaffeinatedTech/jmap-bridge/internal/gmailapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// gapiEnv is the API-mode engine harness the write-path tests share.
type gapiEnv struct {
	fx  *fixturegmail.Server
	st  *store.Store
	eng *Engine
}

func newGmailAPIEnv(t *testing.T, fx *fixturegmail.Server) *gapiEnv {
	t.Helper()
	return newGmailAPIEnvWrapped(t, fx, nil)
}

// newGmailAPIEnvWrapped is newGmailAPIEnv with an optional wrapper around
// every backend session the engine builds (tests that need to observe a
// specific provider call).
func newGmailAPIEnvWrapped(t *testing.T, fx *fixturegmail.Server, wrap func(mb.Backend) mb.Backend) *gapiEnv {
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
	ni := storeNative{st: st, account: "gapi"}
	cfg := Config{
		Account: "gapi",
		NewBackend: func() mb.Backend {
			b := mb.Backend(gmailapi.NewBackend(gmailapi.Config{Account: "gapi", Client: client, Native: ni}))
			if wrap != nil {
				b = wrap(b)
			}
			return b
		},
		Interval:    150 * time.Millisecond,
		BatchSize:   50,
		Concurrency: 2,
	}
	eng := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go eng.Run(ctx)
	waitUntil(t, 5*time.Second, "engine ready", eng.Ready)
	return &gapiEnv{fx: fx, st: st, eng: eng}
}

func (env *gapiEnv) emails(t *testing.T) []*jmapapi.Email {
	t.Helper()
	list, _, notFound, err := env.st.EmailsByID(context.Background(), "gapi", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	if len(notFound) > 0 {
		t.Fatalf("unexpected notFound: %v", notFound)
	}
	return list
}

func (env *gapiEnv) mailboxID(t *testing.T, name string) string {
	t.Helper()
	mbs, _, err := env.st.Mailboxes(context.Background(), "gapi")
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	for _, m := range mbs {
		if m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("mailbox %q not found", name)
	return ""
}

func gapiSeedLabels(fx *fixturegmail.Server) {
	fx.SeedLabel(fixturegmail.LabelInbox, "INBOX", "system")
	fx.SeedLabel(fixturegmail.LabelSent, "SENT", "system")
	fx.SeedLabel(fixturegmail.LabelDraft, "Drafts", "system")
	fx.SeedLabel(fixturegmail.LabelTrash, "Trash", "system")
	fx.SeedLabel(fixturegmail.LabelSpam, "Spam", "system")
	fx.SeedLabel("Label_work", "Work", "user")
}

func TestGmailAPIEngineWritePath(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
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
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()

	waitUntil(t, 5*time.Second, "email ingested", func() bool { return len(env.emails(t)) == 1 })
	emailID := env.emails(t)[0].ID

	// Mark read ($seen add removes UNREAD) and flagged in one patch.
	if err := env.eng.ApplyEmailPatch(ctx, "gapi", emailID, jmapapi.EmailPatch{
		KeywordAdd: []string{"$seen", "$flagged"},
	}); err != nil {
		t.Fatalf("ApplyEmailPatch keywords: %v", err)
	}
	labels, _ := fx.MessageLabels("m1")
	if !contains(labels, fixturegmail.LabelStarred) || contains(labels, fixturegmail.LabelUnread) {
		t.Fatalf("fixture labels after keyword patch = %v", labels)
	}
	got := env.emails(t)[0]
	if !got.Keywords["$seen"] || !got.Keywords["$flagged"] {
		t.Fatalf("store keywords after patch = %v", got.Keywords)
	}

	// File into the user label (a membership add on the label model).
	workID := env.mailboxID(t, "Work")
	if err := env.eng.ApplyEmailPatch(ctx, "gapi", emailID, jmapapi.EmailPatch{
		MailboxAdd: []string{workID},
	}); err != nil {
		t.Fatalf("ApplyEmailPatch mailbox: %v", err)
	}
	labels, _ = fx.MessageLabels("m1")
	if !contains(labels, "Label_work") {
		t.Fatalf("fixture labels after membership = %v", labels)
	}

	// Destroy permanently removes the message (no trash routing in the API).
	if err := env.eng.DestroyEmails(ctx, "gapi", emailID); err != nil {
		t.Fatalf("DestroyEmails: %v", err)
	}
	if _, ok := fx.MessageLabels("m1"); ok {
		t.Fatal("fixture still holds the destroyed message")
	}
	waitUntil(t, 5*time.Second, "email tombstoned", func() bool { return len(env.emails(t)) == 0 })
}

func TestGmailAPIEngineRefusesUnsupportedKeyword(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "m1", ThreadID: "thr1", LabelIDs: []string{fixturegmail.LabelInbox},
		InternalDate: time.Now().UnixMilli(),
		Headers:      []fixturegmail.Header{{Name: "Message-ID", Value: "<api1@x>"}, {Name: "Subject", Value: "Hello"}},
		Raw:          raw("Hello", "<api1@x>", "body"),
	})
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()
	waitUntil(t, 5*time.Second, "email ingested", func() bool { return len(env.emails(t)) == 1 })
	emailID := env.emails(t)[0].ID

	err := env.eng.ApplyEmailPatch(ctx, "gapi", emailID, jmapapi.EmailPatch{KeywordAdd: []string{"$answered"}})
	var kwErr *jmapapi.KeywordError
	if !errors.As(err, &kwErr) {
		t.Fatalf("err = %v, want KeywordError", err)
	}
	if len(kwErr.Keywords) != 1 || kwErr.Keywords[0] != "$answered" {
		t.Fatalf("blamed keywords = %v", kwErr.Keywords)
	}
}

func TestGmailAPIEngineCreateDraft(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()

	created, err := env.eng.CreateDraft(ctx, "gapi", jmapapi.DraftSpec{
		To:      []jmapapi.Address{{Email: "you@example.test"}},
		Subject: "A draft",
		Parts:   []jmapapi.DraftPart{{Type: "text/plain", Text: "draft body"}},
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if created.ID == "" {
		t.Fatal("no created id")
	}
	drafts := fx.DraftIDs()
	if len(drafts) != 1 {
		t.Fatalf("fixture drafts = %v, want 1", drafts)
	}
	// The draft handle is persisted against the message's synthetic uid.
	copies, err := env.st.EmailCopies(ctx, "gapi", created.ID)
	if err != nil {
		t.Fatalf("EmailCopies: %v", err)
	}
	if len(copies) == 0 {
		t.Fatal("draft has no stored copy")
	}
	draftID, err := env.st.DraftIDByUID(ctx, "gapi", copies[0].UID)
	if err != nil {
		t.Fatalf("DraftIDByUID: %v", err)
	}
	if draftID != drafts[0] {
		t.Fatalf("stored draft id = %q, fixture = %q", draftID, drafts[0])
	}
}

func TestGmailAPIEngineCreateDraftMultiMailbox(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()

	inbox := env.mailboxID(t, "INBOX")
	work := env.mailboxID(t, "Work")
	created, err := env.eng.CreateDraft(ctx, "gapi", jmapapi.DraftSpec{
		MailboxIDs: []string{inbox, work},
		To:         []jmapapi.Address{{Email: "you@example.test"}},
		Subject:    "multi mailbox",
		Parts:      []jmapapi.DraftPart{{Type: "text/plain", Text: "body"}},
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	copies, err := env.st.EmailCopies(ctx, "gapi", created.ID)
	if err != nil {
		t.Fatalf("EmailCopies: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d, want 2 (INBOX + Work)", len(copies))
	}
	native, ok, err := env.st.NativeByUID(ctx, "gapi", copies[0].UID)
	if err != nil || !ok {
		t.Fatalf("native for uid %d: %q ok=%v err=%v", copies[0].UID, native, ok, err)
	}
	labels, ok := fx.MessageLabels(native)
	if !ok {
		t.Fatalf("created message %s missing on the server", native)
	}
	if !contains(labels, fixturegmail.LabelInbox) || !contains(labels, "Label_work") {
		t.Fatalf("labels = %v, want INBOX + Work", labels)
	}
}

func TestGmailAPIEngineImportEmail(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()

	blobID, err := env.st.PutBlob(ctx, "gapi", "message/rfc822", raw("Imported API", "<api-imp@x>", "imported body"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	inbox := env.mailboxID(t, "INBOX")
	work := env.mailboxID(t, "Work")

	created, err := env.eng.ImportEmail(ctx, "gapi", jmapapi.ImportSpec{
		BlobID:     blobID,
		MailboxIDs: []string{inbox, work},
		Keywords:   map[string]bool{"$seen": true, "$flagged": true},
	})
	if err != nil {
		t.Fatalf("ImportEmail: %v", err)
	}
	if created.ID == "" || created.BlobID != blobID {
		t.Fatalf("created = %+v", created)
	}

	// The one Gmail message carries both labels, seen and flagged.
	copies, err := env.st.EmailCopies(ctx, "gapi", created.ID)
	if err != nil {
		t.Fatalf("EmailCopies: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d, want 2 (INBOX + Work)", len(copies))
	}
	for _, c := range copies {
		native, ok, err := env.st.NativeByUID(ctx, "gapi", c.UID)
		if err != nil || !ok || native == "" {
			t.Fatalf("native for uid %d: %q ok=%v err=%v", c.UID, native, ok, err)
		}
		labels, ok := fx.MessageLabels(native)
		if !ok {
			t.Fatalf("imported message %s missing on the server", native)
		}
		if contains(labels, fixturegmail.LabelUnread) || !contains(labels, fixturegmail.LabelStarred) {
			t.Fatalf("labels = %v, want $seen + $flagged", labels)
		}
		if !contains(labels, fixturegmail.LabelInbox) || !contains(labels, "Label_work") {
			t.Fatalf("labels = %v, want INBOX + Work", labels)
		}
	}
}

func TestGmailAPIEngineMailboxSet(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	env := newGmailAPIEnv(t, fx)
	ctx := context.Background()

	id, err := env.eng.CreateMailbox(ctx, "gapi", "Projects", "", 0)
	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if _, ok := fx.LabelIDByName("Projects"); !ok {
		t.Fatal("created label not on the server")
	}
	if err := env.eng.RenameMailbox(ctx, "gapi", id, "Archive", ""); err != nil {
		t.Fatalf("RenameMailbox: %v", err)
	}
	if _, ok := fx.LabelIDByName("Archive"); !ok {
		t.Fatal("renamed label not on the server")
	}
	// onDestroyRemoveEmails=true is refused by name in API mode.
	if err := env.eng.DestroyMailbox(ctx, "gapi", id, true); !errors.Is(err, jmapapi.ErrOnDestroyRemoveEmails) {
		t.Fatalf("DestroyMailbox(removeEmails=true) err = %v, want ErrOnDestroyRemoveEmails", err)
	}
	if err := env.eng.DestroyMailbox(ctx, "gapi", id, false); err != nil {
		t.Fatalf("DestroyMailbox(removeEmails=false): %v", err)
	}
	if _, ok := fx.LabelIDByName("Archive"); ok {
		t.Fatal("label still on the server after destroy")
	}
}

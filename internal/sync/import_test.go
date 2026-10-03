package sync

// Email/import backend tests (RFC 8621 §4.8): raw bytes from the blob
// store land on the server and in the cache, with keywords, receivedAt
// and multi-mailbox membership verified through the store and a second
// IMAP session.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

func awaitInbox(t *testing.T, env *testEnv) string {
	t.Helper()
	waitUntil(t, 10*time.Second, "inbox discovered", func() bool {
		return mailboxBy(t, env, func(m *jmapapi.Mailbox) bool { return m.Role == "inbox" }) != nil
	})
	return inboxID(t, env)
}

func TestImportEmailRoundTrip(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	inbox := awaitInbox(t, env)

	rawMsg := raw("Imported", "<imported@example.test>", "imported body")
	blobID, err := env.st.PutBlob(context.Background(), "acct", "message/rfc822", rawMsg)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	received := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	created, err := env.eng.ImportEmail(context.Background(), "acct", jmapapi.ImportSpec{
		BlobID:     blobID,
		MailboxIDs: []string{inbox},
		Keywords:   map[string]bool{"$seen": true},
		ReceivedAt: received,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if created.ID == "" || created.BlobID != blobID || created.ThreadID == "" || created.Size <= 0 {
		t.Fatalf("created = %+v", created)
	}

	e := emailByID(t, env, created.ID)
	if e == nil {
		t.Fatal("imported email not in store")
	}
	if e.Subject != "Imported" || len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != inbox {
		t.Errorf("email = %#v", e)
	}
	if !e.Keywords["$seen"] {
		t.Errorf("keywords = %v, want $seen", e.Keywords)
	}
	if !e.ReceivedAt.Equal(received) {
		t.Errorf("receivedAt = %v, want %v", e.ReceivedAt, received)
	}
	if e.BlobID != blobID {
		t.Errorf("stored blobId = %q, want %q", e.BlobID, blobID)
	}

	// The server holds exactly one copy, flagged \Seen (second session).
	got := uidsVia(t, env, "INBOX")
	if len(got) != 1 {
		t.Fatalf("INBOX uids = %v, want one", got)
	}
	if flags := flagsVia(t, env, "INBOX", got[0]); !hasFlagName(flags, `\Seen`) {
		t.Errorf("flags = %v, want \\Seen", flags)
	}
}

func TestImportEmailMultiMailboxFilesExtras(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	inbox := awaitInbox(t, env)

	env.fx.CreateFolder("Imports")
	env.eng.requestPass("")
	waitUntil(t, 10*time.Second, "imports discovered", func() bool {
		return mailboxBy(t, env, func(m *jmapapi.Mailbox) bool { return m.Name == "Imports" }) != nil
	})
	imports := mailboxBy(t, env, func(m *jmapapi.Mailbox) bool { return m.Name == "Imports" }).ID

	rawMsg := raw("Multi", "<multi@example.test>", "multi body")
	blobID, err := env.st.PutBlob(context.Background(), "acct", "message/rfc822", rawMsg)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	created, err := env.eng.ImportEmail(context.Background(), "acct", jmapapi.ImportSpec{
		BlobID:     blobID,
		MailboxIDs: []string{inbox, imports},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	e := emailByID(t, env, created.ID)
	if e == nil || len(e.MailboxIDs) != 2 {
		t.Fatalf("mailboxIds = %#v, want both", e)
	}
	if got := uidsVia(t, env, "INBOX"); len(got) != 1 {
		t.Errorf("INBOX uids = %v, want one", got)
	}
	if got := uidsVia(t, env, "Imports"); len(got) != 1 {
		t.Errorf("Imports uids = %v, want one", got)
	}
}

func TestImportEmailErrors(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	inbox := awaitInbox(t, env)
	ctx := context.Background()

	_, err := env.eng.ImportEmail(ctx, "acct", jmapapi.ImportSpec{
		BlobID: "no-such-blob", MailboxIDs: []string{inbox},
	})
	if !errors.Is(err, jmapapi.ErrBlobNotFound) {
		t.Errorf("bad blob err = %v, want ErrBlobNotFound", err)
	}

	blobID, err := env.st.PutBlob(ctx, "acct", "message/rfc822", raw("X", "<x@example.test>", "x"))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	_, err = env.eng.ImportEmail(ctx, "acct", jmapapi.ImportSpec{
		BlobID: blobID, MailboxIDs: []string{"no-such-mailbox"},
	})
	if !errors.Is(err, jmapapi.ErrUnknownMailbox) {
		t.Errorf("unknown mailbox err = %v, want ErrUnknownMailbox", err)
	}
}

package sync

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/test/wireimap"
)

// The Gmail write-path tests run the engine's Backend against a raw
// wire fake (internal/imapdrv's fake lives in that package's tests), so
// the exact commands Gmail receives are asserted, not assumed.

// gmFolders seeds the Gmail-shaped mailbox set: INBOX, the implicit All
// Mail (archive role), and Trash.
func gmFolders() []store.Folder {
	return []store.Folder{
		{Name: "INBOX", Delim: '/', Role: "inbox", UIDValidity: 7},
		{Name: "[Gmail]/All Mail", Delim: '/', Role: "archive", Implicit: true, UIDValidity: 7},
		{Name: "[Gmail]/Trash", Delim: '/', Role: "trash", UIDValidity: 7},
		{Name: "[Gmail]/Sent Mail", Delim: '/', Role: "sent", UIDValidity: 7},
	}
}

func gmStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SyncFolders(context.Background(), "acct", gmFolders()); err != nil {
		t.Fatal(err)
	}
	return st
}

func gmSeed(t *testing.T, st *store.Store, uid uint32, folders ...string) string {
	t.Helper()
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rec := store.MessageRec{
		UID: uid, UIDValidity: 7, ReceivedAt: at, Size: 100, SharedUIDs: true,
		HeadersJSON: `{"subject":"hello","from":[],"to":[]}`,
		Subject:     "hello", SubjectL: "hello", FromL: "a@example.test", ToL: "me@example.test",
		MessageIDs: []string{"<gm@example>"},
		Structure:  `{"partId":"1","type":"text/plain","size":100}`,
	}
	for _, folder := range folders {
		if err := st.PutMessages(context.Background(), "acct", folder, []store.MessageRec{rec}); err != nil {
			t.Fatal(err)
		}
	}
	emails, _, _, err := st.EmailsByID(context.Background(), "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return emails[0].ID
}

// gmEngine builds an engine whose writer points at the fake wire server
// and is already marked Gmail. Nothing runs: the Backend methods are
// exercised directly. The fake is returned so tests can assert the
// exact commands the writer issued.
func gmEngine(t *testing.T, st *store.Store, handle func(tag, line string) []string) (*Engine, *wireimap.Server) {
	t.Helper()
	fake := wireimap.Start(t, "CAPABILITY IMAP4rev1", handle)
	cfg := Config{
		Account: "acct",
		IMAP: imapdrv.Config{
			Host: fake.Host(), Port: fake.Port(), Username: "test", Password: "pw",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	e := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.wr.noteGmail(true)
	return e, fake
}

// The mailbox ids the seeded folders got.
func gmMailboxIDs(t *testing.T, st *store.Store) (inbox, all, trash string) {
	t.Helper()
	mbs, _, err := st.Mailboxes(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range mbs {
		switch mb.Path {
		case "INBOX":
			inbox = mb.ID
		case "[Gmail]/All Mail":
			all = mb.ID
		case "[Gmail]/Trash":
			trash = mb.ID
		}
	}
	return
}

// The archive flow (FR-M.18): removing INBOX and adding the archive
// mailbox issues exactly one label STORE — the \Inbox removal — and no
// copy, because All Mail membership is implicit.
func TestGmailArchivePatchRemovesInboxOnly(t *testing.T) {
	st := gmStore(t)
	id := gmSeed(t, st, 5)
	inboxID, allID, _ := gmMailboxIDs(t, st)
	ctx := context.Background()

	var stores int
	e, fake := gmEngine(t, st, func(tag, line string) []string {
		switch {
		case strings.Contains(line, "CAPABILITY"):
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1 UIDPLUS"}
		case strings.Contains(line, " SELECT "):
			return []string{
				"* 1 EXISTS",
				"* OK [UIDVALIDITY 7] uv",
				"* OK [UIDNEXT 9] un",
				tag + " OK [READ-WRITE] selected",
			}
		case strings.Contains(line, "UNSELECT"):
			return []string{tag + " OK"}
		case strings.Contains(line, "UID STORE"):
			stores++
		}
		return nil
	})
	// The engine's writer must dial the fake: point it at the real port.
	if err := e.ApplyEmailPatch(ctx, "acct", id, jmapapi.EmailPatch{
		MailboxAdd:    []string{allID},
		MailboxRemove: []string{inboxID},
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if stores != 1 {
		t.Fatalf("%d STORE commands, want exactly 1 label store", stores)
	}
	line := fake.LineMatching("UID STORE")
	if !strings.Contains(line, `-X-GM-LABELS.SILENT (\Inbox)`) {
		t.Fatalf("archive wrote %q, want the \\Inbox label removal", line)
	}
	if strings.Contains(line, "COPY") || strings.Contains(line, "MOVE") {
		t.Fatalf("archive used COPY/MOVE: %q", line)
	}
	// The local commit reflects the server's word: INBOX membership
	// gone, All Mail membership present (the implicit add).
	copies, err := st.EmailCopies(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].MailboxID != allID {
		t.Fatalf("memberships after archive = %#v", copies)
	}
}

// A membership removal targeting All Mail is refused: the Gmail server
// model cannot express it, and the cache must not pretend otherwise.
func TestGmailRefusesRemoveFromAllMail(t *testing.T) {
	st := gmStore(t)
	id := gmSeed(t, st, 5, "INBOX", "[Gmail]/All Mail")
	_, allID, _ := gmMailboxIDs(t, st)
	e, _ := gmEngine(t, st, func(tag, line string) []string {
		if strings.Contains(line, "CAPABILITY") {
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1"}
		}
		return nil
	})
	err := e.ApplyEmailPatch(context.Background(), "acct", id, jmapapi.EmailPatch{
		MailboxRemove: []string{allID},
	})
	if err == nil || !strings.Contains(err.Error(), "managed by the server") {
		t.Fatalf("removal from All Mail = %v, want the implicit-membership refusal", err)
	}
}

// Destroy on Gmail is permanent only through Trash: copies are moved
// there (MOVE), then expunged where expunge means expunge (FR-M.18).
func TestGmailDestroyRoutesThroughTrash(t *testing.T) {
	st := gmStore(t)
	id := gmSeed(t, st, 5)
	_, _, trashID := gmMailboxIDs(t, st)
	_ = trashID
	ctx := context.Background()

	var moved, expunged bool
	e, fake := gmEngine(t, st, func(tag, line string) []string {
		switch {
		case strings.Contains(line, "CAPABILITY"):
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1 UIDPLUS MOVE"}
		case strings.Contains(line, " SELECT "):
			return []string{
				"* 1 EXISTS",
				"* OK [UIDVALIDITY 7] uv",
				"* OK [UIDNEXT 9] un",
				tag + " OK [READ-WRITE] selected",
			}
		case strings.Contains(line, "UNSELECT"):
			return []string{tag + " OK"}
		case strings.Contains(line, "UID MOVE"):
			moved = true
			return []string{
				"* OK [COPYUID 7 5 5] moved",
				"* 1 EXPUNGE",
			}
		case strings.Contains(line, "UID EXPUNGE"):
			expunged = true
		}
		return nil
	})
	if err := e.DestroyEmails(ctx, "acct", id); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !moved || !expunged {
		t.Fatalf("moved=%v expunged=%v", moved, expunged)
	}
	// The move targeted Trash, not somewhere else.
	if line := fake.LineMatching("UID MOVE"); !strings.Contains(line, `[Gmail]/Trash`) {
		t.Fatalf("move line = %q", line)
	}
	exists, live, err := st.EmailState(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || live {
		t.Fatal("destroy did not tombstone the email")
	}
}

// The grace window (FR-S.12): a uid the engine itself just wrote is
// spared from tombstoning on the next reconcile, while foreign expunges
// are not.
func TestGraceWindowSparesOwnWrites(t *testing.T) {
	st := gmStore(t)
	e, _ := gmEngine(t, st, func(tag, line string) []string {
		return nil
	})
	e.recordOwnWrite("INBOX", []uint32{5})
	if got := e.graceFilter("INBOX", []uint32{5, 6}); len(got) != 1 || got[0] != 6 {
		t.Fatalf("grace filter = %v, want only the foreign uid 6", got)
	}
	// Another folder's writes do not shield this folder's uids.
	if got := e.graceFilter("Sent", []uint32{5}); len(got) != 1 {
		t.Fatalf("cross-folder grace leaked: %v", got)
	}
	// Pruning: writes older than twice the window vanish from the map.
	e.ownMu.Lock()
	e.ownWrites["INBOX"][5] = time.Now().Add(-2 * graceWindow)
	e.ownMu.Unlock()
	e.recordOwnWrite("Sent", []uint32{9})
	e.ownMu.Lock()
	_, still := e.ownWrites["INBOX"][5]
	e.ownMu.Unlock()
	if still {
		t.Fatal("stale own-write entry was not pruned")
	}
	if got := e.graceFilter("INBOX", []uint32{5}); len(got) != 1 {
		t.Fatal("expired grace still shields the uid")
	}
}

// gmLabelFor's mapping table, including the two system folders
// SPECIAL-USE does not cover. The implicit mailbox is decided by the
// store's implicit flag before gmLabelFor is consulted, so the archive
// role has no row here.
func TestGmLabelFor(t *testing.T) {
	cases := []struct{ role, path, want string }{
		{"inbox", "INBOX", `\Inbox`},
		{"sent", "[Gmail]/Sent Mail", `\Sent`},
		{"drafts", "[Gmail]/Drafts", `\Drafts`},
		{"trash", "[Gmail]/Trash", `\Trash`},
		{"junk", "[Gmail]/Spam", `\Spam`},
		{"", "receipts", "receipts"},
		{"", "work/2026", "work/2026"},
		{"", "[Gmail]/Starred", `\Starred`},
		{"", "[Gmail]/Important", `\Important`},
	}
	for _, tc := range cases {
		if got := gmLabelFor(tc.role, tc.path); got != tc.want {
			t.Errorf("gmLabelFor(%q,%q) = %q, want %q", tc.role, tc.path, got, tc.want)
		}
	}
}

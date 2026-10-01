package sync

import (
	"context"
	"fmt"
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

// gmUIDValidity is the per-folder uid space the fixture models. Real
// Gmail gives every label its own IMAP mailbox with its own UIDVALIDITY
// and uid sequence, so one message has a different uid in each folder.
// Sharing one value would let a folder/uid mixup pass silently.
var gmUIDValidity = map[string]uint32{
	"INBOX":             2,
	"[Gmail]/All Mail":  1,
	"[Gmail]/Trash":     11,
	"[Gmail]/Sent Mail": 4,
	"[Gmail]/Drafts":    13,
}

// gmFolders seeds the Gmail-shaped mailbox set: INBOX, the implicit All
// Mail (archive role), and Trash.
func gmFolders() []store.Folder {
	return []store.Folder{
		{Name: "INBOX", Delim: '/', Role: "inbox", UIDValidity: gmUIDValidity["INBOX"]},
		{Name: "[Gmail]/All Mail", Delim: '/', Role: "archive", Implicit: true, UIDValidity: gmUIDValidity["[Gmail]/All Mail"]},
		{Name: "[Gmail]/Trash", Delim: '/', Role: "trash", UIDValidity: gmUIDValidity["[Gmail]/Trash"]},
		{Name: "[Gmail]/Sent Mail", Delim: '/', Role: "sent", UIDValidity: gmUIDValidity["[Gmail]/Sent Mail"]},
		{Name: "[Gmail]/Drafts", Delim: '/', Role: "drafts", UIDValidity: gmUIDValidity["[Gmail]/Drafts"]},
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

// gmSeedPerFolder ingests one message into each named folder, each under
// that folder's own uidvalidity, and returns the JMAP id the copies
// share. Per-folder uids are the point: Gmail does not share them.
func gmSeedPerFolder(t *testing.T, st *store.Store, uids map[string]uint32) string {
	t.Helper()
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for folder, uid := range uids {
		rec := store.MessageRec{
			UID: uid, UIDValidity: gmUIDValidity[folder], ReceivedAt: at, Size: 100,
			HeadersJSON: `{"subject":"hello","from":[],"to":[]}`,
			Subject:     "hello", SubjectL: "hello", FromL: "a@example.test", ToL: "me@example.test",
			MessageIDs: []string{"<gm@example>"},
			Structure:  `{"partId":"1","type":"text/plain","size":100}`,
		}
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

// gmSeed is gmSeedPerFolder with one uid reused in every named folder.
func gmSeed(t *testing.T, st *store.Store, uid uint32, folders ...string) string {
	t.Helper()
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	uids := make(map[string]uint32, len(folders))
	for _, folder := range folders {
		uids[folder] = uid
	}
	return gmSeedPerFolder(t, st, uids)
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
// copy, because All Mail membership is implicit. The STORE must be
// sourced from a copy the patch is not removing (All Mail), with that
// folder's own uid: Gmail answers OK but declines to remove the selected
// folder's own label, which is exactly the archive no-op this pins.
func TestGmailArchivePatchRemovesInboxOnly(t *testing.T) {
	st := gmStore(t)
	const inboxUID, allUID = 203000, 331743
	id := gmSeedPerFolder(t, st, map[string]uint32{
		"INBOX":            inboxUID,
		"[Gmail]/All Mail": allUID,
	})
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
	// Folder and uid must be paired: the STORE carries All Mail's uid,
	// not the removed INBOX copy's, and it is issued with All Mail
	// selected. A mismatch is what the live gate caught.
	if !strings.Contains(line, fmt.Sprintf("UID STORE %d", allUID)) {
		t.Fatalf("archive STORE = %q, want it to use All Mail's own uid %d", line, allUID)
	}
	if strings.Contains(line, fmt.Sprintf("%d", inboxUID)) {
		t.Fatalf("archive STORE = %q, leaked the removed INBOX uid %d", line, inboxUID)
	}
	if sel := selectedBefore(fake.Lines(), "UID STORE"); !strings.Contains(sel, "All Mail") {
		t.Fatalf("archive selected %q, want [Gmail]/All Mail", sel)
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

// selectedBefore returns the last SELECT/EXAMINE line the client sent
// before the first line containing cmd.
func selectedBefore(lines []string, cmd string) string {
	sel := ""
	for _, l := range lines {
		if strings.Contains(l, " SELECT ") || strings.Contains(l, " EXAMINE ") {
			sel = l
		}
		if strings.Contains(l, cmd) {
			return sel
		}
	}
	return sel
}

// Filing a draft into Sent must still work before All Mail has synced the
// draft: the only copy with a known uid is the Drafts folder the patch
// removes, so the label write falls back to it — folder and uid stay
// paired — rather than refusing the patch outright.
func TestGmailFilesDraftFromRemovedFolder(t *testing.T) {
	st := gmStore(t)
	const draftUID = 42
	id := gmSeedPerFolder(t, st, map[string]uint32{"[Gmail]/Drafts": draftUID})
	mbs, _, err := st.Mailboxes(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	var draftsID, sentID string
	for _, mb := range mbs {
		switch mb.Role {
		case "drafts":
			draftsID = mb.ID
		case "sent":
			sentID = mb.ID
		}
	}
	e, fake := gmEngine(t, st, func(tag, line string) []string {
		switch {
		case strings.Contains(line, "CAPABILITY"):
			return []string{"* CAPABILITY IMAP4rev1 X-GM-EXT-1 UIDPLUS"}
		case strings.Contains(line, " SELECT "):
			return []string{
				"* 1 EXISTS",
				"* OK [UIDVALIDITY 13] uv",
				"* OK [UIDNEXT 9] un",
				tag + " OK [READ-WRITE] selected",
			}
		case strings.Contains(line, "UNSELECT"):
			return []string{tag + " OK"}
		}
		return nil
	})
	if err := e.ApplyEmailPatch(context.Background(), "acct", id, jmapapi.EmailPatch{
		MailboxAdd:    []string{sentID},
		MailboxRemove: []string{draftsID},
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	add := fake.LineMatching("+X-GM-LABELS")
	if !strings.Contains(add, fmt.Sprintf(`UID STORE %d +X-GM-LABELS.SILENT (\Sent)`, draftUID)) {
		t.Fatalf("add line = %q, want the \\Sent add on the draft's own uid", add)
	}
	rem := fake.LineMatching("-X-GM-LABELS")
	if !strings.Contains(rem, fmt.Sprintf(`UID STORE %d -X-GM-LABELS.SILENT (\Drafts)`, draftUID)) {
		t.Fatalf("remove line = %q, want the \\Drafts removal on the draft's own uid", rem)
	}
	if sel := selectedBefore(fake.Lines(), "+X-GM-LABELS"); !strings.Contains(sel, "Drafts") {
		t.Fatalf("filing selected %q, want [Gmail]/Drafts", sel)
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

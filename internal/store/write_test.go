package store

import (
	"context"
	"testing"
	"time"
)

// seedOne syncs the standard folders and puts one unread message in
// INBOX, returning its id.
func seedOne(t *testing.T, s *Store, uid uint32, msgid, subject string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatalf("sync folders: %v", err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{
		mkRec(uid, msgid, subject, "a@example.test", nil, at),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range emails {
		if e.Subject == subject {
			return e.ID
		}
	}
	t.Fatalf("seeded message %q not found", subject)
	return ""
}

func mailboxByName(t *testing.T, s *Store, name string) (id string, total, unread int) {
	t.Helper()
	mbs, _, err := s.Mailboxes(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range mbs {
		if mb.Path == name {
			return mb.ID, mb.TotalEmails, mb.UnreadEmails
		}
	}
	t.Fatalf("mailbox %q not found", name)
	return "", 0, 0
}

func TestCommitKeywordPatchMovesCountsAndState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedOne(t, s, 1, "<kw@example>", "keywords")
	before, err := s.EmailStateString(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, unread := mailboxByName(t, s, "INBOX"); unread != 1 {
		t.Fatalf("unread before = %d, want 1", unread)
	}

	changed, err := s.CommitPatch(ctx, "acct", id, []string{"$seen", "$flagged"}, nil, nil, nil)
	if err != nil || !changed {
		t.Fatalf("commit add: changed=%v err=%v", changed, err)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", []string{id}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !emails[0].Keywords["$seen"] || !emails[0].Keywords["$flagged"] {
		t.Errorf("keywords = %#v, want $seen and $flagged", emails[0].Keywords)
	}
	if _, _, unread := mailboxByName(t, s, "INBOX"); unread != 0 {
		t.Errorf("unread after $seen = %d, want 0", unread)
	}
	after, _ := s.EmailStateString(ctx, "acct")
	if after == before {
		t.Error("email state did not advance after a keyword patch")
	}
	cs, err := s.Changes(ctx, "acct", "Email", before)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != id {
		t.Errorf("updated = %v, want [%s]", cs.Updated, id)
	}

	// Removal is the mirror image (FR-M.9: null removes).
	changed, err = s.CommitPatch(ctx, "acct", id, nil, []string{"$flagged", "$seen"}, nil, nil)
	if err != nil || !changed {
		t.Fatalf("commit remove: changed=%v err=%v", changed, err)
	}
	if _, _, unread := mailboxByName(t, s, "INBOX"); unread != 1 {
		t.Errorf("unread after removal = %d, want 1", unread)
	}

	// A patch that changes nothing must not move state (no SSE storm).
	still, _ := s.EmailStateString(ctx, "acct")
	changed, err = s.CommitPatch(ctx, "acct", id, nil, []string{"$flagged"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("no-op patch reported as changed")
	}
	again, _ := s.EmailStateString(ctx, "acct")
	if again != still {
		t.Errorf("state moved %s → %s on a no-op patch", still, again)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestCommitMembershipPatchMoveCopyAndLastRemoval(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedOne(t, s, 1, "<move@example>", "moving")
	inboxID, inboxTotal, _ := mailboxByName(t, s, "INBOX")
	archiveID, _, archiveUnread := mailboxByName(t, s, "Archive")
	if inboxTotal != 1 || archiveUnread != 0 {
		t.Fatalf("seed state: inbox=%d archive-unread=%d", inboxTotal, archiveUnread)
	}

	// Copy into Archive with the uid the server's COPYUID reported.
	if _, err := s.CommitPatch(ctx, "acct", id, nil, nil,
		[]MembershipAdd{{MailboxID: archiveID, UID: 42, UIDValidity: 7}}, nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	copies, err := s.EmailCopies(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %#v, want 2 memberships", copies)
	}
	var mapped bool
	for _, c := range copies {
		if c.MailboxID == archiveID && c.Folder == "Archive" && c.UID == 42 {
			mapped = true
		}
	}
	if !mapped {
		t.Errorf("archive copy missing uid mapping: %#v", copies)
	}
	if _, _, unread := mailboxByName(t, s, "Archive"); unread != 1 {
		t.Errorf("archive unread = %d, want 1", unread)
	}

	// Remove from INBOX: the message lives on in Archive.
	if _, err := s.CommitPatch(ctx, "acct", id, nil, nil, nil, []string{inboxID}); err != nil {
		t.Fatalf("remove inbox: %v", err)
	}
	if _, live, err := s.EmailState(ctx, "acct", id); err != nil || !live {
		t.Fatalf("email live after one removal: live=%v err=%v", live, err)
	}
	if _, total, _ := mailboxByName(t, s, "INBOX"); total != 0 {
		t.Errorf("inbox total = %d, want 0", total)
	}

	// Remove the last membership: tombstone, exactly as a foreign
	// expunge would have done.
	state, _ := s.EmailStateString(ctx, "acct")
	if _, err := s.CommitPatch(ctx, "acct", id, nil, nil, nil, []string{archiveID}); err != nil {
		t.Fatalf("remove archive: %v", err)
	}
	if _, live, err := s.EmailState(ctx, "acct", id); err != nil || live {
		t.Errorf("email still live after losing its last mailbox: live=%v err=%v", live, err)
	}
	cs, err := s.Changes(ctx, "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Destroyed) != 1 || cs.Destroyed[0] != id {
		t.Errorf("destroyed = %v, want [%s]", cs.Destroyed, id)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestCommitMembershipRestoreClearsRaceTombstone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedOne(t, s, 1, "<race@example>", "race")
	archiveID, _, _ := mailboxByName(t, s, "Archive")

	if err := s.CommitDestroy(ctx, "acct", []string{id}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// The sync engine can expunge-ahead of our commit; the copy we just
	// created must bring the email back rather than join a tombstone.
	if _, err := s.CommitPatch(ctx, "acct", id, nil, nil,
		[]MembershipAdd{{MailboxID: archiveID, UID: 5, UIDValidity: 7}}, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, live, err := s.EmailState(ctx, "acct", id); err != nil || !live {
		t.Fatalf("restored email live=%v err=%v", live, err)
	}
	copies, err := s.EmailCopies(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].MailboxID != archiveID {
		t.Errorf("copies = %#v, want the archive membership", copies)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestCommitDestroyDropsEveryMembership(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedOne(t, s, 1, "<dup@example>", "in two places")
	// The Sent copy must agree with the INBOX seed's flags (none): the
	// dedupe rule only merges copies whose server state matches.
	if err := s.PutMessages(ctx, "acct", "Sent", []MessageRec{
		mkRec(9, "<dup@example>", "in two places", "a@example.test", nil,
			time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Fatalf("second membership: %v", err)
	}
	copies, err := s.EmailCopies(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %#v, want 2", copies)
	}
	state, _ := s.EmailStateString(ctx, "acct")

	if err := s.CommitDestroy(ctx, "acct", []string{id}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	exists, live, err := s.EmailState(ctx, "acct", id)
	if err != nil || live {
		t.Errorf("email live after destroy: live=%v err=%v", live, err)
	}
	if !exists {
		t.Error("destroyed email lost its row: /changes could no longer replay it as a tombstone")
	}
	if got, err := s.EmailCopies(ctx, "acct", id); err != nil || len(got) != 0 {
		t.Errorf("copies after destroy = %#v (err %v), want none", got, err)
	}
	cs, err := s.Changes(ctx, "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Destroyed) != 1 {
		t.Errorf("destroyed = %v, want the email", cs.Destroyed)
	}
	// Destroying it again is a no-op success (FR-M.10), not an error.
	if err := s.CommitDestroy(ctx, "acct", []string{id, "no-such-id"}); err != nil {
		t.Errorf("second destroy: %v", err)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestCommitAppendMintsIDAndMapsUID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	rec := mkRec(11, "<draft@example>", "Draft subject", "me@example.test",
		[]string{`\Draft`, `\Seen`}, time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC))
	res, err := s.CommitAppend(ctx, "acct", "INBOX", rec)
	if err != nil {
		t.Fatalf("commit append: %v", err)
	}
	id := res.ID
	if id == "" {
		t.Fatal("no id minted")
	}
	if res.ThreadID == "" || res.Size != rec.Size {
		t.Errorf("created facts = thread %q size %d, want thread and size %d", res.ThreadID, res.Size, rec.Size)
	}
	if _, live, err := s.EmailState(ctx, "acct", id); err != nil || !live {
		t.Fatalf("appended email live=%v err=%v", live, err)
	}
	copies, err := s.EmailCopies(ctx, "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].UID != 11 || copies[0].Folder != "INBOX" {
		t.Errorf("copies = %#v, want INBOX uid 11", copies)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", []string{id}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !emails[0].Keywords["$draft"] || !emails[0].Keywords["$seen"] {
		t.Errorf("keywords = %#v, want $draft and $seen", emails[0].Keywords)
	}
	if emails[0].Subject != "Draft subject" || emails[0].ThreadID == "" {
		t.Errorf("email = subject %q thread %q", emails[0].Subject, emails[0].ThreadID)
	}
	if _, total, _ := mailboxByName(t, s, "INBOX"); total != 1 {
		t.Errorf("inbox total = %d, want 1", total)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestCommitMailboxRenameRewritesDescendants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	archiveID, _, _ := mailboxByName(t, s, "Archive")
	childID, _, _ := mailboxByName(t, s, "Archive/2026")
	before, _ := s.MailboxStateString(ctx, "acct")

	if err := s.CommitMailboxRename(ctx, "acct", archiveID, "Archive", "Old", "", '/', true); err != nil {
		t.Fatalf("rename: %v", err)
	}
	renamed, _, _ := mailboxByName(t, s, "Old")
	if renamed != archiveID {
		t.Errorf("renamed id = %s, want the original %s (ids survive a rename)", renamed, archiveID)
	}
	if moved, _, _ := mailboxByName(t, s, "Old/2026"); moved != childID {
		t.Errorf("descendant id = %s, want %s", moved, childID)
	}
	// The child's parent id is unchanged — only paths moved.
	mbs, _, err := s.Mailboxes(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range mbs {
		if mb.ID == childID && mb.ParentID != archiveID {
			t.Errorf("child parent = %s, want %s", mb.ParentID, archiveID)
		}
		if mb.Path == "Archive" || mb.Path == "Archive/2026" {
			t.Errorf("old path %q still present", mb.Name)
		}
	}
	after, _ := s.MailboxStateString(ctx, "acct")
	if after == before {
		t.Error("mailbox state did not advance after a rename")
	}
}

func TestMailboxLookups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	archiveID, _, _ := mailboxByName(t, s, "Archive")

	if got, err := s.MailboxIDByRole(ctx, "acct", "archive"); err != nil || got != archiveID {
		t.Errorf("by role = %q (err %v), want %q", got, err, archiveID)
	}
	if got, err := s.MailboxIDByRole(ctx, "acct", "trash"); err != nil || got != "" {
		t.Errorf("missing role = %q (err %v), want empty", got, err)
	}
	path, err := s.MailboxPath(ctx, "acct", archiveID)
	if err != nil || path != "Archive" {
		t.Errorf("path = %q (err %v), want Archive", path, err)
	}
	if id, err := s.MailboxIDByPath(ctx, "acct", "Archive/2026"); err != nil || id == "" {
		t.Errorf("id by path = %q (err %v), want the child id", id, err)
	}
	if _, err := s.MailboxPath(ctx, "acct", "nope"); err != ErrMailboxUnknown {
		t.Errorf("unknown path err = %v, want ErrMailboxUnknown", err)
	}
	if id, err := s.MailboxIDByPath(ctx, "acct", "nope"); err != nil || id != "" {
		t.Errorf("unknown path id = %q (err %v), want empty", id, err)
	}
}

// On shared-UID servers (Gmail) the same uid arrives through every
// folder it is labelled with: the second folder's copy must dedupe to
// the first email even when the Message-ID differs (or is absent),
// because it is the same server-side message (FR-S.10).
func TestPutMessagesSharedUIDDedupe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	first := mkRec(5, "<a@example>", "shared", "a@example.test", nil, at)
	first.SharedUIDs = true
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{first}); err != nil {
		t.Fatal(err)
	}
	before, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("after first folder: %d emails", len(before))
	}

	// A second folder reports the same uid with a different Message-ID:
	// still the same message on a shared-UID server.
	second := mkRec(5, "<b@example>", "shared", "a@example.test", []string{`\Seen`}, at)
	second.SharedUIDs = true
	if err := s.PutMessages(ctx, "acct", "Archive", []MessageRec{second}); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("shared-uid duplicate minted a second email (%d)", len(after))
	}
	if after[0].ID != before[0].ID {
		t.Fatal("deduped email changed id")
	}
	if !after[0].Keywords["$seen"] {
		t.Error("fetched flags were not applied to the shared email")
	}
	// Both folders are memberships of the one object.
	inboxID, inboxTotal, _ := mailboxByName(t, s, "INBOX")
	allID, allTotal, _ := mailboxByName(t, s, "Archive")
	if inboxTotal != 1 || allTotal != 1 {
		t.Errorf("counts: INBOX %d, All Mail %d", inboxTotal, allTotal)
	}
	_ = inboxID
	_ = allID
}

// LinkSharedUIDs adds the missing membership and mapping for a message
// the account already knows under its shared uid, without touching
// headers or keywords (the Gmail backfill's cheap half).
func TestLinkSharedUIDsAddsMembershipOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rec := mkRec(9, "<link@example>", "linked", "a@example.test", []string{`\Flagged`}, at)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{rec}); err != nil {
		t.Fatal(err)
	}
	before, _, _, _ := s.EmailsByID(ctx, "acct", nil, false)
	if err := s.LinkSharedUIDs(ctx, "acct", "Archive", 7, []uint32{9, 10}); err != nil {
		t.Fatal(err)
	}
	after, _, _, _ := s.EmailsByID(ctx, "acct", nil, false)
	if len(after) != len(before) {
		t.Fatalf("LinkSharedUIDs created an email (%d → %d)", len(before), len(after))
	}
	if !after[0].Keywords["$flagged"] {
		t.Error("keywords were touched by the link")
	}
	_, allTotal, _ := mailboxByName(t, s, "Archive")
	if allTotal != 1 {
		t.Errorf("All Mail total = %d, want 1", allTotal)
	}
}

// X-GM-THRID seeds the thread: two messages sharing only the thrid (no
// Message-ID linkage, different subjects) land in one thread, and the
// thread key is registered for later arrivals (FR-S.10).
func TestThreadDerivationUsesGmThrid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	a := mkRec(1, "<t1@example>", "Completely unrelated", "a@example.test", nil, at)
	a.GmThrid = 42
	b := mkRec(2, "<t2@example>", "Also unrelated", "a@example.test", nil, at)
	b.GmThrid = 42
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{a, b}); err != nil {
		t.Fatal(err)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 {
		t.Fatalf("%d emails", len(emails))
	}
	if emails[0].ThreadID != emails[1].ThreadID {
		t.Fatalf("thrid siblings split across threads: %s vs %s",
			emails[0].ThreadID, emails[1].ThreadID)
	}
	// And a third message with the same thrid joins the same thread.
	c := mkRec(3, "<t3@example>", "Third", "a@example.test", nil, at)
	c.GmThrid = 42
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{c}); err != nil {
		t.Fatal(err)
	}
	emails, _, _, _ = s.EmailsByID(ctx, "acct", nil, false)
	if len(emails) != 3 || emails[2].ThreadID != emails[0].ThreadID {
		t.Fatal("later thrid arrival did not join the thread")
	}
}

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
		if mb.Name == name {
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

	changed, err := s.CommitKeywordPatch(ctx, "acct", id, []string{"$seen", "$flagged"}, nil)
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
	changed, err = s.CommitKeywordPatch(ctx, "acct", id, nil, []string{"$flagged", "$seen"})
	if err != nil || !changed {
		t.Fatalf("commit remove: changed=%v err=%v", changed, err)
	}
	if _, _, unread := mailboxByName(t, s, "INBOX"); unread != 1 {
		t.Errorf("unread after removal = %d, want 1", unread)
	}

	// A patch that changes nothing must not move state (no SSE storm).
	still, _ := s.EmailStateString(ctx, "acct")
	changed, err = s.CommitKeywordPatch(ctx, "acct", id, nil, []string{"$flagged"})
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
	if err := s.CommitMembershipPatch(ctx, "acct", id,
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
	if err := s.CommitMembershipPatch(ctx, "acct", id, nil, []string{inboxID}); err != nil {
		t.Fatalf("remove inbox: %v", err)
	}
	if live, err := s.EmailLive(ctx, "acct", id); err != nil || !live {
		t.Fatalf("email live after one removal: live=%v err=%v", live, err)
	}
	if _, total, _ := mailboxByName(t, s, "INBOX"); total != 0 {
		t.Errorf("inbox total = %d, want 0", total)
	}

	// Remove the last membership: tombstone, exactly as a foreign
	// expunge would have done.
	state, _ := s.EmailStateString(ctx, "acct")
	if err := s.CommitMembershipPatch(ctx, "acct", id, nil, []string{archiveID}); err != nil {
		t.Fatalf("remove archive: %v", err)
	}
	if live, err := s.EmailLive(ctx, "acct", id); err != nil || live {
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
	if err := s.CommitMembershipPatch(ctx, "acct", id,
		[]MembershipAdd{{MailboxID: archiveID, UID: 5, UIDValidity: 7}}, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if live, err := s.EmailLive(ctx, "acct", id); err != nil || !live {
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
	if err := s.PutMessages(ctx, "acct", "Sent", []MessageRec{
		mkRec(9, "<dup@example>", "in two places", "a@example.test", []string{`\Seen`},
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
	if live, err := s.EmailLive(ctx, "acct", id); err != nil || live {
		t.Errorf("email live after destroy: live=%v err=%v", live, err)
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
	id, err := s.CommitAppend(ctx, "acct", "INBOX", rec)
	if err != nil {
		t.Fatalf("commit append: %v", err)
	}
	if id == "" {
		t.Fatal("no id minted")
	}
	if live, err := s.EmailLive(ctx, "acct", id); err != nil || !live {
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

	if err := s.CommitMailboxRename(ctx, "acct", archiveID, "Archive", "Old", "", '/'); err != nil {
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
		if mb.Name == "Archive" || mb.Name == "Archive/2026" {
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

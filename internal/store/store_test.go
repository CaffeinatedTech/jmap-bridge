package store

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(context.Background(), Options{DataDir: t.TempDir(), Logger: quiet})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testFolders() []Folder {
	return []Folder{
		{Name: "INBOX", Delim: '/', Role: "inbox", UIDValidity: 7},
		{Name: "Sent", Delim: '/', Role: "sent", UIDValidity: 7},
		{Name: "Archive", Delim: '/', Role: "archive", UIDValidity: 7},
		{Name: "Archive/2026", Delim: '/', UIDValidity: 7},
	}
}

// mkRec builds a MessageRec the way convert will: canonical headers
// JSON, lowercase mirrors, a one-part structure.
func mkRec(uid uint32, msgid, subject, from string, flags []string, at time.Time) MessageRec {
	ho := map[string]any{
		"subject": subject,
		"from":    []map[string]string{{"name": "Sender", "email": from}},
		"to":      []map[string]string{{"name": "Me", "email": "me@example.test"}},
	}
	raw, err := jsonMarshal(ho)
	if err != nil {
		panic(err)
	}
	hj := string(raw)
	lower := func(s string) string { return strings.ToLower(s) }
	return MessageRec{
		UID:         uid,
		UIDValidity: 7,
		Flags:       flags,
		ReceivedAt:  at,
		Size:        100 + int64(uid),
		HeadersJSON: hj,
		Subject:     subject,
		SubjectL:    lower(subject),
		FromL:       lower(from),
		ToL:         "me@example.test",
		MessageIDs:  []string{msgid},
		Structure:   `{"partId":"1","type":"text/plain","charset":"utf-8","size":100}`,
	}
}

func TestSyncFoldersHierarchyAndRoles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatalf("sync folders: %v", err)
	}
	mbs, state, err := s.Mailboxes(ctx, "acct")
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	if state == "0" {
		t.Fatal("mailbox state did not advance")
	}
	if len(mbs) != 4 {
		t.Fatalf("got %d mailboxes, want 4", len(mbs))
	}
	// Sort order: inbox first, then archive bucket, then sent.
	if mbs[0].Role != "inbox" || mbs[0].Name != "INBOX" {
		t.Errorf("first mailbox = %q (%q), want INBOX/inbox", mbs[0].Name, mbs[0].Role)
	}
	byPath := map[string]*jmapapi.Mailbox{}
	for _, mb := range mbs {
		byPath[mb.Path] = mb
	}
	child := byPath["Archive/2026"]
	if child == nil || child.ParentID == "" {
		t.Fatalf("Archive/2026 parent not resolved: %#v", child)
	}
	if child.ParentID != byPath["Archive"].ID {
		t.Errorf("Archive/2026 parent = %s, want Archive id", child.ParentID)
	}
	// The wire name is the leaf: nesting travels in parentId, so a client
	// matching "2026" in a tree finds it (RFC 8621 §2, FR-M.1) — the same
	// shape Fastmail answers with.
	if child.Name != "2026" {
		t.Errorf("nested mailbox name = %q, want the leaf %q (path stays %q)",
			child.Name, "2026", child.Path)
	}
	// A folder vanishing is a destroy, not a silent delete.
	kept := append([]Folder(nil), testFolders()[:3]...)
	if _, err := s.SyncFolders(ctx, "acct", kept); err != nil {
		t.Fatalf("resync: %v", err)
	}
	cs, err := s.Changes(ctx, "acct", "Mailbox", state)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(cs.Destroyed) != 1 || cs.Destroyed[0] != child.ID {
		t.Errorf("destroyed = %v, want [%s]", cs.Destroyed, child.ID)
	}
}

func TestPutMessagesDedupeThreadCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	recs := []MessageRec{
		mkRec(1, "<a@example>", "Quarterly report", "alice@example.test", []string{`\Seen`}, at),
		mkRec(2, "<b@example>", "Re: Quarterly report", "me@example.test", nil, at.Add(time.Hour)),
		mkRec(3, "<c@example>", "Unrelated", "bob@example.test", nil, at.Add(2*time.Hour)),
	}
	recs[1].References = []string{"<a@example>"}
	recs[1].InReplyTo = []string{"<a@example>"}
	if err := s.PutMessages(ctx, "acct", "INBOX", recs); err != nil {
		t.Fatalf("put: %v", err)
	}
	// The reply joins the first message's thread by References.
	mbs, _, err := s.Mailboxes(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	inbox := mbs[0]
	if inbox.TotalEmails != 3 || inbox.UnreadEmails != 2 {
		t.Errorf("inbox counts = %d/%d, want 3/2", inbox.TotalEmails, inbox.UnreadEmails)
	}
	if inbox.TotalThreads != 2 || inbox.UnreadThreads != 2 {
		t.Errorf("inbox threads = %d/%d, want 2/2", inbox.TotalThreads, inbox.UnreadThreads)
	}
	// Same message arriving in a second folder keeps its id.
	sent := testFolders()[1]
	_ = sent
	dup := mkRec(1, "<a@example>", "Quarterly report", "alice@example.test", []string{`\Seen`}, at)
	dup.UID = 1
	// Re-file through Sent under a different uid.
	dupIn := mkRec(9, "<a@example>", "Quarterly report", "alice@example.test", []string{`\Seen`}, at)
	if err := s.PutMessages(ctx, "acct", "Sent", []MessageRec{dupIn}); err != nil {
		t.Fatalf("dup put: %v", err)
	}
	emails, _, notFound, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notFound) != 0 {
		t.Fatalf("unexpected notFound: %v", notFound)
	}
	if len(emails) != 3 {
		t.Fatalf("dedupe failed: %d emails, want 3", len(emails))
	}
	bySubject := map[string]*jmapapi.Email{}
	for _, e := range emails {
		bySubject[e.Subject] = e
	}
	report := bySubject["Quarterly report"]
	if len(report.MailboxIDs) != 2 {
		t.Errorf("report mailboxIds = %v, want 2 memberships", report.MailboxIDs)
	}
	// Thread/get over the report thread, oldest first.
	threads, _, _, err := s.ThreadsByID(ctx, "acct", []string{report.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || len(threads[0].EmailIDs) != 2 {
		t.Fatalf("thread = %#v, want 2 members", threads)
	}
	if threads[0].EmailIDs[0] != report.ID {
		t.Errorf("thread not oldest-first: %v", threads[0].EmailIDs)
	}
	// Delta-maintained counts must equal a from-scratch recount.
	assertCountsMatchRecount(t, s, "acct")
}

// TestPutMessagesSameFolderDuplicateDeliveries pins the dedupe scope:
// the Message-ID reuse is cross-folder only — a second delivery of the
// same Message-ID into the same folder is its own message (two rows,
// counts 2), while the same message surfacing in a second folder still
// shares one id with two memberships (and the counts that implies).
func TestPutMessagesSameFolderDuplicateDeliveries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rec := func(uid uint32) MessageRec {
		return mkRec(uid, "<dup@example>", "Duplicated delivery", "alice@example.test", nil, at)
	}
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{rec(1)}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, total, unread := mailboxByName(t, s, "INBOX"); total != 1 || unread != 1 {
		t.Fatalf("inbox after first delivery = %d/%d, want 1/1", total, unread)
	}

	// Same Message-ID, different uid, same folder: a second delivery.
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{rec(2)}); err != nil {
		t.Fatalf("duplicate put: %v", err)
	}
	emails, _, notFound, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notFound) != 0 {
		t.Fatalf("unexpected notFound: %v", notFound)
	}
	if len(emails) != 2 {
		t.Errorf("same-folder redelivery merged: %d emails, want 2", len(emails))
	}
	if _, total, unread := mailboxByName(t, s, "INBOX"); total != 2 || unread != 2 {
		t.Errorf("inbox after duplicate = %d/%d, want 2/2", total, unread)
	}

	// The same message in a second folder still dedupes cross-folder.
	if err := s.PutMessages(ctx, "acct", "Archive", []MessageRec{rec(3)}); err != nil {
		t.Fatalf("cross-folder put: %v", err)
	}
	emails, _, _, err = s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 {
		t.Errorf("cross-folder dedupe failed: %d emails, want 2", len(emails))
	}
	shared := 0
	for _, e := range emails {
		if len(e.MailboxIDs) == 2 {
			shared++
		}
	}
	if shared != 1 {
		t.Errorf("shared emails = %d, want exactly one id with two memberships", shared)
	}
	if _, total, unread := mailboxByName(t, s, "Archive"); total != 1 || unread != 1 {
		t.Errorf("archive counts = %d/%d, want 1/1", total, unread)
	}
	assertCountsMatchRecount(t, s, "acct")
}

// TestAppendThenConflictingDeliveryStaysSeparate pins the flags-agree
// half of the dedupe rule: a Sent copy stored \Seen and the same
// Message-ID delivered unflagged into INBOX (self-sent mail) are two
// IMAP messages whose state one JMAP object cannot hold truthfully, so
// they stay separate emails and the delivered copy counts as unread.
func TestAppendThenConflictingDeliveryStaysSeparate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rec := mkRec(26, "<self@x>", "self-sent", "me@example.test", []string{`\Seen`}, at)
	if _, err := s.CommitAppend(ctx, "acct", "Sent", rec); err != nil {
		t.Fatal(err)
	}
	delivered := mkRec(53, "<self@x>", "self-sent", "me@example.test", []string{`\Recent`}, at)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{delivered}); err != nil {
		t.Fatal(err)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 {
		t.Errorf("conflicting copies merged: %d emails, want 2", len(emails))
	}
	if _, total, unread := mailboxByName(t, s, "INBOX"); total != 1 || unread != 1 {
		t.Errorf("inbox counts = %d/%d, want 1/1 (delivery is unread)", total, unread)
	}
	if _, total, _ := mailboxByName(t, s, "Sent"); total != 1 {
		t.Errorf("sent total = %d, want 1", total)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestFlagUpdateMovesUnreadAndState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{
		mkRec(1, "<a@example>", "One", "a@example.test", nil, at),
	}); err != nil {
		t.Fatal(err)
	}
	state, err := s.EmailStateString(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.UpdateFlags(ctx, "acct", "INBOX", 7, 1, []string{`\Seen`, `\Flagged`})
	if err != nil || !found {
		t.Fatalf("update flags: found=%v err=%v", found, err)
	}
	emails, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !emails[0].Keywords["$seen"] || !emails[0].Keywords["$flagged"] {
		t.Errorf("keywords = %#v, want $seen+$flagged", emails[0].Keywords)
	}
	mbs, _, _ := s.Mailboxes(ctx, "acct")
	if mbs[0].UnreadEmails != 0 || mbs[0].UnreadThreads != 0 {
		t.Errorf("unread after seen = %d/%d, want 0/0", mbs[0].UnreadEmails, mbs[0].UnreadThreads)
	}
	cs, err := s.Changes(ctx, "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Updated) != 1 {
		t.Errorf("updated = %v, want the flagged email", cs.Updated)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestExpungeTombstonesAndMembershipSurvival(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{
		mkRec(1, "<a@example>", "Both", "a@example.test", nil, at),
		mkRec(2, "<b@example>", "Only", "b@example.test", nil, at),
	}); err != nil {
		t.Fatal(err)
	}
	both, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var bothID, onlyID string
	for _, e := range both {
		if e.Subject == "Both" {
			bothID = e.ID
		} else {
			onlyID = e.ID
		}
	}
	// File "Both" into Archive as well, then expunge it from INBOX: it
	// must survive on its id.
	rec := mkRec(5, "<a@example>", "Both", "a@example.test", nil, at)
	if err := s.PutMessages(ctx, "acct", "Archive", []MessageRec{rec}); err != nil {
		t.Fatal(err)
	}
	state, _ := s.EmailStateString(ctx, "acct")
	if err := s.RemoveUIDs(ctx, "acct", "INBOX", 7, []uint32{1, 2}); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Changes(ctx, "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Destroyed) != 1 || cs.Destroyed[0] != onlyID {
		t.Errorf("destroyed = %v, want [%s]", cs.Destroyed, onlyID)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != bothID {
		t.Errorf("updated = %v, want [%s] (membership-only change)", cs.Updated, bothID)
	}
	emails, _, notFound, err := s.EmailsByID(ctx, "acct", []string{bothID, onlyID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 1 || emails[0].ID != bothID {
		t.Errorf("live emails = %v, want only %s", emails, bothID)
	}
	if !reflect.DeepEqual(notFound, []string{onlyID}) {
		t.Errorf("notFound = %v, want [%s]", notFound, onlyID)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestUIDValidityResetMintsFreshIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{
		mkRec(1, "<a@example>", "Kept", "a@example.test", []string{`\Seen`}, at),
	}); err != nil {
		t.Fatal(err)
	}
	before, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil || len(before) != 1 {
		t.Fatalf("setup: %v %d", err, len(before))
	}
	oldID := before[0].ID
	state, _ := s.EmailStateString(ctx, "acct")

	// Server resets UIDVALIDITY and the message reappears under a new
	// uid with identical content.
	if err := s.ResetFolder(ctx, "acct", "INBOX", 8); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Changes(ctx, "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(cs.Destroyed, oldID) {
		t.Fatalf("destroyed = %v, want old id %s", cs.Destroyed, oldID)
	}
	redelivered := mkRec(1, "<a@example>", "Kept", "a@example.test", []string{`\Seen`}, at)
	redelivered.UIDValidity = 8
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{redelivered}); err != nil {
		t.Fatal(err)
	}
	after, _, _, err := s.EmailsByID(ctx, "acct", nil, false)
	if err != nil || len(after) != 1 {
		t.Fatalf("re-backfill: %v %d", err, len(after))
	}
	if after[0].ID == oldID {
		t.Error("uidvalidity reset recycled the destroyed id (FR-S.6 forbids it)")
	}
	if after[0].Subject != "Kept" {
		t.Errorf("subject lost: %q", after[0].Subject)
	}
	assertCountsMatchRecount(t, s, "acct")
}

func TestQueryFiltersSortsAndCollapses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	recs := []MessageRec{
		mkRec(1, "<a@example>", "Apple pie", "alice@example.test", nil, at),
		mkRec(2, "<b@example>", "Re: Apple pie", "me@example.test", nil, at.Add(time.Hour)),
		mkRec(3, "<c@example>", "Banana bread", "bob@example.test", nil, at.Add(2*time.Hour)),
		mkRec(4, "<d@example>", "Cherry", "carol@example.test", []string{`\Seen`}, at.Add(3*time.Hour)),
	}
	recs[1].References = []string{"<a@example>"}
	if err := s.PutMessages(ctx, "acct", "INBOX", recs); err != nil {
		t.Fatal(err)
	}
	mbs, _, _ := s.Mailboxes(ctx, "acct")
	inbox := mbs[0].ID

	ids, pos, total, counter, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox},
		Limit:  2,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 4 || pos != 0 || len(ids) != 2 {
		t.Fatalf("page = %d ids @%d of %d, want 2@0 of 4", len(ids), pos, total)
	}
	if counter == "" || counter == "0" {
		t.Errorf("counter = %q, want advanced state", counter)
	}

	// text: AND of words across subject/people.
	_, _, total, _, err = s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{Text: "apple"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("text apple → %d, want 2 (thread)", total)
	}
	fromBob, _, totalBob, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{From: "bob"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if totalBob != 1 || len(fromBob) != 1 {
		t.Errorf("from bob → %d ids/%d total, want 1/1", len(fromBob), totalBob)
	}

	// collapseThreads keeps the newest exemplar of the thread.
	ids, _, total, _, err = s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter:          jmapapi.EmailFilter{InMailbox: inbox},
		CollapseThreads: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("collapsed total = %d, want 3", total)
	}
	// anchor addressing: window starting at the second id.
	if len(ids) >= 2 {
		_, pos, _, _, err = s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: inbox},
			Anchor: ids[1], AnchorOffset: 1, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if pos != 2 {
			t.Errorf("anchor position = %d, want 2", pos)
		}
	}
	// hasKeyword + unknown mailbox.
	seen, _, _, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{HasKeyword: "$seen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("$seen → %d, want 1", len(seen))
	}
	none, _, total, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: "mb-nonexistent"},
	})
	if err != nil || total != 0 || len(none) != 0 {
		t.Errorf("unknown mailbox: ids=%v total=%d err=%v, want empty", none, total, err)
	}
}

func TestHydrationStoresBodiesAndBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	rec := mkRec(1, "<a@example>", "Body", "a@example.test", nil, at)
	rec.HasAttachment = true
	rec.Structure = `{"partId":"1","type":"multipart/mixed","subParts":[` +
		`{"partId":"2","type":"text/plain","charset":"utf-8","size":5},` +
		`{"partId":"3","type":"application/pdf","size":9,"name":"r.pdf","disposition":"attachment"}]}`
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{rec}); err != nil {
		t.Fatal(err)
	}

	var gotPreview, gotBody []string
	s.Ensure = func(_ context.Context, _ string, previewIDs, bodyIDs []string) error {
		gotPreview, gotBody = previewIDs, bodyIDs
		// Body requests carry preview-filling with them; preview-only
		// requests hit SetPreviews.
		if len(previewIDs) > 0 {
			if err := s.SetPreviews(context.Background(), "acct", map[string]string{
				previewIDs[0]: "hello wor",
			}); err != nil {
				return err
			}
		}
		return s.PutHydrated(context.Background(), "acct", bodyIDs[0], BodyResult{
			Values:      map[string]string{"2": "hello world"},
			Attachments: []AttachmentData{{PartID: "3", MediaType: "application/pdf", Name: "r.pdf", Data: []byte("%PDF-1.4")}},
			Preview:     "hello wor",
		})
	}

	emails, _, _, err := s.EmailsByID(ctx, "acct", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody) != 1 {
		t.Fatalf("ensure saw preview=%v body=%v, want one body", gotPreview, gotBody)
	}
	e := emails[0]
	if e.Preview != "hello wor" {
		t.Errorf("preview = %q", e.Preview)
	}
	if e.BodyValues["2"] != "hello world" {
		t.Errorf("bodyValues = %#v", e.BodyValues)
	}
	if len(e.Attachments) != 1 || e.Attachments[0].BlobID == "" {
		t.Fatalf("attachments = %#v, want blobId stamped", e.Attachments)
	}
	if len(e.TextParts) != 1 || e.TextParts[0] != "2" {
		t.Errorf("textParts = %v, want [2]", e.TextParts)
	}
	// The blob file exists and reads back byte-exact.
	data, mediaType, err := s.ReadBlob(ctx, "acct", e.Attachments[0].BlobID)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(data) != "%PDF-1.4" || mediaType != "application/pdf" {
		t.Errorf("blob = %q (%s)", data, mediaType)
	}
	if len(e.Attachments[0].BlobID) != 13 {
		t.Errorf("blob id = %q, want 13-char id", e.Attachments[0].BlobID)
	}
	// A second read must NOT trigger Ensure again (hydrated).
	s.Ensure = func(context.Context, string, []string, []string) error {
		t.Fatal("Ensure called after hydration")
		return nil
	}
	if _, _, _, err := s.EmailsByID(ctx, "acct", nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestChangesReplayAndFloor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	s1, _ := s.EmailStateString(ctx, "acct")
	if _, err := s.Changes(ctx, "acct", "Email", s1); err != nil {
		t.Fatalf("stable state must replay empty: %v", err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := s.PutMessages(ctx, "acct", "INBOX", []MessageRec{
		mkRec(1, "<a@example>", "New", "a@example.test", nil, at),
	}); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Changes(ctx, "acct", "Email", s1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created) != 1 {
		t.Fatalf("created = %v, want 1", cs.Created)
	}
	// Unknown/future states degrade to cannotCalculateChanges.
	if _, err := s.Changes(ctx, "acct", "Email", "999999"); err == nil {
		t.Error("future sinceState must fail")
	}
	if _, err := s.Changes(ctx, "acct", "Email", "not-a-number"); err == nil {
		t.Error("garbage sinceState must fail")
	}
	if _, err := s.Changes(ctx, "acct", "Thread", s1); err == nil {
		t.Error("unsupported kind must fail")
	}
}

// assertCountsMatchRecount proves the incrementally maintained counters
// agree with a from-scratch recount (the oracle for PLAN §4's count
// risk).
func assertCountsMatchRecount(t *testing.T, s *Store, account string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT rowid FROM mailboxes WHERE account = ? AND deleted IS NULL`, account)
	if err != nil {
		t.Fatal(err)
	}
	var uids []int64
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		uids = append(uids, u)
	}
	_ = rows.Close()
	for _, uid := range uids {
		var want struct{ total, unread, threads, unreadThreads int }
		if err := tx.QueryRow(`SELECT total_emails, unread_emails, total_threads, unread_threads
			FROM mailboxes WHERE rowid = ?`, uid).Scan(&want.total, &want.unread, &want.threads, &want.unreadThreads); err != nil {
			t.Fatal(err)
		}
		if err := recountMailbox(context.Background(), tx, account, uid); err != nil {
			t.Fatal(err)
		}
		var got struct{ total, unread, threads, unreadThreads int }
		if err := tx.QueryRow(`SELECT total_emails, unread_emails, total_threads, unread_threads
			FROM mailboxes WHERE rowid = ?`, uid).Scan(&got.total, &got.unread, &got.threads, &got.unreadThreads); err != nil {
			t.Fatal(err)
		}
		if want != got {
			t.Errorf("mailbox %d counts: incremental=%+v recount=%+v", uid, want, got)
		}
	}
}

func contains(list []string, want string) bool {
	sort.Strings(list)
	i := sort.SearchStrings(list, want)
	return i < len(list) && list[i] == want
}

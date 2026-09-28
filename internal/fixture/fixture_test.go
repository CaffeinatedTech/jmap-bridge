package fixture

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New()
}

func mustQuery(t *testing.T, s *Store, q jmapapi.EmailQuery) (ids []string, total, position int) {
	t.Helper()
	ids, position, total, _, err := s.QueryEmails(context.Background(), "personal", q)
	if err != nil {
		t.Fatalf("QueryEmails: %v", err)
	}
	return ids, total, position
}

func TestMailboxCountsAndHierarchy(t *testing.T) {
	s := newStore(t)
	mbs, state, err := s.Mailboxes(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if state != "1" {
		t.Errorf("state = %q, want 1", state)
	}
	byID := map[string]*jmapapi.Mailbox{}
	for _, mb := range mbs {
		byID[mb.ID] = mb
	}
	inbox := byID["mb-inbox"]
	if inbox.TotalEmails != 5 || inbox.UnreadEmails != 2 {
		t.Errorf("inbox counts = %d/%d, want 5/2", inbox.TotalEmails, inbox.UnreadEmails)
	}
	if inbox.TotalThreads != 4 || inbox.UnreadThreads != 2 {
		t.Errorf("inbox threads = %d/%d, want 4/2", inbox.TotalThreads, inbox.UnreadThreads)
	}
	if inbox.Role != "inbox" || inbox.ParentID != "" {
		t.Errorf("inbox = role %q parent %q", inbox.Role, inbox.ParentID)
	}
	child := byID["mb-archive-2026"]
	if child.ParentID != "mb-archive" {
		t.Errorf("2026 parent = %q, want mb-archive", child.ParentID)
	}
	if child.TotalEmails != 1 {
		t.Errorf("child counts must be exact membership, got %d", child.TotalEmails)
	}
	// Rights are all granted in the fixture.
	if !inbox.MayRead || !inbox.MayAddItems || !inbox.MayDelete {
		t.Error("fixture rights should be granted")
	}
}

func TestMailboxesByIDUnknown(t *testing.T) {
	s := newStore(t)
	got, _, notFound, err := s.MailboxesByID(context.Background(), "personal", []string{"mb-inbox", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mb-inbox" {
		t.Errorf("got = %v", got)
	}
	if len(notFound) != 1 || notFound[0] != "ghost" {
		t.Errorf("notFound = %v", notFound)
	}
}

func TestQueryDefaultSortIsNewestFirst(t *testing.T) {
	s := newStore(t)
	ids, total, _ := mustQuery(t, s, jmapapi.EmailQuery{})
	if total != 10 {
		t.Fatalf("total = %d, want 10", total)
	}
	if ids[0] != "em-draft" {
		t.Errorf("first = %q, want newest (em-draft)", ids[0])
	}
	if ids[len(ids)-1] != "em-oldnote" {
		t.Errorf("last = %q, want oldest (em-oldnote)", ids[len(ids)-1])
	}
}

func TestQueryInMailboxAndSorts(t *testing.T) {
	s := newStore(t)
	ids, total, _ := mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{InMailbox: "mb-inbox"}})
	if total != 5 {
		t.Errorf("inbox total = %d, want 5", total)
	}
	if ids[0] != "em-meeting" {
		t.Errorf("first inbox email = %q, want em-meeting (newest)", ids[0])
	}

	asc, _, _ := mustQuery(t, s, jmapapi.EmailQuery{
		Sort: []jmapapi.EmailSort{{Property: "receivedAt", Ascending: true}},
	})
	if asc[0] != "em-oldnote" {
		t.Errorf("ascending first = %q, want em-oldnote", asc[0])
	}

	bySubject, _, _ := mustQuery(t, s, jmapapi.EmailQuery{
		Sort: []jmapapi.EmailSort{{Property: "subject", Ascending: true}},
	})
	if bySubject[0] != "em-draft" {
		t.Errorf("subject-ascending first = %q, want em-draft (\"Draft: …\" sorts first)", bySubject[0])
	}

	bySize, _, _ := mustQuery(t, s, jmapapi.EmailQuery{
		Sort: []jmapapi.EmailSort{{Property: "size", Ascending: true}},
	})
	if bySize[0] != "em-oldnote" {
		t.Errorf("size-ascending first = %q, want em-oldnote (256 bytes)", bySize[0])
	}
}

func TestQueryCollapseThreadsKeepsFirstInSortOrder(t *testing.T) {
	s := newStore(t)
	ids, total, _ := mustQuery(t, s, jmapapi.EmailQuery{
		Filter:          jmapapi.EmailFilter{InMailbox: "mb-inbox"},
		CollapseThreads: true,
	})
	if total != 4 {
		t.Fatalf("collapsed total = %d, want 4", total)
	}
	// Newest-first sort: the newer reply is the exemplar of thr-report.
	for _, id := range ids {
		if id == "em-report" {
			t.Error("collapsed list contains both members of thr-report")
		}
	}
	found := false
	for _, id := range ids {
		if id == "em-report-reply" {
			found = true
		}
	}
	if !found {
		t.Errorf("collapsed list missing the thread exemplar: %v", ids)
	}
}

func TestQueryPositionLimitAnchor(t *testing.T) {
	s := newStore(t)
	ids, _, pos := mustQuery(t, s, jmapapi.EmailQuery{Position: 1, Limit: 2})
	if pos != 1 || len(ids) != 2 {
		t.Fatalf("position=%d ids=%v", pos, ids)
	}
	all, _, _ := mustQuery(t, s, jmapapi.EmailQuery{})
	if ids[0] != all[1] || ids[1] != all[2] {
		t.Errorf("window %v does not match all[1:3] %v", ids, all[1:3])
	}

	anchored, _, pos := mustQuery(t, s, jmapapi.EmailQuery{Anchor: all[0], AnchorOffset: 2, Limit: 3})
	if pos != 2 {
		t.Errorf("anchor position = %d, want 2", pos)
	}
	if len(anchored) != 3 || anchored[0] != all[2] {
		t.Errorf("anchored window = %v", anchored)
	}

	// A vanished anchor clamps to the end rather than failing (the
	// contract jmap-tui's window repair expects).
	stale, _, pos := mustQuery(t, s, jmapapi.EmailQuery{Anchor: "ghost", Limit: 3})
	if pos != 10 || len(stale) != 0 {
		t.Errorf("stale anchor: pos=%d ids=%v, want end of list", pos, stale)
	}
}

func TestQuerySearchFilters(t *testing.T) {
	s := newStore(t)

	ids, total, _ := mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{Text: "quarterly"}})
	if total != 2 {
		t.Errorf("text %q total = %d, want 2 (%v)", "quarterly", total, ids)
	}

	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{From: "alice"}})
	if total != 1 {
		t.Errorf("from alice total = %d, want 1", total)
	}

	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{To: "alice"}})
	if total != 2 {
		t.Errorf("to alice total = %d, want 2", total)
	}

	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{Subject: "quarterly"}})
	if total != 2 {
		t.Errorf("subject quarterly total = %d, want 2", total)
	}

	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{HasKeyword: "$flagged"}})
	if total != 1 {
		t.Errorf("$flagged total = %d, want 1", total)
	}

	after := time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC) // exclusive
	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{After: &after}})
	if total != 4 {
		t.Errorf("after total = %d, want 4", total)
	}

	before := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, total, _ = mustQuery(t, s, jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{Before: &before}})
	if total != 1 {
		t.Errorf("before total = %d, want 1 (only em-oldnote predates August)", total)
	}
}

func TestThreadsOldestFirst(t *testing.T) {
	s := newStore(t)
	threads, _, notFound, err := s.ThreadsByID(context.Background(), "personal", []string{"thr-report", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || len(notFound) != 1 {
		t.Fatalf("threads=%v notFound=%v", threads, notFound)
	}
	ids := threads[0].EmailIDs
	if len(ids) != 2 || ids[0] != "em-report" || ids[1] != "em-report-reply" {
		t.Errorf("members = %v, want oldest first", ids)
	}
}

func TestChanges(t *testing.T) {
	s := newStore(t)
	cs, err := s.Changes(context.Background(), "personal", "Email", "1")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if cs.NewState != "1" || len(cs.Updated) != 0 {
		t.Errorf("cs = %+v", cs)
	}
	if _, err := s.Changes(context.Background(), "personal", "Email", "stale"); err != jmapapi.ErrCannotCalculateChanges {
		t.Errorf("stale state err = %v, want ErrCannotCalculateChanges", err)
	}
	if _, err := s.Changes(context.Background(), "personal", "Mailbox", ""); err != jmapapi.ErrCannotCalculateChanges {
		t.Errorf("empty state err = %v, want ErrCannotCalculateChanges", err)
	}
}

func TestSeedsPerAccountDeterministically(t *testing.T) {
	s := newStore(t)
	a, _, err := s.Mailboxes(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Mailboxes(context.Background(), "account-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("accounts seed differently: %d vs %d mailboxes", len(a), len(b))
	}
	// Repeat fetches are stable.
	again, _, _ := s.Mailboxes(context.Background(), "account-a")
	for i := range a {
		if a[i] != again[i] {
			t.Fatalf("mailbox %d changed between reads", i)
		}
	}
}

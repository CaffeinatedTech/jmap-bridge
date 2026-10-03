package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	_ "modernc.org/sqlite"
)

// searchFixture ingests three messages whose shapes cover the search
// filters: distinct senders, one flagged, one attachment-bearing.
func searchFixture(t *testing.T, s *Store) (inbox string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	recs := []MessageRec{
		mkRec(1, "<s1@example>", "Quarterly report attached", "ada@example.test", nil, at),
		mkRec(2, "<s2@example>", "Lunch tomorrow?", "grace@navy.mil", []string{`$Flagged`}, at.Add(time.Hour)),
		mkRec(3, "<s3@example>", "Re: Quarterly report", "ada@example.test", nil, at.Add(2*time.Hour)),
	}
	recs[0].HasAttachment = true
	if err := s.PutMessages(ctx, "acct", "INBOX", recs); err != nil {
		t.Fatal(err)
	}
	mbs, _, _ := s.Mailboxes(ctx, "acct")
	return mbs[0].ID
}

func query(t *testing.T, s *Store, f jmapapi.EmailFilter) (ids []string, total int, counter string) {
	t.Helper()
	ids, _, total, counter, err := s.QueryEmails(context.Background(), "acct", jmapapi.EmailQuery{Filter: f})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return ids, total, counter
}

func TestFTSTextMatchesHeadersImmediately(t *testing.T) {
	s := newTestStore(t)
	inbox := searchFixture(t, s)

	// Subject token, case-insensitive, mid-subject.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Text: "quarterly", InMailbox: inbox}); total != 2 {
		t.Errorf("subject token total = %d, want 2", total)
	}
	// Multi-word text ANDs across fields: "quarterly lunch" matches
	// nothing (different messages), "quarterly report" matches both.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Text: "quarterly lunch"}); total != 0 {
		t.Errorf("cross-message AND total = %d, want 0", total)
	}
	// Sender token via the text alias.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Text: "grace"}); total != 1 {
		t.Errorf("sender token total = %d, want 1", total)
	}
	// Body tokens are absent until hydration (FR-X.5).
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Text: "zebra"}); total != 0 {
		t.Errorf("unhydrated body token total = %d, want 0", total)
	}
	// Punctuation-only text matches nothing, not everything.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Text: "!!!"}); total != 0 {
		t.Errorf("punctuation-only total = %d, want 0", total)
	}
}

func TestFTSFromToPrefixAndDomain(t *testing.T) {
	s := newTestStore(t)
	searchFixture(t, s)

	// Prefix on the local part (FR-X.2).
	if _, total, _ := query(t, s, jmapapi.EmailFilter{From: "ada"}); total != 2 {
		t.Errorf("from prefix total = %d, want 2", total)
	}
	// Domain match.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{From: "navy.mil"}); total != 1 {
		t.Errorf("from domain total = %d, want 1", total)
	}
	// to covers to+cc (the recipient mirror's shape).
	if _, total, _ := query(t, s, jmapapi.EmailFilter{To: "me@example.test"}); total != 3 {
		t.Errorf("to total = %d, want 3", total)
	}
	// Subject filter is its own column, not a text alias.
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Subject: "lunch"}); total != 1 {
		t.Errorf("subject total = %d, want 1", total)
	}
	// A from filter must not match recipients ("me@example.test" is on
	// every message's to line).
	if _, total, _ := query(t, s, jmapapi.EmailFilter{From: "me@example.test"}); total != 0 {
		t.Errorf("from leaked into recipient column: total = %d, want 0", total)
	}
}

// TestFTSSubjectIsWholeToken pins the deliberate asymmetry (FR-X.2):
// from/to match by token prefix, subject does not. A partial subject
// must return nothing so substring-search clients (jmap-tui) fall back
// to their own scan instead of being shadowed by a partial server page.
func TestFTSSubjectIsWholeToken(t *testing.T) {
	s := newTestStore(t)
	searchFixture(t, s)

	if _, total, _ := query(t, s, jmapapi.EmailFilter{Subject: "lunch"}); total != 1 {
		t.Fatalf("subject whole-token total = %d, want 1", total)
	}
	if _, total, _ := query(t, s, jmapapi.EmailFilter{Subject: "lunc"}); total != 0 {
		t.Errorf("subject partial total = %d, want 0 (whole-token semantics)", total)
	}
}

func TestFTSBodyTokensArriveWithHydration(t *testing.T) {
	s := newTestStore(t)
	inbox := searchFixture(t, s)
	ctx := context.Background()

	before, _, _, counter, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{Text: "zebra"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("pre-hydration zebra hits = %v", before)
	}

	// Hydrate the first message: its body now carries the token.
	if err := s.PutHydrated(ctx, "acct", beforeOrFirst(t, s, inbox), BodyResult{
		Values: map[string]string{"1": "the zebra escaped again"},
	}); err != nil {
		t.Fatal(err)
	}

	after, _, total, afterCounter, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{Text: "zebra"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(after) != 1 {
		t.Fatalf("post-hydration zebra = %v (%d), want 1", after, total)
	}
	if afterCounter == counter {
		t.Error("queryState did not move when body tokens arrived (FR-X.7)")
	}
}

// beforeOrFirst returns any message id in the inbox (the helper name
// predates its use: hydration needs one id, any id).
func beforeOrFirst(t *testing.T, s *Store, inbox string) string {
	t.Helper()
	ids, _, _, _, err := s.QueryEmails(context.Background(), "acct",
		jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{InMailbox: inbox}})
	if err != nil || len(ids) == 0 {
		t.Fatalf("inbox ids: %v %v", ids, err)
	}
	return ids[0]
}

func TestFTSTombstoneLeavesIndex(t *testing.T) {
	s := newTestStore(t)
	searchFixture(t, s)

	if _, total, _ := query(t, s, jmapapi.EmailFilter{From: "grace@navy.mil"}); total != 1 {
		t.Fatalf("pre-tombstone total = %d, want 1", total)
	}
	if err := s.ResetFolder(context.Background(), "acct", "INBOX", 9); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := query(t, s, jmapapi.EmailFilter{From: "grace@navy.mil"}); total != 0 {
		t.Errorf("post-tombstone total = %d, want 0 (FR-X.1 index/tombstone consistency)", total)
	}
}

func TestQueryMultiKeySort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	// Two messages share a sender and a receivedAt to exercise the
	// deterministic id tiebreak under equal keys.
	recs := []MessageRec{
		mkRec(1, "<m1@example>", "Zulu", "ada@example.test", nil, at),
		mkRec(2, "<m2@example>", "Alpha", "ada@example.test", nil, at),
		mkRec(3, "<m3@example>", "Yankee", "grace@navy.mil", nil, at.Add(time.Hour)),
	}
	if err := s.PutMessages(ctx, "acct", "INBOX", recs); err != nil {
		t.Fatal(err)
	}
	subs := func(ids []string) map[string]bool {
		out := map[string]bool{}
		emails, _, _, err := s.EmailsByID(ctx, "acct", ids, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range emails {
			out[e.Subject] = true
		}
		return out
	}

	// from ASC then subject ASC: the two ada messages lead, Alpha
	// before Zulu; the id tiebreak is deterministic but unobservable
	// here — equal keys keep a stable pair either way.
	ids, _, total, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Sort: []jmapapi.EmailSort{
			{Property: "from", Ascending: true},
			{Property: "subject", Ascending: true},
		},
	})
	if err != nil || total != 3 {
		t.Fatalf("multi-sort: %v total=%d", err, total)
	}
	first := subs(ids[:2])
	if !first["Alpha"] || !first["Zulu"] {
		t.Errorf("from-then-subject sort misplaced ties: %v", ids)
	}
	if subs(ids[2:])["Yankee"] != true {
		t.Errorf("last row should be the later sender: %v", ids)
	}
}

func TestSearchBackfillHookScansScope(t *testing.T) {
	s := newTestStore(t)
	inbox := searchFixture(t, s)
	ctx := context.Background()

	var hooked [][]string
	s.SearchBackfill = func(account string, ids []string) {
		if account != "acct" {
			t.Errorf("backfill account = %q", account)
		}
		hooked = append(hooked, ids)
	}
	s.BackfillScan = 2 // bound visible

	if _, _, _, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox, Text: "quarterly"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(hooked) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(hooked))
	}
	if len(hooked[0]) != 2 {
		t.Errorf("candidates = %d, want the BackfillScan bound of 2", len(hooked[0]))
	}

	// No text filter, no backfill: header filters always match headers.
	hooked = nil
	if _, _, _, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox, Subject: "quarterly"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(hooked) != 0 {
		t.Errorf("header-only query triggered backfill: %v", hooked)
	}

	// Nil hook (search.backfill = false): identical results, no error.
	s.SearchBackfill = nil
	ids, _, total, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox, Text: "quarterly"},
	})
	if err != nil || total != 2 || len(ids) != 2 {
		t.Errorf("backfill-off query = %v/%d/%v", ids, total, err)
	}
}

func TestFTSIndexRebuiltAfterUpgrade(t *testing.T) {
	// A database whose emails predate the v5 index must gain FTS rows
	// on open without any new ingest (the one-time open backfill).
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMessages(ctx, "acct", "INBOX",
		[]MessageRec{mkRec(1, "<r1@example>", "Prehistoric mammoth", "old@example.test", nil, time.Now())}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-v5 database: drop the index rows and clear the
	// built marker, then reopen.
	if err := dropFTSRows(dir); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	_, _, total, _, err := s2.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{Text: "mammoth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("upgraded index total = %d, want 1", total)
	}
}

// dropFTSRows empties the search index and clears its built marker,
// leaving the emails in place — the shape of a database that synced
// before the v5 migration.
func dropFTSRows(dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "bridge.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DELETE FROM email_search;
		DELETE FROM sync_state WHERE account = '' AND scope = 'fts:built'`); err != nil {
		return err
	}
	return nil
}

func TestCoveredPageMatchesStreaming(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SyncFolders(ctx, "acct", testFolders()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var recs []MessageRec
	for i := 0; i < 40; i++ {
		r := mkRec(uint32(i+1), fmt.Sprintf("<p%d@example>", i),
			fmt.Sprintf("Msg %02d", i), "ada@example.test", nil, at.Add(time.Duration(i)*time.Minute))
		if i%10 == 0 {
			r.HasAttachment = true
		}
		recs = append(recs, r)
	}
	if err := s.PutMessages(ctx, "acct", "INBOX", recs); err != nil {
		t.Fatal(err)
	}
	mbs, _, _ := s.Mailboxes(ctx, "acct")
	inbox := mbs[0].ID

	stream := func(f jmapapi.EmailFilter) []string {
		ids, _, _, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{Filter: f})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	page := func(q jmapapi.EmailQuery) ([]string, int, int) {
		ids, pos, total, _, err := s.QueryEmails(ctx, "acct", q)
		if err != nil {
			t.Fatal(err)
		}
		return ids, pos, total
	}
	full := stream(jmapapi.EmailFilter{InMailbox: inbox})

	// Every offset page must equal the streamed window at the same
	// position, totals included.
	for _, off := range []int{0, 7, 39, 40, 41} {
		ids, pos, total := page(jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: inbox}, Position: off, Limit: 5,
		})
		want := 5
		if off >= len(full) {
			want = 0
		} else if len(full)-off < 5 {
			want = len(full) - off
		}
		if total != len(full) {
			t.Errorf("offset %d: total = %d, want %d", off, total, len(full))
		}
		wantPos := off
		if wantPos > len(full) {
			wantPos = len(full)
		}
		if pos != wantPos {
			t.Errorf("offset %d: position = %d, want %d", off, pos, wantPos)
		}
		if len(ids) != want {
			t.Fatalf("offset %d: window = %d ids, want %d", off, len(ids), want)
		}
		for i, id := range ids {
			if full[wantPos+i] != id {
				t.Errorf("offset %d: window[%d] = %s, want %s", off, i, id, full[wantPos+i])
			}
		}
	}

	// Anchor addressing: window at anchor index + offset, true position.
	for _, idx := range []int{0, 5, 39} {
		ids, pos, total := page(jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: inbox},
			Anchor: full[idx], AnchorOffset: 1, Limit: 5,
		})
		if pos != idx+1 {
			t.Errorf("anchor@%d: position = %d, want %d", idx, pos, idx+1)
		}
		if total != len(full) {
			t.Errorf("anchor@%d: total = %d, want %d", idx, total, len(full))
		}
		want := min(5, total-pos)
		if len(ids) != want {
			t.Errorf("anchor@%d: %d ids, want %d", idx, len(ids), want)
		}
		for i, id := range ids {
			if full[pos+i] != id {
				t.Errorf("anchor@%d: window[%d] = %s != streamed %s", idx, i, id, full[pos+i])
			}
		}
	}
	// An unknown anchor is an error, not a clamp to the end (RFC 8620
	// §5.5): Email/query reports anchorNotFound.
	_, _, _, _, err := s.QueryEmails(ctx, "acct", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox}, Anchor: "nope", Limit: 5,
	})
	if !errors.Is(err, jmapapi.ErrAnchorNotFound) {
		t.Errorf("unknown anchor error = %v, want ErrAnchorNotFound", err)
	}
	// Attachment filter narrows the covered total.
	_, _, total := page(jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: inbox, HasAttachment: boolPtr(true)}, Limit: 5,
	})
	if total != 4 {
		t.Errorf("attachment-filtered total = %d, want 4", total)
	}
}

func boolPtr(b bool) *bool { return &b }

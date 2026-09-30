package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

func bookRec(href, name string, order int) BookRec {
	return BookRec{Href: href, Name: name, SortOrder: order, MayWrite: true, MayDelete: true}
}

func cardRec(uid, href, etag, js string) StoreCard {
	return StoreCard{
		UID: uid, Kind: "individual", Name: uid,
		JSContact: js, VCard: "BEGIN:VCARD\r\nUID:" + uid + "\r\nEND:VCARD\r\n",
		Href: href, ETag: etag,
	}
}

func TestSyncBooksLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SyncBooks(ctx, "personal", []BookRec{
		bookRec("/test/addresses.vcf/", "Addresses", 100),
	}); err != nil {
		t.Fatal(err)
	}
	books, state, nf, err := s.AddressBooksByID(ctx, "personal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || len(nf) != 0 {
		t.Fatalf("books = %+v nf=%v", books, nf)
	}
	if books[0].Name != "Addresses" || !books[0].MayWrite {
		t.Errorf("book = %+v", books[0])
	}
	firstState := state

	// Discovery re-running with identical properties: the JMAP state
	// must NOT advance — and crucially, a discovery pass never seeds
	// the sync cursor or ctag (SaveBookSync owns those). The state has
	// to hold still (M6's stale-cursor trap).
	if err := s.SyncBooks(ctx, "personal", []BookRec{
		{
			Href: "/test/addresses.vcf/", Name: "Addresses", SortOrder: 100,
			MayWrite: true, MayDelete: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	ba, err := s.BookAddressByHref(ctx, "personal", "/test/addresses.vcf/")
	if err != nil || ba == nil || ba.SyncToken != "" || ba.CTag != "" {
		t.Fatalf("discovery leaked cursors into the row: %+v err=%v", ba, err)
	}
	_, state, _, err = s.AddressBooksByID(ctx, "personal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state != firstState {
		t.Fatalf("state advanced on bookkeeping-only pass: %s -> %s", firstState, state)
	}

	// Rename: state moves, /changes reports an update.
	if err := s.SyncBooks(ctx, "personal", []BookRec{
		{
			Href: "/test/addresses.vcf/", Name: "Renamed", SortOrder: 100,
			MayWrite: true, MayDelete: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	books, state2, _, err := s.AddressBooksByID(ctx, "personal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if books[0].Name != "Renamed" || state2 == firstState {
		t.Fatalf("rename not reflected: %+v state %s->%s", books[0], firstState, state2)
	}
	cs, err := s.Changes(ctx, "personal", "AddressBook", firstState)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.Updated, []string{books[0].ID}) {
		t.Fatalf("changes updated = %+v", cs)
	}

	// Second book; drop the first → book tombstone AND card cascade.
	if err := s.SyncBooks(ctx, "personal", []BookRec{
		bookRec("/test/addresses.vcf/", "Renamed", 100),
		bookRec("/test/work.vcf/", "Work", 200),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PutCards(ctx, "personal", books[0].ID,
		[]StoreCard{cardRec("uid-1", "/test/addresses.vcf/1.vcf", "e1", `{"uid":"uid-1"}`)})
	if err != nil || n != 1 {
		t.Fatalf("put cards: %v touched=%d", err, n)
	}
	if err := s.SyncBooks(ctx, "personal", []BookRec{
		bookRec("/test/work.vcf/", "Work", 200),
	}); err != nil {
		t.Fatal(err)
	}
	booksAfter, _, _, _ := s.AddressBooksByID(ctx, "personal", nil)
	if len(booksAfter) != 1 || booksAfter[0].Name != "Work" {
		t.Fatalf("books after drop = %+v", booksAfter)
	}
	cards, _, _, err := s.CardsByID(ctx, "personal", []string{"uid-1"})
	if err != nil || len(cards) != 0 {
		t.Fatalf("cascade failed: %+v %v", cards, err)
	}
	cs, err = s.Changes(ctx, "personal", "ContactCard", state2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.Destroyed, []string{"uid-1"}) {
		t.Fatalf("cascade not in changes: %+v", cs)
	}
}

func TestPutCardsIdempotentAndChanges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SyncBooks(ctx, "personal", []BookRec{bookRec("/test/addresses.vcf/", "Addresses", 0)}); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := s.AddressBooksByID(ctx, "personal", nil)
	bid := books[0].ID

	n, err := s.PutCards(ctx, "personal", bid, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "etag-1", `{"uid":"uid-a","kind":"individual"}`),
	})
	if err != nil || n != 1 {
		t.Fatalf("first ingest: touched=%d err=%v", n, err)
	}
	_, state1, _, _ := s.CardsByID(ctx, "personal", nil)

	// Same bytes again: zero touched, zero state movement.
	n, err = s.PutCards(ctx, "personal", bid, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "etag-1", `{"uid":"uid-a","kind":"individual"}`),
	})
	if err != nil || n != 0 {
		t.Fatalf("idempotent re-ingest: touched=%d err=%v", n, err)
	}
	_, state2, _, _ := s.CardsByID(ctx, "personal", nil)
	if state1 != state2 {
		t.Fatalf("state moved on no-op: %s -> %s", state1, state2)
	}

	// ETag change → updated; CS reports it after state1.
	n, err = s.PutCards(ctx, "personal", bid, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "etag-2", `{"uid":"uid-a","kind":"individual","name":{"full":"A B"}}`),
	})
	if err != nil || n != 1 {
		t.Fatalf("etag update: touched=%d err=%v", n, err)
	}
	cs, err := s.Changes(ctx, "personal", "ContactCard", state1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.Updated, []string{"uid-a"}) {
		t.Fatalf("changes = %+v", cs)
	}

	// Resurrection: same uid returns after a tombstone.
	if _, err := s.TombstoneCardsByHrefs(ctx, "personal", []string{"/test/addresses.vcf/a.vcf"}); err != nil {
		t.Fatal(err)
	}
	_, state3, _, _ := s.CardsByID(ctx, "personal", nil)
	n, err = s.PutCards(ctx, "personal", bid, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "etag-3", `{"uid":"uid-a","kind":"individual"}`),
	})
	if err != nil || n != 1 {
		t.Fatalf("resurrect: touched=%d err=%v", n, err)
	}
	cs, err = s.Changes(ctx, "personal", "ContactCard", state3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.Created, []string{"uid-a"}) {
		t.Fatalf("resurrect changes = %+v", cs)
	}
}

func TestPruneAndStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SyncBooks(ctx, "personal", []BookRec{bookRec("/test/addresses.vcf/", "Addresses", 0)}); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := s.AddressBooksByID(ctx, "personal", nil)
	bid := books[0].ID
	if _, err := s.PutCards(ctx, "personal", bid, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "e1", `{"uid":"uid-a"}`),
		cardRec("uid-b", "/test/addresses.vcf/b.vcf", "e1", `{"uid":"uid-b"}`),
	}); err != nil {
		t.Fatal(err)
	}
	states, err := s.States(ctx, "personal")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"AddressBook", "ContactCard"} {
		if states[k] == "" || states[k] == "0" {
			t.Errorf("state %s = %q, want non-zero", k, states[k])
		}
	}
	// Prune keeps a.vcf, removes b.vcf (href not in the fresh listing).
	if _, err := s.PruneCardsInBook(ctx, "personal", bid, []string{"/test/addresses.vcf/a.vcf"}); err != nil {
		t.Fatal(err)
	}
	cards, _, notFound, err := s.CardsByID(ctx, "personal", []string{"uid-b"})
	if err != nil || len(cards) != 0 || !reflect.DeepEqual(notFound, []string{"uid-b"}) {
		t.Fatalf("prune left data: %+v %v", cards, notFound)
	}
}

func TestBookCardLookups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SyncBooks(ctx, "personal", []BookRec{bookRec("/test/addresses.vcf/", "Addresses", 0)}); err != nil {
		t.Fatal(err)
	}
	ba, err := s.BookAddressByHref(ctx, "personal", "/test/addresses.vcf/")
	if err != nil || ba == nil {
		t.Fatalf("by href: %v %+v", err, ba)
	}
	ba2, err := s.BookAddress(ctx, "personal", ba.ID)
	if err != nil || ba2 == nil || ba2.Href != ba.Href {
		t.Fatalf("by id: %v", err)
	}
	if _, err := s.PutCards(ctx, "personal", ba.ID, []StoreCard{
		cardRec("uid-a", "/test/addresses.vcf/a.vcf", "etag-9", `{"uid":"uid-a"}`),
	}); err != nil {
		t.Fatal(err)
	}
	loc, err := s.CardLocation(ctx, "personal", "uid-a")
	if err != nil || loc == nil || loc.ETag != "etag-9" || loc.Href != "/test/addresses.vcf/a.vcf" {
		t.Fatalf("location: %v %+v", err, loc)
	}
	card, exists, err := s.CardByUID(ctx, "personal", "uid-a")
	if err != nil || !exists || card.ID != "uid-a" {
		t.Fatalf("by uid: %v %v", err, exists)
	}
	if err := s.CommitCardDestroy(ctx, "personal", "uid-a"); err != nil {
		t.Fatal(err)
	}
	loc, err = s.CardLocation(ctx, "personal", "uid-a")
	if err != nil || loc != nil {
		t.Fatalf("destroyed card still addressable: %+v", loc)
	}
	refs, err := s.CardRefs(ctx, "personal", ba.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("refs after destroy: %+v %v", refs, err)
	}
}

func TestCardsByIDUnknownAndAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cards, state, nf, err := s.CardsByID(ctx, "personal", []string{"nope"})
	if err != nil || state != "0" || !reflect.DeepEqual(nf, []string{"nope"}) || len(cards) != 0 {
		t.Fatalf("empty account: %+v %q %v %v", cards, state, nf, err)
	}
	var _ jmapapi.ChangeSet
}

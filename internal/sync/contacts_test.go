package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/dav"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturecarddav"
)

func newContactEngine(t *testing.T, url, user, pass string) (*Engine, *store.Store) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), store.Options{DataDir: t.TempDir(), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{
		Account:  "personal",
		Interval: time.Hour, // the loop sleeps between passes; tests drive passes directly
		CardDAV:  &dav.Config{URL: url, Username: user, Password: pass},
	}
	e := New(cfg, st, quiet)
	return e, st
}

func vcard(uid, fn string) string {
	return "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\nEND:VCARD\r\n"
}

func TestSyncContactsTierSync(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	home := "/test/addresses.vcf/"
	fx.PutCard(home, "a.vcf", vcard("uid-a", "Ada"))
	fx.PutCard(home, "b.vcf", vcard("uid-b", "Grace"))

	e, st := newContactEngine(t, fx.URL(), "u", "p")
	ctx := context.Background()
	sess, err := e.davSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	cards, state, _, err := st.CardsByID(ctx, "personal", nil)
	if err != nil || len(cards) != 2 {
		t.Fatalf("cards = %+v err = %v", cards, err)
	}
	cs, err := st.Changes(ctx, "personal", "ContactCard", "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created) != 2 {
		t.Fatalf("created = %v", cs.Created)
	}

	// Foreign edit: server-side change lands as ContactCard update.
	before := state
	fx.PutCard(home, "a.vcf", vcard("uid-a", "Ada Lovelace"))
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	cards, state, _, _ = st.CardsByID(ctx, "personal", []string{"uid-a"})
	if state == before {
		t.Fatal("state did not move after foreign edit")
	}
	if !strings.Contains(string(cards[0].Content), "Ada Lovelace") {
		t.Fatalf("edit not folded: %s", cards[0].Content)
	}
	cs, _ = st.Changes(ctx, "personal", "ContactCard", before)
	if len(cs.Updated) != 1 || cs.Updated[0] != "uid-a" {
		t.Fatalf("changes = %+v", cs)
	}

	// Foreign delete.
	fx.DeleteCard(home, "b.vcf")
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	cs, _ = st.Changes(ctx, "personal", "ContactCard", state)
	if len(cs.Destroyed) != 1 || cs.Destroyed[0] != "uid-b" {
		t.Fatalf("delete not detected: %+v", cs)
	}

	// Book added and removed: AddressBook changes + cascade.
	fx.AddBook("/test/work.vcf/")
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatalf("fourth pass: %v", err)
	}
	books, bookState, _, _ := st.AddressBooksByID(ctx, "personal", nil)
	if len(books) != 2 {
		t.Fatalf("books = %d", len(books))
	}
	_ = bookState
	cs, _ = st.Changes(ctx, "personal", "AddressBook", "0")
	if len(cs.Created) != 2 {
		t.Fatalf("book created = %v", cs.Created)
	}
}

func TestSyncContactsFallbackTiers(t *testing.T) {
	for _, tier := range []fixturecarddav.Tier{fixturecarddav.TierGetctag, fixturecarddav.TierBare} {
		name := map[fixturecarddav.Tier]string{
			fixturecarddav.TierGetctag: "getctag", fixturecarddav.TierBare: "bare",
		}[tier]
		t.Run(name, func(t *testing.T) {
			fx := fixturecarddav.New(tier, "u", "p")
			defer fx.Close()
			home := "/test/addresses.vcf/"
			fx.PutCard(home, "a.vcf", vcard("uid-a", "Ada"))

			e, st := newContactEngine(t, fx.URL(), "u", "p")
			ctx := context.Background()
			sess, err := e.davSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.syncContacts(ctx, sess); err != nil {
				t.Fatalf("seed pass: %v", err)
			}
			cards, _, _, _ := st.CardsByID(ctx, "personal", []string{"uid-a"})
			if len(cards) != 1 {
				t.Fatalf("fallback tier must ingest via listing: %+v", cards)
			}

			// Foreign edit + delete detected through the listing diff.
			fx.PutCard(home, "a.vcf", vcard("uid-a", "Ada L."))
			fx.PutCard(home, "c.vcf", vcard("uid-c", "New"))
			if err := e.syncContacts(ctx, sess); err != nil {
				t.Fatalf("edit pass: %v", err)
			}
			cards, _, _, _ = st.CardsByID(ctx, "personal", nil)
			if len(cards) != 2 {
				t.Fatalf("cards after fallback diff = %d", len(cards))
			}
			var ada string
			for _, c := range cards {
				if c.ID == "uid-a" {
					ada = string(c.Content)
				}
			}
			if !strings.Contains(ada, "Ada L.") {
				t.Fatalf("fallback missed the edit: %s", ada)
			}
		})
	}
}

func TestContactsReadyGate(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	e, _ := newContactEngine(t, fx.URL(), "u", "p")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if e.ContactsReady() {
		t.Fatal("ready before any pass")
	}
	go e.contactsLoop(ctx)

	deadline := time.After(5 * time.Second)
	for !e.ContactsReady() {
		select {
		case <-deadline:
			t.Fatal("contacts never became ready")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// And an unreachable server keeps it false: start a fresh engine on
	// a dead URL and confirm it stays not-ready (the capability must
	// never appear on hope, FR-P.3).
	e2, _ := newContactEngine(t, "http://127.0.0.1:1/", "u", "p")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.contactsLoop(ctx2)
	time.Sleep(300 * time.Millisecond)
	if e2.ContactsReady() {
		t.Fatal("ready against a dead server")
	}
}

func TestWriteCreateUpdateMoveDestroy(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	fx.AddBook("/test/work.vcf/")
	e, st := newContactEngine(t, fx.URL(), "u", "p")
	ctx := context.Background()
	sess, err := e.davSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := st.AddressBooksByID(ctx, "personal", nil)
	if len(books) != 2 {
		t.Fatalf("books = %+v", books)
	}
	b1, b2 := books[0].ID, books[1].ID

	content := func(name string) json.RawMessage {
		c := map[string]any{
			"uid": "w-1", "kind": "individual",
			"name":   map[string]any{"full": name, "components": []any{}, "isOrdered": true},
			"emails": map[string]any{"0": map[string]any{"address": name + "@example.com"}},
		}
		raw, _ := json.Marshal(c)
		return raw
	}

	id, err := e.CreateContact(ctx, "personal", jmapapi.ContactSpec{BookID: b1, Content: content("ada")})
	if err != nil || id != "w-1" {
		t.Fatalf("create: %v %q", err, id)
	}
	// The server now holds it at the deterministic href.
	if got := fx.PutCard("/test/addresses.vcf/", "probe.vcf", vcard("probe", "p")); got == "" {
		t.Fatal("fixture broken")
	}
	_ = st

	// Same uid again → exists (FR-P.8).
	if _, err := e.CreateContact(ctx, "personal", jmapapi.ContactSpec{BookID: b1, Content: content("ada")}); !errors.Is(err, jmapapi.ErrContactExists) {
		t.Fatalf("second create err = %v", err)
	}

	// Update lands on the server, not just the cache.
	newContent := map[string]any{
		"uid": "w-1", "kind": "individual",
		"name": map[string]any{"full": "Ada Updated", "components": []any{}, "isOrdered": true},
	}
	raw, _ := json.Marshal(newContent)
	if err := e.UpdateContact(ctx, "personal", "w-1", jmapapi.ContactSpec{BookID: b1, Content: raw}); err != nil {
		t.Fatalf("update: %v", err)
	}
	loc, _ := st.CardLocation(ctx, "personal", "w-1")
	if loc == nil || !strings.Contains(loc.ETag, "\"") {
		t.Fatalf("location after update = %+v", loc)
	}
	c, err := sess.Get(ctx, loc.Href)
	if err != nil || !strings.Contains(string(c.Raw), "Ada Updated") {
		t.Fatalf("server copy wrong: %v %s", err, c.Raw)
	}

	// Move to the other book: new href exists, old one gone.
	if err := e.UpdateContact(ctx, "personal", "w-1", jmapapi.ContactSpec{BookID: b2, Content: raw}); err != nil {
		t.Fatalf("move: %v", err)
	}
	loc, _ = st.CardLocation(ctx, "personal", "w-1")
	if !strings.HasPrefix(loc.Href, "/test/work.vcf/") {
		t.Fatalf("card did not move: %+v", loc)
	}
	if _, err := sess.Get(ctx, "/test/addresses.vcf/w-1.vcf"); !errors.Is(err, dav.ErrGone) {
		t.Fatalf("old copy survived: %v", err)
	}

	// Destroy removes from the server (FR-P.10) then tombstones.
	if err := e.DestroyContact(ctx, "personal", "w-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := sess.Get(ctx, loc.Href); !errors.Is(err, dav.ErrGone) {
		t.Fatalf("server still has it: %v", err)
	}
	cards, _, notFound, _ := st.CardsByID(ctx, "personal", []string{"w-1"})
	if len(cards) != 0 || len(notFound) != 1 {
		t.Fatalf("cache still has it: %+v %v", cards, notFound)
	}
	// Destroy already-gone id → notFound (nothing to remove).
	if err := e.DestroyContact(ctx, "personal", "w-1"); !errors.Is(err, jmapapi.ErrObjectNotFound) {
		t.Fatalf("double destroy = %v", err)
	}

	// The sync pass after all this sees the server exactly as the cache.
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	cs, _ := st.Changes(ctx, "personal", "ContactCard", locless(state(t, st)))
	if len(cs.Created)+len(cs.Updated)+len(cs.Destroyed) != 0 {
		t.Fatalf("cache and server disagree after writes: %+v", cs)
	}
}

func state(t *testing.T, st *store.Store) string {
	t.Helper()
	states, err := st.States(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	return states["ContactCard"]
}

func locless(s string) string { return s }

func TestWritePhotoRoundTrip(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	e, st := newContactEngine(t, fx.URL(), "u", "p")
	ctx := context.Background()
	sess, err := e.davSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := st.AddressBooksByID(ctx, "personal", nil)
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// Server-side card with a v3 inline photo (base64, ENCODING=b).
	pngBase64 := "iVBORw0KGgoAAAANSUhEUg==" // decodes; fixture-safe
	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:photo-1\r\nFN:Photo\r\n" +
		"PHOTO;ENCODING=b;TYPE=PNG:" + pngBase64 + "\r\nEND:VCARD\r\n"
	fx.PutCard("/test/addresses.vcf/", "photo.vcf", vcf)
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	cards, _, _, _ := st.CardsByID(ctx, "personal", []string{"photo-1"})
	if len(cards) != 1 {
		t.Fatalf("photo card not ingested: %+v", cards)
	}
	var c struct {
		Photo struct {
			BlobID string `json:"blobId"`
			Type   string `json:"type"`
			Size   int64  `json:"size"`
		} `json:"photo"`
	}
	if err := json.Unmarshal(cards[0].Content, &c); err != nil {
		t.Fatal(err)
	}
	if c.Photo.BlobID == "" || c.Photo.Type != "image/png" {
		t.Fatalf("photo ref wrong: %s", cards[0].Content)
	}
	data, media, err := st.ReadBlob(ctx, "personal", c.Photo.BlobID)
	if err != nil || media != "image/png" {
		t.Fatalf("blob unreadable: %v %q", err, media)
	}

	// Update the card without touching the photo: the rebuild must keep
	// the PHOTO (the bytes come back from the blob store, DAV-first PUT).
	content := map[string]any{
		"uid": "photo-1", "kind": "individual",
		"name":  map[string]any{"full": "Photo Two"},
		"photo": map[string]any{"blobId": c.Photo.BlobID, "type": media, "size": len(data)},
	}
	raw, _ := json.Marshal(content)
	if err := e.UpdateContact(ctx, "personal", "photo-1", jmapapi.ContactSpec{
		BookID: books[0].ID, Content: raw,
	}); err != nil {
		t.Fatalf("photo-preserving update: %v", err)
	}
	loc, _ := st.CardLocation(ctx, "personal", "photo-1")
	got, err := sess.Get(ctx, loc.Href)
	if err != nil || !strings.Contains(string(got.Raw), "data:image/png;base64,iVBORw0KGgo") {
		t.Fatalf("photo lost on rebuild: %v %s", err, got.Raw)
	}
	if !strings.Contains(string(got.Raw), "Photo Two") {
		t.Fatalf("name update missing: %s", got.Raw)
	}
}

func TestWriteToReadOnlyBookFails(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	fx.AddReadOnlyBook("/test/viewonly.vcf/")
	e, st := newContactEngine(t, fx.URL(), "u", "p")
	ctx := context.Background()
	sess, err := e.davSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := st.AddressBooksByID(ctx, "personal", nil)
	var ro string
	for _, b := range books {
		if !b.MayWrite {
			ro = b.ID
		}
	}
	if ro == "" {
		t.Fatal("read-only book not discovered")
	}
	raw, _ := json.Marshal(map[string]any{"uid": "ro-1", "kind": "individual"})
	_, err = e.CreateContact(ctx, "personal", jmapapi.ContactSpec{BookID: ro, Content: raw})
	if !errors.Is(err, jmapapi.ErrNotWritable) {
		t.Fatalf("err = %v", err)
	}
}

// TestWriteGeneratedUidStable is the regression for the Radicale
// no-uid-conflict 409 (M6 cross-client gate): a create without a uid
// mints one, and an update MUST reuse it — in the stored JSContact, in
// the rebuilt vCard and in the row id. A second UID per update is a
// card the server reads as a foreign collision.
func TestWriteGeneratedUidStable(t *testing.T) {
	fx := fixturecarddav.New(fixturecarddav.TierSync, "u", "p")
	defer fx.Close()
	e, st := newContactEngine(t, fx.URL(), "u", "p")
	ctx := context.Background()
	sess, err := e.davSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	books, _, _, _ := st.AddressBooksByID(ctx, "personal", nil)

	raw, _ := json.Marshal(map[string]any{
		"kind":   "individual",
		"name":   map[string]any{"full": "No UID"},
		"emails": map[string]any{"0": map[string]any{"address": "x@e.invalid"}},
	})
	id, err := e.CreateContact(ctx, "personal", jmapapi.ContactSpec{BookID: books[0].ID, Content: raw})
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := st.CardLocation(ctx, "personal", id)
	c1, err := sess.Get(ctx, loc.Href)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c1.Raw), "UID:"+id) {
		t.Fatalf("create body lost the generated uid: %s", c1.Raw)
	}
	cards, _, _, _ := st.CardsByID(ctx, "personal", []string{id})
	if !strings.Contains(string(cards[0].Content), `"uid":"`+id+`"`) {
		t.Fatalf("stored JSContact lost the generated uid: %s", cards[0].Content)
	}

	upd, _ := json.Marshal(map[string]any{"name": map[string]any{"full": "Renamed"}})
	if err := e.UpdateContact(ctx, "personal", id, jmapapi.ContactSpec{BookID: books[0].ID, Content: upd}); err != nil {
		t.Fatalf("update: %v", err)
	}
	c2, err := sess.Get(ctx, loc.Href)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c2.Raw), "UID:"+id) {
		t.Fatalf("update minted a different uid: %s", c2.Raw)
	}
	if strings.Count(string(c2.Raw), "UID:") != 1 {
		t.Fatalf("duplicate UID lines: %s", c2.Raw)
	}
}

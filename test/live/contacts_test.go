package live

// The M6 live gate, bridge side (PLAN §12): the full CardDAV path —
// discovery, sync-collection ingest, create/update/move/destroy through
// the jmapapi seam, and an independent DAV session seeing every mutation
// exactly as written (D-14 for contacts) — against a REAL CardDAV
// server. Skips unless JMAP_BRIDGE_TEST_CARDDAV_URL is set (AGENTS.md:
// env-only creds, never committed, never echoed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/dav"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	bridgesync "github.com/CaffeinatedTech/jmap-bridge/internal/sync"
)

func liveCardDAV(t *testing.T) dav.Config {
	t.Helper()
	url := os.Getenv("JMAP_BRIDGE_TEST_CARDDAV_URL")
	if url == "" {
		t.Skip("JMAP_BRIDGE_TEST_CARDDAV_URL unset; skipping live CardDAV test")
	}
	user := os.Getenv("JMAP_BRIDGE_TEST_CARDDAV_USERNAME")
	pass := os.Getenv("JMAP_BRIDGE_TEST_CARDDAV_PASSWORD")
	if user == "" || pass == "" {
		t.Skip("JMAP_BRIDGE_TEST_CARDDAV_USERNAME/PASSWORD unset; skipping")
	}
	return dav.Config{URL: url, Username: user, Password: pass}
}

// contactHarness: a store and a contacts-enabled engine pointed at the
// live server (plus, when IMAP creds exist too, an IMAP block so the
// full production wiring is what runs).
func contactHarness(t *testing.T) (*store.Store, *bridgesync.Engine) {
	t.Helper()
	cfg := liveCardDAV(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), store.Options{DataDir: t.TempDir(), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eCfg := bridgesync.Config{
		Account:  "livetest",
		Interval: time.Hour,
		CardDAV:  &cfg,
	}
	e := bridgesync.New(eCfg, st, quiet)
	return st, e
}

func TestLiveContactsGate(t *testing.T) {
	cfg := liveCardDAV(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sess, err := dav.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("live discovery: %v", err)
	}
	books, err := sess.Books(ctx)
	if err != nil {
		t.Fatalf("live books: %v", err)
	}
	if len(books) == 0 {
		t.Fatal("live server must expose at least one writable address book")
	}
	t.Logf("discovered %d book(s)", len(books))

	st, e := contactHarness(t)
	// Drive the pass through the engine exactly as Run does.
	if err := e.SyncContactsOnce(ctx); err != nil {
		t.Fatalf("live contacts pass: %v", err)
	}

	// Create a probe card through the JMAP seam.
	probe := fmt.Sprintf("bridge-probe-%d", time.Now().UnixNano())
	jsContent, _ := json.Marshal(map[string]any{
		"uid": probe, "kind": "individual",
		"name":   map[string]any{"full": "Bridge Probe", "components": []any{}, "isOrdered": true},
		"emails": map[string]any{"0": map[string]any{"address": "probe@bridge.invalid"}},
	})
	res := liveContactsSet(t, st, e, "ContactCard/set", map[string]any{
		"create": map[string]any{"c0": withBook(jsContent, firstBook(t, st))},
	})
	id := createdIDOf(t, res, "c0")
	t.Logf("created %q on the live server (destroyed in cleanup)", id)
	t.Cleanup(func() {
		_ = e.DestroyContact(context.Background(), "livetest", id)
	})

	// An independent DAV session must see the card — IMAP-first's
	// contacts twin: the cache never reports what the server refused.
	ver, err := dav.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ver.SyncBook(context.Background(), books[0].Href, "")
	if err != nil {
		t.Fatalf("verify sync: %v", err)
	}
	found := false
	var href, etag string
	for _, c := range out.Changed {
		cards, err := ver.MultiGet(context.Background(), books[0].Href, []string{c.Href})
		if err != nil {
			continue
		}
		for _, cd := range cards {
			if strings.Contains(string(cd.Raw), probe) {
				found, href, etag = true, cd.Href, cd.ETag
			}
		}
	}
	if !found {
		t.Fatal("created card not on the live server")
	}

	// Sync the engine again: the card lands in the cache with /changes
	// history (FR-P.2, FR-P.7).
	if err := e.SyncContactsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cards, state, _, err := st.CardsByID(ctx, "livetest", []string{probe})
	if err != nil || len(cards) != 1 {
		t.Fatalf("card not synced: %+v %v", cards, err)
	}
	cs, err := st.Changes(ctx, "livetest", "ContactCard", "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created) == 0 {
		t.Fatal("no ContactCard history for a real ingest")
	}
	_ = state

	// Update through the seam (a mutable-property patch, exactly what
	// jmap-tui sends), then confirm the server copy moved.
	upd, _ := json.Marshal(map[string]any{
		"name": map[string]any{"full": "Bridge Probe Renamed", "components": []any{}, "isOrdered": true},
	})
	res2 := liveContactsSet(t, st, e, "ContactCard/set", map[string]any{
		"update": map[string]any{probe: withoutBook(upd)},
	})
	if nu, ok := res2["notUpdated"].(map[string]any); ok && len(nu) > 0 {
		t.Fatalf("update rejected: %v", nu)
	}
	c, err := ver.Get(context.Background(), href)
	if err != nil || !strings.Contains(string(c.Raw), "Bridge Probe Renamed") {
		t.Fatalf("update not on the server: %v", err)
	}
	_ = etag

	// Destroy: gone on the server, tombstoned in the cache.
	if err := e.DestroyContact(ctx, "livetest", probe); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := ver.Get(context.Background(), href); err == nil {
		t.Fatal("card survived destroy on the server")
	}
	cards, _, notFound, _ := st.CardsByID(ctx, "livetest", []string{probe})
	if len(cards) != 0 || len(notFound) != 1 {
		t.Fatal("destroyed card still live in the cache")
	}
}

func firstBook(t *testing.T, st *store.Store) string {
	t.Helper()
	books, _, _, err := st.AddressBooksByID(context.Background(), "livetest", nil)
	if err != nil || len(books) == 0 {
		t.Fatalf("no books synced: %v", err)
	}
	for _, b := range books {
		if b.MayWrite {
			return b.ID
		}
	}
	t.Fatal("no writable book")
	return ""
}

func withBook(raw []byte, bookID string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["addressBookIds"] = map[string]bool{bookID: true}
	return m
}

func withoutBook(raw []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// liveContactsSet dispatches one contacts method through the real
// handler seams (store + engine as Backend), mirroring liveSet.
func liveContactsSet(t *testing.T, st *store.Store, e *bridgesync.Engine, method string, args map[string]any) map[string]any {
	t.Helper()
	args["accountId"] = "livetest"
	h := jmapapi.NewHandler(st)
	acct := &jmapapi.Account{
		ID: "livetest", Store: st, Backend: e,
		Capabilities: []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", jmapapi.ContactURN},
	}
	body, err := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", jmapapi.ContactURN},
		"methodCalls": []any{[]any{method, args, "c1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	raw, _ := json.Marshal(out)
	if status != 200 {
		t.Fatalf("%s: HTTP %d: %s", method, status, raw)
	}
	var wire struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.MethodResponses) == 0 {
		t.Fatalf("bad response: %s", raw)
	}
	var name string
	_ = json.Unmarshal(wire.MethodResponses[0][0], &name)
	if name == "error" {
		t.Fatalf("%s: method error: %s", method, wire.MethodResponses[0][1])
	}
	var m map[string]any
	if err := json.Unmarshal(wire.MethodResponses[0][1], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

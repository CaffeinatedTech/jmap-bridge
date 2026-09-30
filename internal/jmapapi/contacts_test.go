package jmapapi_test

// Contacts method-surface tests (FR-P.4–.13): dispatch shapes, patch
// semantics, error mapping and capability honesty, against the fake
// Store/Backend seams.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

var contactCaps = []string{
	"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail",
	jmapapi.ContactURN,
}

func callBatch(t *testing.T, using []string, calls ...[]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"using": using, "methodCalls": calls})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type callResult struct {
	Name string
	Args map[string]any
}

func dispatchContacts(t *testing.T, st *fakeStore, be jmapapi.Backend, using []string, calls ...[]any) (int, []callResult, any) {
	t.Helper()
	h := jmapapi.NewHandler(st)
	acct := &jmapapi.Account{ID: "acct1", Store: st, Backend: be, Capabilities: using}
	status, out := h.Dispatch(context.Background(), acct, callBatch(t, using, calls...))
	if status == 200 {
		var wire struct {
			MethodResponses [][3]json.RawMessage `json:"methodResponses"`
		}
		raw, _ := json.Marshal(out)
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("bad response: %s", raw)
		}
		var results []callResult
		for _, mr := range wire.MethodResponses {
			var name string
			var args map[string]any
			_ = json.Unmarshal(mr[0], &name)
			_ = json.Unmarshal(mr[1], &args)
			results = append(results, callResult{Name: name, Args: args})
		}
		return status, results, out
	}
	return status, nil, out
}

func TestAddressBookGet(t *testing.T) {
	st := newFakeStore()
	st.addBook(&jmapapi.AddressBook{
		ID: "b1", Name: "Addresses", SortOrder: 100,
		MayRead: true, MayWrite: true, MayShare: true, MayDelete: true,
	})
	st.addBook(&jmapapi.AddressBook{ID: "b2", Name: "Work", SortOrder: 200, MayRead: true})
	_, results, _ := dispatchContacts(t, st, nil, contactCaps,
		[]any{"AddressBook/get", map[string]any{"accountId": "acct1"}, "c1"})
	if len(results) != 1 || results[0].Name != "AddressBook/get" {
		t.Fatalf("results = %+v", results)
	}
	list, _ := results[0].Args["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("list = %v", results[0].Args)
	}
	// b2 may-write false must survive to the wire (myRights and the
	// top-level spellings FR-P.4 names).
	var b2 map[string]any
	for _, item := range list {
		m := item.(map[string]any)
		if m["id"] == "b2" {
			b2 = m
		}
	}
	if b2 == nil {
		t.Fatal("b2 missing")
	}
	if rights, _ := b2["myRights"].(map[string]any); rights["mayWrite"] != false {
		t.Errorf("b2 myRights = %v", b2["myRights"])
	}
	if b2["mayWrite"] != false || b2["mayRead"] != true {
		t.Errorf("b2 top-level flags = %v", b2)
	}
	// b1 (lowest sortOrder) is the default book.
	var b1 map[string]any
	for _, item := range list {
		m := item.(map[string]any)
		if m["id"] == "b1" {
			b1 = m
		}
	}
	if b1["isDefault"] != true {
		t.Errorf("b1 not default: %v", b1)
	}

	_, results, _ = dispatchContacts(t, st, nil, contactCaps,
		[]any{"AddressBook/get", map[string]any{"accountId": "acct1", "ids": []string{"b2", "nope"}}, "c1"})
	list, _ = results[0].Args["list"].([]any)
	nf, _ := results[0].Args["notFound"].([]any)
	if len(list) != 1 || len(nf) != 1 || nf[0] != "nope" {
		t.Fatalf("by-ids = %+v", results[0].Args)
	}
}

func TestContactCardGetAndProperties(t *testing.T) {
	st := newFakeStore()
	st.addCard(&jmapapi.ContactCard{
		ID: "uid-1", AddressBookIDs: []string{"b1"},
		Content: json.RawMessage(`{"kind":"individual","uid":"uid-1","name":{"full":"Ada","components":[{"kind":"given","value":"Ada"}],"isOrdered":true},"emails":{"0":{"address":"ada@example.com"}}}`),
	})
	_, results, _ := dispatchContacts(t, st, nil, contactCaps,
		[]any{"ContactCard/get", map[string]any{
			"accountId":  "acct1",
			"properties": []string{"name", "emails"},
		}, "c1"})
	list, _ := results[0].Args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", results[0].Args)
	}
	card := list[0].(map[string]any)
	if card["id"] != "uid-1" {
		t.Error("id must always travel")
	}
	if _, ok := card["name"]; !ok {
		t.Error("name missing")
	}
	if _, ok := card["addressBookIds"]; ok {
		t.Errorf("addressBookIds must be filtered out: %v", card)
	}
	if _, ok := card["emails"]; !ok {
		t.Error("emails missing")
	}

	// Unknown ids → notFound, not half-answered (FR-P.6).
	_, results, _ = dispatchContacts(t, st, nil, contactCaps,
		[]any{"ContactCard/get", map[string]any{"accountId": "acct1", "ids": []string{"gone"}}, "c1"})
	nf, _ := results[0].Args["notFound"].([]any)
	if len(nf) != 1 || nf[0] != "gone" {
		t.Fatalf("notFound = %v", results[0].Args)
	}
}

func TestContactCardSetCreate(t *testing.T) {
	st := newFakeStore()
	st.addBook(&jmapapi.AddressBook{ID: "b1", Name: "Addresses", MayRead: true, MayWrite: true})
	st.addBlob("p1", []byte("\x89PNG fake"), "image/png")
	be := newFakeBackend()

	content := map[string]any{
		"@type": "Card", "version": "1.0", "kind": "individual",
		"addressBookIds": map[string]bool{"b1": true},
		"name":           map[string]any{"full": "Grace", "components": []any{}, "isOrdered": true},
		"emails":         map[string]any{"0": map[string]any{"address": "grace@example.com"}},
	}
	_, results, _ := dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create":    map[string]any{"c0": content},
		}, "c1"})
	resp := results[0].Args
	created, _ := resp["created"].(map[string]any)
	entry, _ := created["c0"].(map[string]any)
	if entry["id"] != "gen-1" {
		t.Fatalf("created = %v (backend saw %+v)", resp, be.created)
	}
	if len(be.created) != 1 || !strings.Contains(string(be.created[0].Content), "grace@example.com") {
		t.Fatalf("spec not threaded: %+v", be.created)
	}
	// The spec's content must NOT carry id/addressBookIds — those are
	// row facts the store owns.
	var obj map[string]any
	_ = json.Unmarshal(be.created[0].Content, &obj)
	if _, ok := obj["addressBookIds"]; ok {
		t.Error("addressBookIds leaked into the content spec")
	}
	if _, ok := obj["id"]; ok {
		t.Error("id leaked into the content spec")
	}

	// Unknown book → invalidProperties, no backend call.
	before := len(be.created)
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"nope": true}, "kind": "individual",
			}},
		}, "c1"})
	nc := results[0].Args["notCreated"].(map[string]any)
	se := nc["c0"].(map[string]any)
	if se["type"] != "invalidProperties" || len(be.created) != before {
		t.Fatalf("unknown book not rejected: %v", nc)
	}

	// Multiple books → invalidArguments (FR-P.13).
	multi := map[string]any{
		"addressBookIds": map[string]bool{"b1": true}, "kind": "individual",
	}
	st.addBook(&jmapapi.AddressBook{ID: "b2", Name: "Work", MayWrite: true})
	multi["addressBookIds"] = map[string]bool{"b1": true, "b2": true}
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create":    map[string]any{"c0": multi},
		}, "c1"})
	nc = results[0].Args["notCreated"].(map[string]any)
	if se := nc["c0"].(map[string]any); se["type"] != "invalidArguments" {
		t.Fatalf("multi-book err = %v", nc)
	}

	// uid collision → exists (FR-P.8).
	be.existsUIDs["dup-1"] = true
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"b1": true}, "uid": "dup-1",
			}},
		}, "c1"})
	nc = results[0].Args["notCreated"].(map[string]any)
	if se := nc["c0"].(map[string]any); se["type"] != "exists" {
		t.Fatalf("collision err = %v", nc)
	}

	// Photo with an image blob → bytes threaded to the backend
	// (FR-P.11).
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"b1": true},
				"photo":          map[string]any{"blobId": "p1"},
			}},
		}, "c1"})
	if _, ok := results[0].Args["created"]; !ok {
		t.Fatalf("photo create failed: %v", results[0].Args)
	}
	last := be.created[len(be.created)-1]
	if string(last.Photo) != "\x89PNG fake" || last.PhotoMedia != "image/png" || last.PhotoBlobID != "p1" {
		t.Fatalf("photo not resolved: %+v", last)
	}

	// Photo with a non-image blob → invalidProperties (FR-P.11).
	st.addBlob("t1", []byte("GIF89a"), "image/gif")
	st.blobs["x1"] = blobRec{data: []byte("hi"), media: "text/plain"}
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"b1": true},
				"photo":          map[string]any{"blobId": "x1"},
			}},
		}, "c1"})
	nc = results[0].Args["notCreated"].(map[string]any)
	if se := nc["c0"].(map[string]any); se["type"] != "invalidProperties" {
		t.Fatalf("non-image photo err = %v", nc)
	}
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"b1": true},
				"photo":          map[string]any{"blobId": "gone"},
			}},
		}, "c1"})
	nc = results[0].Args["notCreated"].(map[string]any)
	if se := nc["c0"].(map[string]any); se["type"] != "blobNotFound" {
		t.Fatalf("missing blob err = %v", nc)
	}
}

// blobRec mirrors the fakeStore's blob record for direct seeding.
type blobRec = struct {
	data  []byte
	media string
}

func TestContactCardSetUpdate(t *testing.T) {
	st := newFakeStore()
	st.addBook(&jmapapi.AddressBook{ID: "b1", Name: "Addresses", MayWrite: true})
	st.addBook(&jmapapi.AddressBook{ID: "b2", Name: "Work", MayWrite: true})
	st.addCard(&jmapapi.ContactCard{
		ID: "u1", AddressBookIDs: []string{"b1"},
		Content: json.RawMessage(`{"kind":"individual","uid":"u1","name":{"full":"Ada"},"emails":{"0":{"address":"a@b.com"},"1":{"address":"c@d.com"}},"notes":{"0":{"note":"old"}}}`),
	})
	be := newFakeBackend()

	// Path patch: replace emails/0, siblings untouched (jmap-tui's form
	// edit shape).
	_, results, _ := dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"update": map[string]any{"u1": map[string]any{
				"emails/0": map[string]any{"address": "new@x.com"},
			}},
		}, "c1"})
	if _, ok := results[0].Args["updated"]; !ok {
		t.Fatalf("update failed: %v", results[0].Args)
	}
	spec := be.updated["u1"]
	var patched map[string]any
	if err := json.Unmarshal(spec.Content, &patched); err != nil {
		t.Fatal(err)
	}
	emails := patched["emails"].(map[string]any)
	if emails["0"].(map[string]any)["address"] != "new@x.com" {
		t.Errorf("patch lost: %v", emails)
	}
	if emails["1"].(map[string]any)["address"] != "c@d.com" {
		t.Errorf("sibling email clobbered: %v", emails)
	}
	// uid and name survive untouched.
	if patched["uid"] != "u1" || patched["name"] == nil {
		t.Errorf("untouched props dropped: %v", patched)
	}

	// null removes the whole property; null at a path removes the entry.
	_, _, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"update": map[string]any{"u1": map[string]any{
				"notes/0": nil,
			}},
		}, "c1"})
	spec = be.updated["u1"]
	_ = json.Unmarshal(spec.Content, &patched)
	if _, ok := patched["notes"]; ok {
		if notes, _ := patched["notes"].(map[string]any); len(notes) != 0 {
			t.Errorf("notes/0 null did not remove: %v", patched["notes"])
		}
	}

	// Membership move: whole replacement onto b2 (engine moves the card).
	_, _, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"update": map[string]any{"u1": map[string]any{
				"addressBookIds": map[string]bool{"b2": true},
			}},
		}, "c1"})
	if be.updated["u1"].BookID != "b2" {
		t.Errorf("move not threaded: %+v", be.updated["u1"])
	}

	// Immutable fields are refused (FR-P.9).
	for _, key := range []string{"uid", "@type", "version", "id"} {
		call := []any{
			"ContactCard/set",
			map[string]any{
				"accountId": "acct1",
				"update":    map[string]any{"u1": map[string]any{key: "x"}},
			},
			"c1",
		}
		_, results, _ = dispatchContacts(t, st, be, contactCaps, call)
		nu := results[0].Args["notUpdated"].(map[string]any)
		if se := nu["u1"].(map[string]any); se["type"] != "invalidProperties" {
			t.Errorf("immutable %q accepted: %v", key, nu)
		}
	}

	// Empty patch → updated, no backend call.
	before := be.updated
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"update":    map[string]any{"u1": map[string]any{"name": map[string]any{"full": "Ada"}}},
		}, "c1"})
	if _, ok := results[0].Args["updated"]; !ok {
		t.Fatalf("no-op patch: %v", results[0].Args)
	}
	_ = before

	// Unknown id → notFound.
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"update":    map[string]any{"gone": map[string]any{"name": nil}},
		}, "c1"})
	nu := results[0].Args["notUpdated"].(map[string]any)
	if se := nu["gone"].(map[string]any); se["type"] != "notFound" {
		t.Fatalf("unknown update = %v", nu)
	}
}

func TestContactCardSetDestroy(t *testing.T) {
	st := newFakeStore()
	be := newFakeBackend()
	be.knownIDs = map[string]bool{"u1": true}
	_, results, _ := dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"destroy":   []string{"gone"},
		}, "c1"})
	nd := results[0].Args["notDestroyed"].(map[string]any)
	if se := nd["gone"].(map[string]any); se["type"] != "notFound" {
		t.Fatalf("destroy unknown = %v", nd)
	}
	_, results, _ = dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"destroy":   []string{"u1"},
		}, "c1"})
	if d, _ := results[0].Args["destroyed"].([]any); len(d) != 1 || d[0] != "u1" {
		t.Fatalf("destroy = %v", results[0].Args)
	}
	if len(be.destroyed) != 1 || be.destroyed[0] != "u1" {
		t.Fatalf("backend destroyed = %v", be.destroyed)
	}
}

func TestContactSetErrorsMapping(t *testing.T) {
	st := newFakeStore()
	st.addBook(&jmapapi.AddressBook{ID: "b1", MayWrite: true})
	be := newFakeBackend()
	be.err = jmapapi.ErrOverwritten
	_, results, _ := dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"c0": map[string]any{
				"addressBookIds": map[string]bool{"b1": true},
			}},
		}, "c1"})
	nc := results[0].Args["notCreated"].(map[string]any)
	if se := nc["c0"].(map[string]any); se["type"] != "overwritten" {
		t.Fatalf("overwrite mapping = %v", nc)
	}
}

func TestContactsCapabilityHonesty(t *testing.T) {
	st := newFakeStore()
	// An account without the contacts URN must refuse the method at the
	// request level (FR-P.3: half-working data never happens).
	h := jmapapi.NewHandler(st)
	acct := &jmapapi.Account{ID: "acct1", Store: st, Capabilities: mailCaps}
	status, out := h.Dispatch(context.Background(), acct,
		callBatch(t, contactCaps, []any{"AddressBook/get", map[string]any{"accountId": "acct1"}, "c1"}))
	if status != 400 {
		t.Fatalf("status = %d, want 400 problem", status)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "unknownCapability") {
		t.Fatalf("problem = %s", raw)
	}

	// With the URN offered, /changes works for both contacts types.
	_, results, _ := dispatchContacts(t, st, nil, contactCaps,
		[]any{"ContactCard/changes", map[string]any{"accountId": "acct1", "sinceState": "1"}, "c1"},
		[]any{"AddressBook/changes", map[string]any{"accountId": "acct1", "sinceState": "1"}, "c2"})
	if results[0].Name != "error" || results[0].Args["type"] != "cannotCalculateChanges" {
		t.Logf("empty changes shape: %+v", results[0]) // fake has no recorded changes; cannotCalculate is honest
	}
	if results[1].Name != "error" {
		t.Errorf("AddressBook/changes should not half-answer: %+v", results[1])
	}
}

func TestContactCardSetStateMismatch(t *testing.T) {
	st := newFakeStore()
	be := newFakeBackend()
	_, results, _ := dispatchContacts(t, st, be, contactCaps,
		[]any{"ContactCard/set", map[string]any{
			"accountId": "acct1",
			"ifInState": "999",
			"create":    map[string]any{"c0": map[string]any{"addressBookIds": map[string]bool{"b1": true}}},
		}, "c1"})
	if results[0].Name != "error" || results[0].Args["type"] != "stateMismatch" {
		t.Fatalf("ifInState ignored: %+v", results[0])
	}
}

package jmapapi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// methodResponse is one decoded [name, args, callId] triple.
type methodResponse struct {
	Name   string
	Args   map[string]any
	CallID string
}

// dispatch runs a batch against the handler and returns each response.
func dispatch(t *testing.T, acct *jmapapi.Account, calls ...any) []methodResponse {
	t.Helper()
	h := jmapapi.NewHandler(acct.Store)
	body, err := json.Marshal(map[string]any{
		"using":       acct.Capabilities,
		"methodCalls": calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	if status != 200 {
		t.Fatalf("HTTP %d: %v", status, out)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	resp := make([]methodResponse, 0, len(wire.MethodResponses))
	for _, entry := range wire.MethodResponses {
		var r methodResponse
		if err := json.Unmarshal(entry[0], &r.Name); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(entry[1], &r.Args); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(entry[2], &r.CallID); err != nil {
			t.Fatal(err)
		}
		resp = append(resp, r)
	}
	return resp
}

func testAccount(store jmapapi.Store) *jmapapi.Account {
	return &jmapapi.Account{ID: "acct1", Store: store, Capabilities: mailCaps}
}

// TestCoreEcho pins RFC 8620 §3.1: Core/echo returns its arguments
// unchanged, and an empty argument object echoes as {}.
func TestCoreEcho(t *testing.T) {
	acct := testAccount(fixture.New())
	resp := dispatch(t, acct,
		[]any{"Core/echo", map[string]any{"hello": "world", "number": float64(42)}, "c1"},
		[]any{"Core/echo", map[string]any{}, "c2"},
		[]any{"Core/echo", map[string]any{"nested": map[string]any{"deep": "value"}}, "c3"},
	)
	if len(resp) != 3 {
		t.Fatalf("responses = %d, want 3", len(resp))
	}
	for _, r := range resp {
		if r.Name != "Core/echo" {
			t.Fatalf("method = %q, want Core/echo", r.Name)
		}
	}
	if resp[0].Args["hello"] != "world" || resp[0].Args["number"] != float64(42) {
		t.Errorf("echo basic = %v", resp[0].Args)
	}
	if len(resp[1].Args) != 0 {
		t.Errorf("echo empty = %v, want {}", resp[1].Args)
	}
	nested, _ := resp[2].Args["nested"].(map[string]any)
	if nested["deep"] != "value" {
		t.Errorf("echo nested = %v", resp[2].Args)
	}
}

// TestAccountIDErrors pins RFC 8620 §3.6.2: an absent accountId is
// invalidArguments, an unknown one is accountNotFound.
func TestAccountIDErrors(t *testing.T) {
	acct := testAccount(fixture.New())
	resp := dispatch(t, acct, []any{"Mailbox/get", map[string]any{}, "c1"})
	if resp[0].Name != "error" || resp[0].Args["type"] != "invalidArguments" {
		t.Fatalf("missing accountId = %v, want invalidArguments", resp[0])
	}
	resp = dispatch(t, acct, []any{"Mailbox/get", map[string]any{"accountId": "ghost"}, "c2"})
	if resp[0].Name != "error" || resp[0].Args["type"] != "accountNotFound" {
		t.Fatalf("unknown accountId = %v, want accountNotFound", resp[0])
	}
}

// TestGetNotFoundIsAlwaysArray pins RFC 8620 §5.1: the /get response
// object always carries notFound, as an array even when empty.
func TestGetNotFoundIsAlwaysArray(t *testing.T) {
	acct := testAccount(fixture.New())
	for _, method := range []string{"Mailbox/get", "Email/get", "Thread/get"} {
		resp := dispatch(t, acct, []any{method, map[string]any{"accountId": "acct1", "ids": []string{}}, "c"})
		if resp[0].Name != method {
			t.Fatalf("%s: response %v", method, resp[0])
		}
		nf, ok := resp[0].Args["notFound"]
		if !ok {
			t.Errorf("%s: notFound missing", method)
			continue
		}
		list, ok := nf.([]any)
		if !ok {
			t.Errorf("%s: notFound = %T, want array", method, nf)
			continue
		}
		if len(list) != 0 {
			t.Errorf("%s: notFound = %v, want []", method, list)
		}
	}
}

// TestMailboxRightsComplete pins the RFC 8621 §2 MailboxRights booleans.
func TestMailboxRightsComplete(t *testing.T) {
	acct := testAccount(fixture.New())
	resp := dispatch(t, acct, []any{"Mailbox/get", map[string]any{"accountId": "acct1", "ids": nil}, "c"})
	list := resp[0].Args["list"].([]any)
	if len(list) == 0 {
		t.Fatal("no mailboxes")
	}
	mb := list[0].(map[string]any)
	if _, ok := mb["isSubscribed"].(bool); !ok {
		t.Errorf("isSubscribed = %v, want bool", mb["isSubscribed"])
	}
	rights, ok := mb["myRights"].(map[string]any)
	if !ok {
		t.Fatalf("myRights = %T, want object", mb["myRights"])
	}
	for _, prop := range []string{
		"mayReadItems", "mayAddItems", "mayRemoveItems", "maySetSeen",
		"maySetKeywords", "mayCreateChild", "mayRename", "mayDelete", "maySubmit",
	} {
		if _, ok := rights[prop].(bool); !ok {
			t.Errorf("myRights.%s = %v, want bool", prop, rights[prop])
		}
	}
}

// TestBodyValuesBooleansAndTruncation pins RFC 8621 §4.1.4:
// bodyValues always carry isEncodingProblem and isTruncated, and
// maxBodyValueBytes truncates.
func TestBodyValuesBooleansAndTruncation(t *testing.T) {
	acct := testAccount(fixture.New())
	resp := dispatch(t, acct, []any{"Email/get", map[string]any{
		"accountId":           "acct1",
		"ids":                 []string{"em-welcome"},
		"properties":          []string{"textBody", "bodyValues"},
		"fetchTextBodyValues": true,
		"maxBodyValueBytes":   float64(12),
	}, "c"})
	if resp[0].Name != "Email/get" {
		t.Fatalf("response = %v", resp[0])
	}
	list := resp[0].Args["list"].([]any)
	if len(list) == 0 {
		t.Fatal("no email")
	}
	values, ok := list[0].(map[string]any)["bodyValues"].(map[string]any)
	if !ok || len(values) == 0 {
		t.Fatalf("bodyValues = %v", list[0].(map[string]any)["bodyValues"])
	}
	for _, v := range values {
		bv := v.(map[string]any)
		if _, ok := bv["isEncodingProblem"].(bool); !ok {
			t.Errorf("isEncodingProblem = %v, want bool", bv["isEncodingProblem"])
		}
		truncated, ok := bv["isTruncated"].(bool)
		if !ok {
			t.Errorf("isTruncated = %v, want bool", bv["isTruncated"])
		}
		if !truncated {
			t.Errorf("isTruncated = false, want true after maxBodyValueBytes")
		}
		if len(bv["value"].(string)) > 12 {
			t.Errorf("value length = %d, want <= 12", len(bv["value"].(string)))
		}
	}
}

// TestResultReferenceWildcard pins RFC 8620 §3.7's "/list/*/id" shape.
func TestResultReferenceWildcard(t *testing.T) {
	acct := testAccount(fixture.New())
	resp := dispatch(t, acct,
		[]any{"Mailbox/get", map[string]any{"accountId": "acct1", "ids": nil}, "g1"},
		[]any{"Mailbox/get", map[string]any{
			"accountId": "acct1",
			"#ids": map[string]any{
				"resultOf": "g1", "name": "Mailbox/get", "path": "/list/*/id",
			},
		}, "g2"},
	)
	if resp[1].Name != "Mailbox/get" {
		t.Fatalf("second response = %v, want Mailbox/get", resp[1])
	}
	want := map[string]bool{}
	for _, entry := range resp[0].Args["list"].([]any) {
		want[entry.(map[string]any)["id"].(string)] = true
	}
	got := resp[1].Args["list"].([]any)
	if len(got) != len(want) {
		t.Fatalf("wildcard resolved %d ids, want %d", len(got), len(want))
	}
	for _, entry := range got {
		if !want[entry.(map[string]any)["id"].(string)] {
			t.Errorf("unexpected id %v", entry)
		}
	}
}

// TestEmailQueryNegativePositionAndAnchorNotFound pins RFC 8620 §5.5.
func TestEmailQueryNegativePositionAndAnchorNotFound(t *testing.T) {
	acct := testAccount(fixture.New())
	all := dispatch(t, acct, []any{"Email/query", map[string]any{
		"accountId": "acct1", "calculateTotal": true,
	}, "q"})
	ids := all[0].Args["ids"].([]any)
	total := int(all[0].Args["total"].(float64))
	if total < 3 {
		t.Skipf("fixture only has %d emails", total)
	}

	neg := dispatch(t, acct, []any{"Email/query", map[string]any{
		"accountId": "acct1", "position": float64(-3), "calculateTotal": true,
	}, "q"})
	got := neg[0].Args["ids"].([]any)
	if len(got) == 0 || got[0] != ids[total-3] {
		t.Errorf("negative position start = %v, want %v", got, ids[total-3])
	}

	miss := dispatch(t, acct, []any{"Email/query", map[string]any{
		"accountId": "acct1", "anchor": "nonexistent-email-xyz",
	}, "q"})
	if miss[0].Name != "error" || miss[0].Args["type"] != "anchorNotFound" {
		t.Errorf("unknown anchor = %v, want anchorNotFound", miss[0])
	}
}

// newRecordingBackend returns a backend that mints a mailbox id and
// records the mailbox ids each created draft targeted.
func newRecordingBackend() *recordingBackend {
	return &recordingBackend{fakeBackend: *newFakeBackend()}
}

// TestCreationReferenceResolvesMailbox pins RFC 8620 §5.3: an Email/set
// create may reference a mailbox created earlier in the same batch.
func TestCreationReferenceResolvesMailbox(t *testing.T) {
	back := newRecordingBackend()
	acct := testAccount(fixture.New())
	acct.Backend = back
	resp := dispatch(t, acct,
		[]any{"Mailbox/set", map[string]any{
			"accountId": "acct1",
			"create":    map[string]any{"newMb": map[string]any{"name": "Creation Ref Test", "parentId": nil}},
		}, "mb"},
		[]any{"Email/set", map[string]any{
			"accountId": "acct1",
			"create": map[string]any{"refEmail": map[string]any{
				"mailboxIds":    map[string]any{"#newMb": true},
				"from":          []any{map[string]any{"name": "Test", "email": "test@example.com"}},
				"to":            []any{map[string]any{"name": "User", "email": "user@example.com"}},
				"subject":       "Creation ref test",
				"bodyStructure": map[string]any{"type": "text/plain", "partId": "1"},
				"bodyValues":    map[string]any{"1": map[string]any{"value": "body"}},
			}},
		}, "em"},
	)
	if resp[1].Name != "Email/set" {
		t.Fatalf("Email/set response = %v", resp[1])
	}
	created, ok := resp[1].Args["created"].(map[string]any)
	if !ok {
		t.Fatalf("Email/set created = %v (notCreated %v)", resp[1].Args["created"], resp[1].Args["notCreated"])
	}
	if _, ok := created["refEmail"]; !ok {
		t.Fatalf("Email not created: %v", resp[1].Args)
	}
	if len(back.drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(back.drafts))
	}
	if len(back.drafts[0].MailboxIDs) != 1 || back.drafts[0].MailboxIDs[0] != "mb-new" {
		t.Errorf("draft mailboxIds = %v, want [mb-new]", back.drafts[0].MailboxIDs)
	}
}

// recordingBackend overlays a real mailbox/create result on fakeBackend.
type recordingBackend struct {
	fakeBackend
	drafts []jmapapi.DraftSpec
}

func (b *recordingBackend) CreateMailbox(context.Context, string, string, string, int) (string, error) {
	return "mb-new", nil
}

func (b *recordingBackend) CreateDraft(_ context.Context, _ string, spec jmapapi.DraftSpec) (*jmapapi.CreatedEmail, error) {
	b.drafts = append(b.drafts, spec)
	return &jmapapi.CreatedEmail{ID: "em-new", BlobID: "blob-1", ThreadID: "thr-1", Size: 10}, nil
}

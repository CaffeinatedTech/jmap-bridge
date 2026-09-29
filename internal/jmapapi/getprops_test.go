package jmapapi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// TestGetAlwaysReturnsID pins RFC 8620 §5.1: "The id property of the
// object is *always* returned, even if not explicitly requested." A
// client that asked for a narrow property list and got no ids back
// cannot key its cache — which is exactly how the M2 gate caught it
// (jmap-tui's list windows come from a properties-restricted Email/get).
func TestGetAlwaysReturnsID(t *testing.T) {
	for _, tc := range []struct {
		method string
		prop   string
	}{{"Email/get", "subject"}, {"Mailbox/get", "name"}} {
		st := fixture.New()
		h := jmapapi.NewHandler(st)
		acct := &jmapapi.Account{ID: "acct1", Store: st}
		body, err := json.Marshal(map[string]any{
			"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
			"methodCalls": []any{[]any{tc.method, map[string]any{
				"accountId": "acct1", "properties": []string{tc.prop},
			}, "c1"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		status, out := h.Dispatch(context.Background(), acct, body)
		if status != 200 {
			t.Fatalf("%s: HTTP %d", tc.method, status)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			MethodResponses [][]json.RawMessage `json:"methodResponses"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil || len(wire.MethodResponses) == 0 {
			t.Fatalf("%s: bad response: %s", tc.method, raw)
		}
		var args struct {
			List []map[string]any `json:"list"`
		}
		if err := json.Unmarshal(wire.MethodResponses[0][1], &args); err != nil {
			t.Fatalf("%s: decode: %v", tc.method, err)
		}
		if len(args.List) == 0 {
			t.Fatalf("%s: empty list", tc.method)
		}
		for i, obj := range args.List {
			id, _ := obj["id"].(string)
			if id == "" {
				t.Errorf("%s object %d has no id with properties=[%s]: %v",
					tc.method, i, tc.prop, obj)
			}
		}
	}
}

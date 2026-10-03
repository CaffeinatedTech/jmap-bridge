package jmapapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/metrics"
)

// FR-D.6: every dispatched call is counted by account and method — even
// an unknown one — and a failed call is counted by error type.
func TestMetricsCountMethodsAndErrors(t *testing.T) {
	reg := metrics.New()
	st := fixture.New()
	h := jmapapi.NewHandler(st, jmapapi.WithMetrics(reg))
	acct := &jmapapi.Account{ID: "acct1", Store: st, Capabilities: mailCaps}
	body, err := json.Marshal(map[string]any{
		"using": mailCaps,
		"methodCalls": []any{
			[]any{"Mailbox/get", map[string]any{"accountId": "acct1"}, "c1"},
			[]any{"Bogus/method", map[string]any{}, "c2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := h.Dispatch(context.Background(), acct, body); status != 200 {
		t.Fatalf("Dispatch status = %d, want 200", status)
	}

	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	got := b.String()
	for _, want := range []string{
		`jmap_bridge_method_calls_total{account="acct1",method="Mailbox/get"} 1`,
		`jmap_bridge_method_calls_total{account="acct1",method="Bogus/method"} 1`,
		`jmap_bridge_method_errors_total{account="acct1",method="Bogus/method",type="unknownMethod"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, got)
		}
	}
}

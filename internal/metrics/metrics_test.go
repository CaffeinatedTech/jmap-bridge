package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestExpositionContainsHelpTypeAndSeries(t *testing.T) {
	r := New()
	calls := r.Counter("jmap_bridge_method_calls_total", "JMAP method calls.", "account", "method")
	calls.With("personal", "Email/get").Add(3)
	calls.With("work", "Mailbox/get").Inc()
	r.GaugeFunc("jmap_bridge_sync_lag_seconds", "Seconds since the last pass.", func() []Sample {
		return []Sample{
			{Labels: []Label{{Name: "account", Value: "personal"}}, Value: 12.5},
		}
	})

	var b strings.Builder
	n, err := r.WriteTo(&b)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(b.Len()) {
		t.Fatalf("WriteTo reported %d bytes, buffer holds %d", n, b.Len())
	}
	got := b.String()
	for _, want := range []string{
		"# HELP jmap_bridge_method_calls_total JMAP method calls.\n",
		"# TYPE jmap_bridge_method_calls_total counter\n",
		`jmap_bridge_method_calls_total{account="personal",method="Email/get"} 3`,
		`jmap_bridge_method_calls_total{account="work",method="Mailbox/get"} 1`,
		"# HELP jmap_bridge_sync_lag_seconds Seconds since the last pass.\n",
		"# TYPE jmap_bridge_sync_lag_seconds gauge\n",
		`jmap_bridge_sync_lag_seconds{account="personal"} 12.5`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, got)
		}
	}
	// HELP/TYPE must precede the family's series.
	if strings.Index(got, "# TYPE jmap_bridge_method_calls_total") >
		strings.Index(got, `jmap_bridge_method_calls_total{`) {
		t.Errorf("TYPE must precede counter series\n%s", got)
	}
	// Families are ordered by name; sync_lag sorts before method_calls?
	// No: "jmap_bridge_method..." < "jmap_bridge_sync...", so method
	// first.
	if strings.Index(got, "jmap_bridge_method_calls_total") >
		strings.Index(got, "jmap_bridge_sync_lag_seconds") {
		t.Errorf("families are not sorted by name\n%s", got)
	}
}

// TestGaugeFamilyAccumulatesSources pins the multi-account fix: every sync
// engine registers the same per-account gauge names on one registry, so a
// repeated GaugeFunc must add a source rather than panic (the v0.1.6
// startup crash with two accounts).
func TestGaugeFamilyAccumulatesSources(t *testing.T) {
	r := New()
	help := "Seconds since the last pass."
	r.GaugeFunc("jmap_bridge_sync_lag_seconds", help, func() []Sample {
		return []Sample{{Labels: []Label{{Name: "account", Value: "personal"}}, Value: 1}}
	})
	r.GaugeFunc("jmap_bridge_sync_lag_seconds", help, func() []Sample {
		return []Sample{{Labels: []Label{{Name: "account", Value: "work"}}, Value: 2}}
	})

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	got := b.String()
	if strings.Count(got, "# TYPE jmap_bridge_sync_lag_seconds") != 1 {
		t.Errorf("family emitted more than one TYPE line:\n%s", got)
	}
	for _, want := range []string{
		`jmap_bridge_sync_lag_seconds{account="personal"} 1`,
		`jmap_bridge_sync_lag_seconds{account="work"} 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exposition missing %q\n%s", want, got)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	v := r.Counter("c", "help", "account")
	v.With(`a"b\c` + "\nd").Inc()

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	got := b.String()
	want := `c{account="a\"b\\c\nd"} 1` + "\n"
	if !strings.Contains(got, want) {
		t.Fatalf("escaping wrong:\n got %q\nwant %q", got, want)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	render := func() string {
		r := New()
		v := r.Counter("c", "help", "account", "method")
		v.With("work", "Email/get").Inc()
		v.With("personal", "Email/get").Inc()
		v.With("personal", "Mailbox/get").Inc()
		var b strings.Builder
		_, _ = r.WriteTo(&b)
		return b.String()
	}
	first := render()
	if second := render(); first != second {
		t.Fatalf("exposition not deterministic:\n%s\n---\n%s", first, second)
	}
	// Series sort by label tuple: account asc, then method asc.
	if strings.Index(first, `account="personal",method="Email/get"`) >
		strings.Index(first, `account="personal",method="Mailbox/get"`) {
		t.Fatalf("series not sorted by label tuple:\n%s", first)
	}
	if strings.Index(first, `account="personal",method="Mailbox/get"`) >
		strings.Index(first, `account="work",method="Email/get"`) {
		t.Fatalf("series not sorted by label tuple:\n%s", first)
	}
}

func TestEmptyRegistryWritesNothing(t *testing.T) {
	r := New()
	var b strings.Builder
	n, err := r.WriteTo(&b)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != 0 || b.Len() != 0 {
		t.Fatalf("empty registry wrote %d bytes: %q", n, b.String())
	}
}

func TestWrongCardinalityPanics(t *testing.T) {
	r := New()
	v := r.Counter("c", "help", "account", "method")
	defer func() {
		if recover() == nil {
			t.Fatal("With with wrong cardinality did not panic")
		}
	}()
	v.With("only-one")
}

func TestConcurrentInc(t *testing.T) {
	r := New()
	v := r.Counter("c", "help", "account")
	const goroutines = 8
	const perGoroutine = 1000
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := v.With("personal")
			for j := 0; j < perGoroutine; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	want := `c{account="personal"} 8000`
	if !strings.Contains(b.String(), want) {
		t.Fatalf("lost increments: %s", b.String())
	}
}

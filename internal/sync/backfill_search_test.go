package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

// TestSearchBackfillHydratesLateMatches is the engine half of FR-X.5:
// a text query answers from headers at once, unhydrated candidates in
// scope hydrate in the background, and the body-only match shows up in
// a later query with a moved queryState — without any prefetch window.
func TestSearchBackfillHydratesLateMatches(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, func(c *Config) {
		c.SearchBackfill = true
		c.Concurrency = 4
	})
	env.fx.Append("INBOX", raw("Alpha report", "<alpha@example.test>", "plain body"))
	env.fx.Append("INBOX", raw("Beta memo", "<beta@example.test>", "the zebra escaped"))
	waitUntil(t, 5*time.Second, "backfill", func() bool {
		emails := env.emails(t)
		return len(emails) == 2
	})

	q := jmapapi.EmailQuery{Filter: jmapapi.EmailFilter{Text: "zebra"}}
	ids, _, total, counter, err := env.st.QueryEmails(context.Background(), "acct", q)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(ids) != 0 {
		t.Fatalf("pre-backfill zebra = %v/%d, want none", ids, total)
	}

	waitUntil(t, 10*time.Second, "search-driven hydration", func() bool {
		_, _, total, after, err := env.st.QueryEmails(context.Background(), "acct", q)
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			return false
		}
		if after == counter {
			t.Errorf("queryState unchanged after body tokens arrived (FR-X.7)")
		}
		return true
	})

	// The hydration came through the backfill lane, not an interactive
	// read. Both unhydrated messages in scope are candidates (the scan
	// cannot know which will match bodies), and the IsHydrated
	// short-circuit must keep the total at exactly that — a re-fetch
	// storm on repeated queries is the failure mode this guards.
	waitUntil(t, 10*time.Second, "both candidates hydrated", func() bool {
		return env.eng.hydrateFetches.Load() == 2
	})
	time.Sleep(300 * time.Millisecond) // settle: a re-fetch would show here
	if n := env.eng.hydrateFetches.Load(); n != 2 {
		t.Errorf("body fetches = %d, want exactly the two scoped candidates", n)
	}
}

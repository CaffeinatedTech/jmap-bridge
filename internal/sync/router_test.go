package sync

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// A process with more than one account shares one store, and the store
// holds a single Ensure hook. Before Router, each New overwrote it, so
// only the last engine built was reachable and every other account's
// uncached-body read failed as "ensure for foreign account".
func TestRouterDispatchesByAccount(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(Config{Account: "a"}, st, log)
	b := New(Config{Account: "b"}, st, log)
	// New claims the hooks per engine (last wins); Router replaces them,
	// exactly as cmd/jmap-bridge wires it after building every engine.
	st.Ensure, st.SearchBackfill = Router(map[string]*Engine{"a": a, "b": b}, false)

	// Empty id lists must not touch the network, so this exercises the
	// account check alone.
	for _, account := range []string{"a", "b", "cache-only"} {
		if err := st.Ensure(context.Background(), account, nil, nil); err != nil {
			t.Errorf("Ensure(%q) = %v, want nil", account, err)
		}
	}
	if st.SearchBackfill != nil {
		t.Error("SearchBackfill set with backfill disabled (FR-X.8)")
	}
}

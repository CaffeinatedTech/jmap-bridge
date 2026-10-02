package sync

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// A message with a malformed header line makes go-message return a hard
// error and no parts. storeBody must cache that as hydrated-empty rather
// than fail: failing would re-fetch the same bytes on every read forever
// and warn once per list view (FR-S.8, FR-X.6).
func TestStoreBodyCachesUnparseableAsEmpty(t *testing.T) {
	st := gmStore(t)
	id := gmSeedPerFolder(t, st, map[string]uint32{"INBOX": 4242})
	e := New(Config{Account: "acct"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := []byte("From: a@b\r\nNotAHeader\r\nSubject: x\r\n\r\nbody\r\n")
	if err := e.storeBody(id, raw); err != nil {
		t.Fatalf("storeBody = %v, want nil (cache empty, no retry)", err)
	}
	hydrated, err := st.IsHydrated(context.Background(), "acct", id)
	if err != nil {
		t.Fatal(err)
	}
	if !hydrated {
		t.Fatal("unparseable body not marked hydrated: it will be re-fetched on every read")
	}
}

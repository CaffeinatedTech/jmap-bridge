package sync

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
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

// FR-D.12: message bodies never reach the logs. go-message's parse
// errors embed the offending bytes ("malformed MIME header line: …"), so
// storeBody must not log the error text — the sentinel below appears in
// the error but must not appear in the captured log output.
func TestStoreBodyNeverLogsMessageBytes(t *testing.T) {
	const sentinel = "SECRET-BODY-SENTINEL-4f3a"
	st := gmStore(t)
	id := gmSeedPerFolder(t, st, map[string]uint32{"INBOX": 4242})
	var logs bytes.Buffer
	e := New(Config{Account: "acct"}, st, slog.New(slog.NewTextHandler(&logs, nil)))

	raw := []byte("From: a@b\r\n" + sentinel + "\r\nSubject: x\r\n\r\n" + sentinel + "\r\n")
	if err := e.storeBody(id, raw); err != nil {
		t.Fatalf("storeBody = %v, want nil", err)
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("message body reached the logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "no readable parts") {
		t.Fatalf("expected the partless warning, got:\n%s", logs.String())
	}
}

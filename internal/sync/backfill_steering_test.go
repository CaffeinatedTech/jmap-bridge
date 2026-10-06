package sync

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// steerBackend is a minimal mailbackend that never finishes BIG's
// backfill, so a pass that visits folders in map order and backfills each
// to completion would never reach INBOX. It records the visit order.
type steerBackend struct {
	mu           sync.Mutex
	events       []string
	backfillWait time.Duration
}

func (f *steerBackend) record(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *steerBackend) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *steerBackend) Kind() mb.Kind                 { return mb.KindGmailAPI }
func (f *steerBackend) Capabilities() mb.Capabilities { return mb.Capabilities{} }
func (f *steerBackend) Connect(context.Context) error { return nil }
func (f *steerBackend) Close() error                  { return nil }
func (f *steerBackend) Ping(context.Context) error    { return nil }
func (f *steerBackend) Release(context.Context) error { return nil }

func (f *steerBackend) Folders(context.Context) ([]mb.Folder, error) {
	// BIG first: map order must not decide, or a not-done BIG starves INBOX.
	return []mb.Folder{
		{Container: "BIG", Name: "BIG"},
		{Container: "INBOX", Name: "INBOX", Role: "inbox"},
	}, nil
}

func (f *steerBackend) FolderStatus(context.Context, string) (mb.FolderStatus, error) {
	return mb.FolderStatus{Version: 1, ModSeq: 5, Messages: 10}, nil
}

func (f *steerBackend) Backfill(ctx context.Context, container string, c mb.Cursor, _ int, _ mb.Hooks) ([]mb.Header, mb.Cursor, error) {
	f.record("back:" + container)
	if f.backfillWait > 0 {
		select {
		case <-ctx.Done():
			return nil, c, ctx.Err()
		case <-time.After(f.backfillWait):
		}
	}
	c.Version = 1
	c.BackfillMark++
	c.BackfillDone = false // never finishes
	return nil, c, nil
}

func (f *steerBackend) Incremental(_ context.Context, container string, c mb.Cursor, _ mb.Hooks) (mb.Delta, mb.Cursor, error) {
	f.record("inc:" + container)
	c.Version = 1
	if c.ModSeq == 0 {
		c.ModSeq = 5
	}
	return mb.Delta{}, c, nil
}

func (f *steerBackend) FetchHeaders(context.Context, string, []mb.Ref) ([]mb.Header, error) {
	return nil, nil
}
func (f *steerBackend) Watch(context.Context, string) (<-chan struct{}, error) { return nil, nil }
func (f *steerBackend) FetchPreviews(context.Context, []mb.Ref) (map[mb.Ref]string, error) {
	return nil, nil
}
func (f *steerBackend) FetchRaw(context.Context, mb.Ref) ([]byte, error) { return nil, nil }
func (f *steerBackend) FetchRawBatch(context.Context, string, []mb.Ref) (map[mb.Ref][]byte, error) {
	return nil, nil
}

func (f *steerBackend) StoreKeywords(context.Context, []mb.Copy, []string, []string) error {
	return nil
}

func (f *steerBackend) SetMembership(context.Context, mb.Message, []mb.Copy, []mb.Mailbox, []mb.Mailbox) ([]mb.Copy, []mb.Ref, error) {
	return nil, nil, nil
}

func (f *steerBackend) Destroy(context.Context, []mb.Ref, string) ([]mb.Ref, error) {
	return nil, nil
}

func (f *steerBackend) Append(context.Context, string, []byte, []string, *time.Time) (mb.Ref, []string, error) {
	return mb.Ref{}, nil, nil
}
func (f *steerBackend) CreateMailbox(context.Context, string) error         { return nil }
func (f *steerBackend) RenameMailbox(context.Context, string, string) error { return nil }
func (f *steerBackend) DeleteMailbox(context.Context, string) error         { return nil }
func (f *steerBackend) HierarchyDelim(context.Context) (rune, error)        { return '/', nil }

// TestPassSteersIncrementalAheadOfColdBackfill is the regression for the
// live failure where a Gmail account stopped showing new mail because the
// pass walked a huge un-backfilled label to completion before reaching
// INBOX. With the budget and ordering, INBOX incremental runs first and
// the pass returns even though BIG is never done.
func TestPassSteersIncrementalAheadOfColdBackfill(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// INBOX is already backfilled; BIG is still cold.
	if err := st.SaveFolderSync(context.Background(), "acct", "INBOX", store.FolderSync{
		UIDValidity: 1, UIDNext: 0, HighestModSeq: 5, BackfillDone: true,
	}); err != nil {
		t.Fatalf("seed INBOX sync: %v", err)
	}

	be := &steerBackend{backfillWait: 15 * time.Millisecond}
	eng := New(Config{
		Account:        "acct",
		Interval:       time.Hour,
		BatchSize:      100,
		Concurrency:    1,
		BackfillBudget: 25 * time.Millisecond,
	}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.work = be
	eng.connected = true

	if err := eng.doPass(context.Background(), ""); err != nil {
		t.Fatalf("doPass: %v", err)
	}

	events := be.log()
	if len(events) == 0 || events[0] != "inc:INBOX" {
		t.Fatalf("pass order = %v, want INBOX incremental first", events)
	}
	for _, e := range events {
		if e == "inc:BIG" {
			t.Fatalf("BIG ran incremental while still backfilling: %v", events)
		}
	}
	backfills := 0
	for _, e := range events {
		if e == "back:BIG" {
			backfills++
		}
	}
	if backfills == 0 {
		t.Fatalf("BIG made no backfill progress: %v", events)
	}
}

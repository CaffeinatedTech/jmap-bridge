package gmailapi

import (
	"context"
	"sync"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// fakeNative is an in-memory NativeIndex standing in for the store's
// native_ids table in adapter tests (the real one is exercised by the
// store and sync suites).
type fakeNative struct {
	mu       sync.Mutex
	nextUID  uint32
	byNative map[string]uint32
	byUID    map[uint32]string
	members  map[string]map[uint32]bool
}

func newFakeNative() *fakeNative {
	return &fakeNative{
		byNative: map[string]uint32{},
		byUID:    map[uint32]string{},
		members:  map[string]map[uint32]bool{},
	}
}

func (f *fakeNative) alloc(native string) uint32 {
	if uid, ok := f.byNative[native]; ok {
		return uid
	}
	f.nextUID++
	uid := f.nextUID
	f.byNative[native] = uid
	f.byUID[uid] = native
	return uid
}

func (f *fakeNative) NativeUID(_ context.Context, native string) (uint32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := f.byNative[native]
	return uid, ok, nil
}

func (f *fakeNative) AllocNativeUIDs(_ context.Context, natives []string) (map[string]uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]uint32, len(natives))
	for _, n := range natives {
		out[n] = f.alloc(n)
	}
	return out, nil
}

func (f *fakeNative) NativeByUID(_ context.Context, uid uint32) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.byUID[uid]
	return n, ok, nil
}

func (f *fakeNative) NativeByUIDs(_ context.Context, uids []uint32) (map[uint32]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uint32]string, len(uids))
	for _, u := range uids {
		if n, ok := f.byUID[u]; ok {
			out[u] = n
		}
	}
	return out, nil
}

func (f *fakeNative) KnownMemberUIDs(_ context.Context, container string, uids []uint32) (map[uint32]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uint32]bool{}
	for _, u := range uids {
		if f.members[container][u] {
			out[u] = true
		}
	}
	return out, nil
}

func (f *fakeNative) markMember(container, native string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid := f.alloc(native)
	if f.members[container] == nil {
		f.members[container] = map[uint32]bool{}
	}
	f.members[container][uid] = true
}

func newAdapter(t *testing.T, fx *fixturegmail.Server, n NativeIndex) *Backend {
	t.Helper()
	return NewBackend(Config{
		Account: "gmail", Client: newTestClient(t, fx), Native: n, BackfillLimit: 0,
	})
}

func seedLabels(fx *fixturegmail.Server) {
	fx.SeedLabel(fixturegmail.LabelInbox, "INBOX", "system")
	fx.SeedLabel(fixturegmail.LabelSent, "SENT", "system")
	fx.SeedLabel("Label_work", "Work", "user")
}

func seedMessage(fx *fixturegmail.Server, id, thread string, labels []string, subject, messageID string, unread bool) {
	if unread {
		labels = append(append([]string(nil), labels...), fixturegmail.LabelUnread)
	}
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: id, ThreadID: thread, LabelIDs: labels,
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "Alice <alice@example.test>"},
			{Name: "To", Value: "Bob <bob@example.test>"},
			{Name: "Subject", Value: subject},
			{Name: "Date", Value: "Mon, 02 Jan 2026 15:04:05 -0700"},
			{Name: "Message-ID", Value: messageID},
		},
		Raw: []byte("From: alice@example.test\r\nSubject: " + subject + "\r\nMessage-ID: " + messageID + "\r\n\r\nbody\r\n"),
	})
}

func TestAdapterDiscoveryAndBackfill(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedLabels(fx)
	seedMessage(fx, "m1", "thr1", []string{fixturegmail.LabelInbox}, "Hello", "<m1@x>", true)
	seedMessage(fx, "m2", "thr2", []string{fixturegmail.LabelInbox, "Label_work"}, "Work", "<m2@x>", false)
	seedMessage(fx, "m3", "thr1", []string{fixturegmail.LabelSent}, "Re: Hello", "<m3@x>", false)

	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	folders, err := b.Folders(ctx)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	byName := map[string]mb.Folder{}
	for _, f := range folders {
		byName[f.Name] = f
	}
	if byName["INBOX"].Role != "inbox" || byName["Sent"].Role != "sent" {
		t.Fatalf("system roles wrong: %+v", byName)
	}
	if !byName[allMailName].Implicit || byName[allMailName].Role != "archive" {
		t.Fatalf("All Mail not synthetic archive: %+v", byName[allMailName])
	}
	if byName["Work"].Role != "" || byName["Work"].Delim != '/' {
		t.Fatalf("user label mapping wrong: %+v", byName["Work"])
	}

	st, err := b.FolderStatus(ctx, "INBOX")
	if err != nil {
		t.Fatalf("FolderStatus: %v", err)
	}
	if st.Version != apiUIDValidity || st.ModSeq != fx.CurrentHistoryID() {
		t.Fatalf("INBOX status = %+v (history %d)", st, fx.CurrentHistoryID())
	}

	headers, cursor, err := b.Backfill(ctx, "INBOX", mb.Cursor{Version: apiUIDValidity, Next: 0, ModSeq: st.ModSeq}, 100, mb.Hooks{})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if !cursor.BackfillDone || len(headers) != 2 {
		t.Fatalf("backfill got %d headers done=%v", len(headers), cursor.BackfillDone)
	}
	flags := map[string][]string{}
	for _, h := range headers {
		flags[h.Envelope.MessageID] = h.Flags
	}
	if keyword.FromIMAP(flags["<m1@x>"])["$seen"] {
		t.Fatalf("m1 should be unread: %v", flags["<m1@x>"])
	}
	if !keyword.FromIMAP(flags["<m2@x>"])["$seen"] {
		t.Fatalf("m2 should be seen: %v", flags["<m2@x>"])
	}
}

func TestAdapterHistoryIncremental(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedLabels(fx)
	seedMessage(fx, "m1", "thr1", []string{fixturegmail.LabelInbox}, "Hello", "<m1@x>", false)
	seedMessage(fx, "m2", "thr2", []string{fixturegmail.LabelInbox}, "Two", "<m2@x>", false)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()
	if _, err := b.Folders(ctx); err != nil {
		t.Fatalf("Folders: %v", err)
	}
	start := fx.CurrentHistoryID()

	// Foreign changes: a new inbox message and a label change on m1.
	fx.ForeignAddMessage(fixturegmail.SeedMessage{
		ID: "m9", ThreadID: "thr9", LabelIDs: []string{fixturegmail.LabelInbox},
		Headers: []fixturegmail.Header{{Name: "Message-ID", Value: "<m9@x>"}, {Name: "Subject", Value: "New"}},
		Raw:     []byte("Message-ID: <m9@x>\r\nSubject: New\r\n\r\nx\r\n"),
	})
	fx.ForeignModifyLabels("m1", []string{fixturegmail.LabelStarred}, nil)

	n.markMember("INBOX", "m1")
	n.markMember("INBOX", "m2")
	hooks := mb.Hooks{KnownUIDs: func() ([]mb.Ref, error) {
		u1, _, _ := n.NativeUID(ctx, "m1")
		u2, _, _ := n.NativeUID(ctx, "m2")
		return []mb.Ref{mb.NewRef("INBOX", apiUIDValidity, u1), mb.NewRef("INBOX", apiUIDValidity, u2)}, nil
	}}

	delta, next, err := b.Incremental(ctx, "INBOX", mb.Cursor{Version: apiUIDValidity, ModSeq: start}, hooks)
	if err != nil {
		t.Fatalf("Incremental: %v", err)
	}
	if next.ModSeq <= start {
		t.Fatalf("history cursor did not advance: %d -> %d", start, next.ModSeq)
	}
	if len(delta.Headers) != 1 || delta.Headers[0].Envelope.MessageID != "<m9@x>" {
		t.Fatalf("expected one new header m9, got %+v", delta.Headers)
	}
	if len(delta.Flags) != 1 || !keyword.FromIMAP(delta.Flags[0].Flags)["$flagged"] {
		t.Fatalf("expected m1 flagged, got %+v", delta.Flags)
	}
	if len(delta.Removed) != 0 {
		t.Fatalf("unexpected removals: %+v", delta.Removed)
	}
}

func TestAdapterHydration(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedLabels(fx)
	raw := []byte("From: alice@example.test\r\nSubject: Hi\r\nMessage-ID: <h1@x>\r\n\r\nbody bytes\r\n")
	fx.SeedMessage(fixturegmail.SeedMessage{ID: "m1", ThreadID: "t1", LabelIDs: []string{fixturegmail.LabelInbox}, Raw: raw})
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()
	uid := n.alloc("m1")
	got, err := b.FetchRaw(ctx, mb.NewRef("INBOX", apiUIDValidity, uid))
	if err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("raw mismatch: %q", got)
	}
	batch, err := b.FetchRawBatch(ctx, "INBOX", []mb.Ref{mb.NewRef("INBOX", apiUIDValidity, uid)})
	if err != nil {
		t.Fatalf("FetchRawBatch: %v", err)
	}
	if string(batch[mb.NewRef("INBOX", apiUIDValidity, uid)]) != string(raw) {
		t.Fatalf("batch raw mismatch")
	}
}

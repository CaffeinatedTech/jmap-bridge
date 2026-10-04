package gmailapi

import (
	"context"
	"errors"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// seedWriteLabels seeds the containers the write tests address: the
// system roles mapLabel recognises plus one user label.
func seedWriteLabels(fx *fixturegmail.Server) {
	seedLabels(fx)
	fx.SeedLabel(fixturegmail.LabelDraft, "Drafts", "system")
	fx.SeedLabel(fixturegmail.LabelTrash, "Trash", "system")
}

func writeCopy(n *fakeNative, mailbox, native string) mb.Copy {
	return mb.Copy{
		MailboxID: mailbox,
		Ref:       mb.NewRef(mailbox, apiUIDValidity, n.alloc(native)),
	}
}

// raw builds a minimal RFC 5322 message for the draft tests.
func raw(subject, msgid, body string) []byte {
	return []byte("From: me@example.test\r\nTo: you@example.test\r\nSubject: " + subject +
		"\r\nMessage-ID: " + msgid + "\r\n\r\n" + body + "\r\n")
}

func TestAdapterStoreKeywords(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	seedMessage(fx, "m1", "t1", []string{fixturegmail.LabelInbox}, "Hi", "<m1@x>", true)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	if err := b.StoreKeywords(ctx, []mb.Copy{writeCopy(n, "INBOX", "m1")}, []string{KeywordSeen, KeywordFlagged}, nil); err != nil {
		t.Fatalf("StoreKeywords: %v", err)
	}
	labels, ok := fx.MessageLabels("m1")
	if !ok {
		t.Fatal("m1 vanished")
	}
	have := map[string]bool{}
	for _, id := range labels {
		have[id] = true
	}
	if !have[fixturegmail.LabelStarred] || have[fixturegmail.LabelUnread] {
		t.Fatalf("labels after store = %v, want STARRED and no UNREAD", labels)
	}
}

func TestAdapterStoreKeywordsRefusesUnsupported(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	seedMessage(fx, "m1", "t1", []string{fixturegmail.LabelInbox}, "Hi", "<m1@x>", false)
	n := newFakeNative()
	b := newAdapter(t, fx, n)

	err := b.StoreKeywords(context.Background(), []mb.Copy{writeCopy(n, "INBOX", "m1")}, []string{"$answered"}, nil)
	var unsupported *mb.UnsupportedKeywordsError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want UnsupportedKeywordsError", err)
	}
	if len(unsupported.Keywords) != 1 || unsupported.Keywords[0] != "$answered" {
		t.Fatalf("unsupported = %v", unsupported.Keywords)
	}
}

func TestAdapterSetMembership(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	seedMessage(fx, "m1", "t1", []string{fixturegmail.LabelInbox}, "Hi", "<m1@x>", false)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	added, touched, err := b.SetMembership(ctx, mb.Message{ID: "e1"}, []mb.Copy{writeCopy(n, "INBOX", "m1")},
		[]mb.Mailbox{{ID: "mbx-work", Path: "Work"}}, []mb.Mailbox{{ID: "mbx-inbox", Path: "INBOX"}})
	if err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	if len(added) != 1 || added[0].MailboxID != "mbx-work" {
		t.Fatalf("added = %+v", added)
	}
	if len(touched) != 1 {
		t.Fatalf("touched = %+v", touched)
	}
	labels, _ := fx.MessageLabels("m1")
	have := map[string]bool{}
	for _, id := range labels {
		have[id] = true
	}
	if !have["Label_work"] || have[fixturegmail.LabelInbox] {
		t.Fatalf("labels after move = %v", labels)
	}
}

func TestAdapterSetMembershipRefusesImplicitRemove(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	seedMessage(fx, "m1", "t1", []string{fixturegmail.LabelInbox}, "Hi", "<m1@x>", false)
	n := newFakeNative()
	b := newAdapter(t, fx, n)

	_, _, err := b.SetMembership(context.Background(), mb.Message{ID: "e1"},
		[]mb.Copy{writeCopy(n, "INBOX", "m1")}, nil,
		[]mb.Mailbox{{ID: "mbx-all", Path: allMailName, Implicit: true}})
	if !mb.IsRejected(err) {
		t.Fatalf("err = %v, want RejectedError", err)
	}
}

func TestAdapterDestroy(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	fx.SeedMessage(fixturegmail.SeedMessage{ID: "m1", ThreadID: "t1", LabelIDs: []string{fixturegmail.LabelInbox}})
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()
	ref := writeCopy(n, "INBOX", "m1").Ref

	if _, err := b.Destroy(ctx, []mb.Ref{ref}, "Trash"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := fx.MessageLabels("m1"); ok {
		t.Fatal("message still present after destroy")
	}
	// A second destroy is a no-op success (FR-M.10).
	if _, err := b.Destroy(ctx, []mb.Ref{ref}, "Trash"); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
}

func TestAdapterAppendDraft(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	ref, flags, err := b.Append(ctx, "Drafts", raw("Draft subject", "<d1@x>", "draft body"), []string{KeywordDraft}, nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	uid, ok := ref.UID()
	if !ok || uid == 0 {
		t.Fatalf("append ref = %+v", ref)
	}
	native, _, _ := n.NativeByUID(ctx, uid)
	if native == "" {
		t.Fatal("no native id mapped for the draft")
	}
	draftID := n.drafts[uid]
	if draftID == "" {
		t.Fatalf("draft handle not saved for uid %d", uid)
	}
	found := false
	for _, id := range fx.DraftIDs() {
		if id == draftID {
			found = true
		}
	}
	if !found {
		t.Fatalf("draft %q not in fixture drafts %v", draftID, fx.DraftIDs())
	}
	if !keyword.FromIMAP(flags)[KeywordDraft] {
		t.Fatalf("flags = %v, want $draft", flags)
	}
	// Gmail applies the DRAFT label to the stored message.
	if labels, ok := fx.MessageLabels(native); !ok || !containsString(labels, fixturegmail.LabelDraft) {
		t.Fatalf("draft message labels = %v ok=%v", labels, ok)
	}
}

func TestAdapterImportIntoLabel(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	// A $seen import lands in INBOX with no UNREAD (the reconcile removes
	// it if Gmail's delivery scan set it).
	ref, flags, err := b.Append(ctx, "INBOX", raw("Imported", "<imp1@x>", "body"), []string{KeywordSeen}, nil)
	if err != nil {
		t.Fatalf("Append import: %v", err)
	}
	uid, ok := ref.UID()
	if !ok || uid == 0 {
		t.Fatalf("import ref = %+v", ref)
	}
	native, _, _ := n.NativeByUID(ctx, uid)
	if native == "" {
		t.Fatal("no native id mapped for the import")
	}
	labels, ok := fx.MessageLabels(native)
	if !ok {
		t.Fatal("imported message not on the server")
	}
	if !containsString(labels, fixturegmail.LabelInbox) || containsString(labels, fixturegmail.LabelUnread) {
		t.Fatalf("imported labels = %v, want INBOX and no UNREAD", labels)
	}
	if !keyword.FromIMAP(flags)[KeywordSeen] {
		t.Fatalf("flags = %v, want $seen", flags)
	}

	// An import without $seen is unread on the server and reported so.
	_, flags, err = b.Append(ctx, "INBOX", raw("Unread", "<imp2@x>", "body"), nil, nil)
	if err != nil {
		t.Fatalf("Append unread import: %v", err)
	}
	if keyword.FromIMAP(flags)[KeywordSeen] {
		t.Fatalf("flags = %v, want unread (no $seen)", flags)
	}
}

func TestAdapterImportRefusesUnsupportedKeyword(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	b := newAdapter(t, fx, newFakeNative())

	_, _, err := b.Append(context.Background(), "INBOX", raw("x", "<x@x>", "y"), []string{"$answered"}, nil)
	var kwErr *mb.UnsupportedKeywordsError
	if !errors.As(err, &kwErr) {
		t.Fatalf("err = %v, want UnsupportedKeywordsError", err)
	}
}

func TestAdapterImportRejectsImplicitAndUnknown(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	b := newAdapter(t, fx, newFakeNative())
	ctx := context.Background()

	if _, _, err := b.Append(ctx, allMailName, raw("x", "<x@x>", "y"), nil, nil); !mb.IsRejected(err) {
		t.Fatalf("All Mail import err = %v, want RejectedError", err)
	}
	if _, _, err := b.Append(ctx, "No Such Label", raw("x", "<x@x>", "y"), nil, nil); err == nil {
		t.Fatal("unknown mailbox import did not error")
	}
}

func TestAdapterMailboxOps(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	b := newAdapter(t, fx, newFakeNative())
	ctx := context.Background()

	if err := b.CreateMailbox(ctx, "Projects"); err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if _, ok := fx.LabelIDByName("Projects"); !ok {
		t.Fatal("created label not listed")
	}
	if err := b.RenameMailbox(ctx, "Projects", "Projects/2026"); err != nil {
		t.Fatalf("RenameMailbox: %v", err)
	}
	if _, ok := fx.LabelIDByName("Projects/2026"); !ok {
		t.Fatal("renamed label not listed")
	}
	if _, ok := fx.LabelIDByName("Projects"); ok {
		t.Fatal("old label name still listed after rename")
	}
	if err := b.DeleteMailbox(ctx, "Projects/2026"); err != nil {
		t.Fatalf("DeleteMailbox: %v", err)
	}
	if _, ok := fx.LabelIDByName("Projects/2026"); ok {
		t.Fatal("deleted label still listed")
	}
}

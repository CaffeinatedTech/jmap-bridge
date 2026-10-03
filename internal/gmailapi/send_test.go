package gmailapi

import (
	"context"
	"testing"

	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// TestAdapterSendMessage relays a fresh message through messages.send and
// reports the provider's Sent copy (GMAIL_API_PLAN §8.1).
func TestAdapterSendMessage(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)

	res, err := b.Send(context.Background(), mb.SendRequest{
		Raw:       raw("Hello", "<send1@x>", "body"),
		MessageID: "<send1@x>",
		From:      "me@example.test",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if uid, ok := res.Ref.UID(); !ok || uid == 0 {
		t.Fatalf("result ref = %+v, want a uid", res.Ref)
	}
	if res.DraftConsumed {
		t.Error("a fresh message must not report a consumed draft")
	}
	if !containsString(res.Keywords, `\Seen`) {
		t.Errorf("keywords = %v, want \\Seen from Gmail's SENT label", res.Keywords)
	}
	if got := len(fx.Sent()); got != 1 {
		t.Fatalf("fixture accepted %d sends, want 1", got)
	}
}

// TestAdapterSendDraft sends an existing Gmail draft through drafts.send,
// consuming the draft handle and filing Sent.
func TestAdapterSendDraft(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)
	ctx := context.Background()

	ref, _, err := b.Append(ctx, "Drafts", raw("Draft", "<draft1@x>", "draft body"), []string{"$draft"}, nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := len(fx.DraftIDs()); got != 1 {
		t.Fatalf("fixture drafts = %d, want 1", got)
	}

	res, err := b.Send(ctx, mb.SendRequest{
		Raw:       raw("Draft", "<draft1@x>", "draft body"),
		MessageID: "<draft1@x>",
		Copies:    []mb.Copy{{MailboxID: "drafts", Ref: ref}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.DraftConsumed {
		t.Error("drafts.send must report the draft as consumed")
	}
	if got := len(fx.DraftIDs()); got != 0 {
		t.Errorf("fixture still holds %d drafts, want 0", got)
	}
	if got := len(fx.Sent()); got != 1 {
		t.Errorf("fixture accepted %d sends, want 1", got)
	}
}

// TestAdapterSendAmbiguousReconciles proves the reconciliation path: the
// fixture accepts the send but answers 500, and the adapter finds the
// accepted message by its Message-ID instead of reporting failure.
func TestAdapterSendAmbiguousReconciles(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierAmbiguousSend})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)

	res, err := b.Send(context.Background(), mb.SendRequest{
		Raw:       raw("Ambiguous", "<amb1@x>", "body"),
		MessageID: "<amb1@x>",
		From:      "me@example.test",
	})
	if err != nil {
		t.Fatalf("Send with reconciliation: %v", err)
	}
	if uid, ok := res.Ref.UID(); !ok || uid == 0 {
		t.Fatalf("reconciled result ref = %+v, want a uid", res.Ref)
	}
	if got := len(fx.Sent()); got != 1 {
		t.Fatalf("fixture accepted %d sends, want exactly 1 (no duplicate)", got)
	}
}

// TestAdapterSendAmbiguousWithoutMessageIDFails pins that a send the
// bridge cannot reconcile (no Message-ID) is reported as the transport
// error rather than silently assumed sent.
func TestAdapterSendAmbiguousWithoutMessageIDFails(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierAmbiguousSend})
	seedWriteLabels(fx)
	n := newFakeNative()
	b := newAdapter(t, fx, n)

	if _, err := b.Send(context.Background(), mb.SendRequest{
		Raw: raw("No id", "", "body"),
	}); err == nil {
		t.Fatal("Send without a Message-ID succeeded; want the ambiguous error")
	}
}

// TestAdapterAmbiguityClassification pins which provider failures trigger
// reconciliation: transport/5xx/throttle yes; a refusal, auth or 404 no.
func TestAdapterAmbiguityClassification(t *testing.T) {
	if !IsAmbiguous(&retryableError{err: context.DeadlineExceeded}) {
		t.Error("a transport failure must be ambiguous")
	}
	if !IsAmbiguous(mb.ErrThrottled) {
		t.Error("a throttle must be ambiguous")
	}
	if IsAmbiguous(&mb.RejectedError{Text: "no"}) {
		t.Error("a refusal is definite, not ambiguous")
	}
	if IsAmbiguous(mb.ErrAuth) {
		t.Error("an auth failure is definite, not ambiguous")
	}
	if IsAmbiguous(ErrNotFound) {
		t.Error("a 404 is definite, not ambiguous")
	}
}

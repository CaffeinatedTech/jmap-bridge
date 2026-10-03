package gmailapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

func TestClientProfileAndLabels(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{User: "me@example.test"})
	fx.SeedLabel(fixturegmail.LabelInbox, "Inbox", "system")
	fx.SeedLabel("Label_work", "Work", "user")
	c := newTestClient(t, fx)

	p, err := c.profile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.EmailAddress != "me@example.test" {
		t.Fatalf("profile address = %q", p.EmailAddress)
	}

	labels, err := c.listLabels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(labels.Labels) != 2 {
		t.Fatalf("labels = %d, want 2", len(labels.Labels))
	}
	var work *string
	for _, l := range labels.Labels {
		if l.Id == "Label_work" {
			name := l.Name
			work = &name
		}
	}
	if work == nil || *work != "Work" {
		t.Fatalf("Work label not decoded: %v", labels.Labels)
	}
}

func TestClientListMessagesPages(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	for i := 0; i < 3; i++ {
		fx.SeedMessage(fixturegmail.SeedMessage{
			ID:       fmt.Sprintf("m%d", i),
			LabelIDs: []string{fixturegmail.LabelInbox},
		})
	}
	c := newTestClient(t, fx)
	ctx := context.Background()

	first, err := c.listMessages(ctx, "", []string{fixturegmail.LabelInbox}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.NextPageToken == "" {
		t.Fatalf("first page = %d messages, next=%q", len(first.Messages), first.NextPageToken)
	}
	second, err := c.listMessages(ctx, "", []string{fixturegmail.LabelInbox}, 2, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || second.NextPageToken != "" {
		t.Fatalf("second page = %d messages, next=%q", len(second.Messages), second.NextPageToken)
	}
}

func TestClientMessageMetadataAndRaw(t *testing.T) {
	raw := []byte("From: a@x\r\nTo: b@x\r\nSubject: Hi\r\nMessage-ID: <abc@x>\r\n\r\nbody\r\n")
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID:       "m1",
		LabelIDs: []string{fixturegmail.LabelInbox, fixturegmail.LabelUnread, "STARRED"},
		Raw:      raw,
		Snippet:  "Hello&#39;s \u200b world",
		Headers: []fixturegmail.Header{
			{Name: "From", Value: "a@x"},
			{Name: "Subject", Value: "Hi"},
			{Name: "Message-ID", Value: "<abc@x>"},
		},
	})
	c := newTestClient(t, fx)
	ctx := context.Background()

	msg, err := c.getMessage(ctx, "m1", formatMetadata, "From", "Subject", "Message-ID")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Id != "m1" {
		t.Fatalf("id = %q", msg.Id)
	}
	headers := neutralHeaders(msg.Payload.Headers)
	if got := HeaderValue(headers, "subject"); got != "Hi" {
		t.Fatalf("Subject = %q", got)
	}
	kw := KeywordsFromLabels(msg.LabelIds)
	if kw[KeywordSeen] || !kw[KeywordFlagged] {
		t.Fatalf("keywords = %v, want seen=false flagged=true", kw)
	}
	if got := DecodeSnippet(msg.Snippet); got != "Hello's  world" {
		t.Fatalf("decoded snippet = %q", got)
	}

	rawMsg, err := c.getMessage(ctx, "m1", formatRaw)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawMsg.Raw)
	if err != nil {
		t.Fatalf("raw not base64url: %v", err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("raw round-trip = %q", decoded)
	}
}

func TestClientModifyAndHistory(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	fx.SeedMessage(fixturegmail.SeedMessage{ID: "m1", LabelIDs: []string{fixturegmail.LabelInbox, "UNREAD"}})
	c := newTestClient(t, fx)
	ctx := context.Background()
	start := fx.CurrentHistoryID()

	updated, err := c.modifyMessage(ctx, "m1", []string{"STARRED"}, []string{"UNREAD"})
	if err != nil {
		t.Fatal(err)
	}
	kw := KeywordsFromLabels(updated.LabelIds)
	if !kw[KeywordSeen] || !kw[KeywordFlagged] {
		t.Fatalf("after modify keywords = %v", kw)
	}

	hist, err := c.listHistory(ctx, start, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.History) == 0 {
		t.Fatal("history recorded no records for the modify")
	}
	sawLabel := false
	for _, rec := range hist.History {
		if len(rec.LabelsAdded) > 0 || len(rec.LabelsRemoved) > 0 {
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Fatalf("history has no label changes: %+v", hist.History)
	}
}

func TestClientQuotaBackoffRecovers(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierQuota, ThrottleFirst: 2})
	c := newTestClient(t, fx)

	if _, err := c.profile(context.Background()); err != nil {
		t.Fatalf("profile did not recover from throttles: %v", err)
	}
	clk := c.clock.(*fakeClock)
	if len(clk.sleeps) != 2 {
		t.Fatalf("backoff sleeps = %v, want 2 (Retry-After each)", clk.sleeps)
	}
	for _, d := range clk.sleeps {
		if d <= 0 {
			t.Fatalf("non-positive backoff sleep: %v", clk.sleeps)
		}
	}
}

func TestClientAuthFailure(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierUnauthorized})
	c := newTestClient(t, fx)
	if _, err := c.profile(context.Background()); !errors.Is(err, mailbackend.ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth", err)
	}
}

func TestClientHistoryExpired(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierHistoryExpired})
	c := newTestClient(t, fx)
	if _, err := c.listHistory(context.Background(), 1, ""); !IsNotFound(err) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestClientEventualVisibility(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierEventualVisibility})
	id := fx.SeedMessage(fixturegmail.SeedMessage{ID: "m1", LabelIDs: []string{fixturegmail.LabelInbox}})
	start := fx.CurrentHistoryID()
	fx.ForeignModifyLabels(id, []string{"STARRED"}, nil)
	c := newTestClient(t, fx)
	ctx := context.Background()

	hist, err := c.listHistory(ctx, start, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.History) != 0 {
		t.Fatalf("history surfaced before release: %+v", hist.History)
	}
	fx.ReleaseHistory()
	hist, err = c.listHistory(ctx, start, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.History) != 1 {
		t.Fatalf("history after release = %d, want 1", len(hist.History))
	}
}

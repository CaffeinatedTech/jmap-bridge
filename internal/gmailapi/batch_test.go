package gmailapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

func TestBatchAgainstFixture(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{User: "batch@example.test"})
	fx.SeedMessage(fixturegmail.SeedMessage{ID: "m1", LabelIDs: []string{fixturegmail.LabelInbox}})
	c := newTestClient(t, fx)

	res, err := c.Batch(context.Background(), []BatchItem{
		{Method: http.MethodGet, Path: "gmail/v1/users/me/profile", Cost: CostGetProfile},
		{Method: http.MethodGet, Path: "gmail/v1/users/me/messages/m1?format=metadata", Cost: CostMessagesGet},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	for i, r := range res {
		if r.Status != http.StatusOK || r.Err != nil {
			t.Fatalf("result %d = %d %v: %s", i, r.Status, r.Err, r.Body)
		}
	}
	if !strings.Contains(string(res[0].Body), "batch@example.test") {
		t.Fatalf("profile body = %s", res[0].Body)
	}
	if !strings.Contains(string(res[1].Body), "\"m1\"") {
		t.Fatalf("message body = %s", res[1].Body)
	}
}

func TestBatchPerItemThrottle(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierQuota, ThrottleFirst: 1000})
	c := newTestClient(t, fx)

	res, err := c.Batch(context.Background(), []BatchItem{
		{Method: http.MethodGet, Path: "gmail/v1/users/me/profile", Cost: CostGetProfile},
		{Method: http.MethodGet, Path: "gmail/v1/users/me/labels", Cost: CostLabelsList},
	})
	if err != nil {
		t.Fatalf("envelope should stay 200 under per-item throttling: %v", err)
	}
	for i, r := range res {
		if r.Status != http.StatusTooManyRequests {
			t.Fatalf("result %d status = %d, want 429", i, r.Status)
		}
		if !errors.Is(r.Err, mailbackend.ErrThrottled) {
			t.Fatalf("result %d err = %v, want ErrThrottled", i, r.Err)
		}
	}
}

// TestBatchCorrelatesOutOfOrder proves results bind to requests by Content-ID
// and not by response ordering: the server answers in reverse, echoing
// <response-item-N> ids.
func TestBatchCorrelatesOutOfOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for i := 2; i >= 0; i-- {
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", "application/http")
			h.Set("Content-ID", fmt.Sprintf("<response-item-%d>", i))
			part, _ := mw.CreatePart(h)
			_, _ = fmt.Fprintf(part, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"index\":%d}", i)
		}
		_ = mw.Close()
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c, err := New(context.Background(), Options{
		Endpoint:            srv.URL,
		HTTPClient:          srv.Client(),
		QuotaUnitsPerSecond: -1,
		Clock:               newFakeClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	res, err := c.Batch(context.Background(), []BatchItem{{Path: "a"}, {Path: "b"}, {Path: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range res {
		want := fmt.Sprintf(`{"index":%d}`, i)
		if got := string(res[i].Body); got != want {
			t.Errorf("result %d body = %s, want %s (correlated by index, not order)", i, got, want)
		}
	}
}

func TestBatchRejectsOversize(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	c := newTestClient(t, fx)
	items := make([]BatchItem, maxBatchItems+1)
	if _, err := c.Batch(context.Background(), items); err == nil {
		t.Fatal("oversize batch accepted")
	}
}

// TestBatchGetCapsFanOut pins the fix for Gmail's "Too many concurrent
// requests for user": one /batch must never carry more than maxBatchGet
// messages.get sub-requests, and every id must still be fetched.
func TestBatchGetCapsFanOut(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	ids := make([]string, 0, maxBatchGet*2+3)
	for i := 0; i < maxBatchGet*2+3; i++ {
		id := fmt.Sprintf("m%d", i)
		fx.SeedMessage(fixturegmail.SeedMessage{ID: id, LabelIDs: []string{fixturegmail.LabelInbox}})
		ids = append(ids, id)
	}
	c := newTestClient(t, fx)
	msgs, err := c.batchGetMessages(context.Background(), ids, formatMetadata, nil)
	if err != nil {
		t.Fatalf("batchGetMessages: %v", err)
	}
	if len(msgs) != len(ids) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(ids))
	}
	if got := fx.MaxBatchItems(); got > maxBatchGet {
		t.Fatalf("batch fan-out = %d, want <= %d", got, maxBatchGet)
	}
}

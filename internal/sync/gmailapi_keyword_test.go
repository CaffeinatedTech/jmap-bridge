package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// keywordCounter records every StoreKeywords call across the engine's
// sessions (which may be built concurrently).
type keywordCounter struct {
	mu    sync.Mutex
	calls [][2][]string
}

func (c *keywordCounter) record(add, remove []string) {
	c.mu.Lock()
	c.calls = append(c.calls, [2][]string{add, remove})
	c.mu.Unlock()
}

func (c *keywordCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// spyBackend wraps one provider session and forwards StoreKeywords to the
// shared counter. The embedded field is set once at construction, so there
// is no cross-goroutine mutation.
type spyBackend struct {
	mb.Backend
	c *keywordCounter
}

func (s *spyBackend) StoreKeywords(ctx context.Context, copies []mb.Copy, add, remove []string) error {
	s.c.record(add, remove)
	return s.Backend.StoreKeywords(ctx, copies, add, remove)
}

// TestGmailAPIRedundantKeywordDeltaIsNotSent is the regression for the M12
// live gate bug: after drafts.send consumed a draft, the caller's
// onSuccessUpdateEmail still carries `keywords/$draft: null`, but Gmail
// refuses to remove a label the message does not carry. The engine must
// compute effective deltas and issue no keyword command at all.
func TestGmailAPIRedundantKeywordDeltaIsNotSent(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	fx.SeedMessage(fixturegmail.SeedMessage{
		ID: "m1", ThreadID: "thr1", LabelIDs: []string{fixturegmail.LabelInbox},
		InternalDate: time.Now().UnixMilli(),
		Headers: []fixturegmail.Header{
			{Name: "Message-ID", Value: "<m1@x>"}, {Name: "Subject", Value: "Hi"},
		},
		Raw: raw("Hi", "<m1@x>", "body"),
	})
	counter := &keywordCounter{}
	env := newGmailAPIEnvWrapped(t, fx, func(b mb.Backend) mb.Backend {
		return &spyBackend{Backend: b, c: counter}
	})
	ctx := context.Background()
	waitUntil(t, 5*time.Second, "email ingested", func() bool { return len(env.emails(t)) == 1 })
	emailID := env.emails(t)[0].ID

	// The message is not a draft and is already $seen; both keyword deltas
	// are no-ops. The membership add is real, so the patch must still land.
	workID := env.mailboxID(t, "Work")
	if err := env.eng.ApplyEmailPatch(ctx, "gapi", emailID, jmapapi.EmailPatch{
		KeywordRemove: []string{"$draft"},
		KeywordAdd:    []string{"$seen"},
		MailboxAdd:    []string{workID},
	}); err != nil {
		t.Fatalf("ApplyEmailPatch: %v", err)
	}
	if n := counter.count(); n != 0 {
		t.Fatalf("StoreKeywords called %d times for a no-op keyword delta: %v", n, counter.calls)
	}
	labels, _ := fx.MessageLabels("m1")
	if !contains(labels, "Label_work") {
		t.Fatalf("membership add did not land: %v", labels)
	}
}

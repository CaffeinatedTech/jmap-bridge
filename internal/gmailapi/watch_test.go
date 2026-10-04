package gmailapi

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// realClockClient builds a client against the fixture with the real clock, so
// the adapter's renewal timer (which the fake clock does not drive) schedules
// against the fixture's short watch window.
func realClockClient(t *testing.T, fx *fixturegmail.Server) *Client {
	t.Helper()
	return newTestClient(t, fx, func(o *Options) { o.Clock = nil })
}

func TestWatchPubSubArmsAndRenews(t *testing.T) {
	// A ~1s window means the adapter re-arms at ~90% of it, so the renewal is
	// observable quickly (the M13 gate's expiry simulation).
	fx := fixturegmail.Start(t, fixturegmail.Options{WatchExpiration: 900 * time.Millisecond})
	b := NewBackend(Config{
		Account: "gmail", Client: realClockClient(t, fx), Native: newFakeNative(),
		WatchMode: "pubsub", PubSubTopic: "projects/p/topics/t",
	})
	if !b.Capabilities().Push {
		t.Fatal("pubsub mode must advertise Push")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notes, err := b.Watch(ctx, "INBOX")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if notes == nil {
		t.Fatal("pubsub mode must return a watch lifetime channel")
	}
	if got := fx.WatchTopic(); got != "projects/p/topics/t" {
		t.Fatalf("watch topic = %q", got)
	}

	// The watch must be renewed at least once before its window lapses.
	deadline := time.Now().Add(5 * time.Second)
	for fx.WatchCalls() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("watch not renewed: calls = %d", fx.WatchCalls())
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case _, ok := <-notes:
		if ok {
			t.Fatal("watch channel should close on shutdown")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not close on shutdown")
	}
	deadline = time.Now().Add(2 * time.Second)
	for !fx.Stopped() {
		if time.Now().After(deadline) {
			t.Fatal("users.stop was not called on shutdown")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestWatchPollModeDoesNotArm(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	b := NewBackend(Config{
		Account: "gmail", Client: realClockClient(t, fx), Native: newFakeNative(),
		WatchMode: "poll",
	})
	if b.Capabilities().Push {
		t.Fatal("poll mode must not advertise Push")
	}
	notes, err := b.Watch(context.Background(), "INBOX")
	if err != nil || notes != nil {
		t.Fatalf("poll mode Watch = (%v, %v), want (nil, nil)", notes, err)
	}
	if fx.WatchCalls() != 0 {
		t.Fatalf("poll mode called users.watch %d times", fx.WatchCalls())
	}
}

func TestRenewDelay(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	// 90% of a short window.
	if got := renewDelay(base.Add(10*time.Second), base); got != 9*time.Second {
		t.Errorf("short window = %v, want 9s", got)
	}
	// Capped at a day for a realistic 7-day window.
	if got := renewDelay(base.Add(7*24*time.Hour), base); got != watchRenewFallback {
		t.Errorf("long window = %v, want %v", got, watchRenewFallback)
	}
	// Absent expiration falls back to daily.
	if got := renewDelay(time.Time{}, base); got != watchRenewFallback {
		t.Errorf("absent expiration = %v, want %v", got, watchRenewFallback)
	}
	// Already expired retries soon.
	if got := renewDelay(base.Add(-time.Hour), base); got != time.Minute {
		t.Errorf("expired = %v, want 1m", got)
	}
}

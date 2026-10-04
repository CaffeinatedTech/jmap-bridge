package sync

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestHydrationQueueDropsOldest pins the queue's damage mitigation: at
// capacity a newer interactive hydration evicts the oldest pending one
// rather than growing without bound.
func TestHydrationQueueDropsOldest(t *testing.T) {
	q := newHydrationQueue(3)
	tickets := make([]*hydrateTicket, 0, 4)
	for i := 0; i < 3; i++ {
		tk := &hydrateTicket{done: make(chan error, 1)}
		if ev := q.push(tk); ev != nil {
			t.Fatalf("push %d evicted a ticket while still under capacity", i)
		}
		tickets = append(tickets, tk)
	}
	if q.len() != 3 {
		t.Fatalf("len = %d, want 3", q.len())
	}
	fresh := &hydrateTicket{done: make(chan error, 1)}
	ev := q.push(fresh)
	if ev != tickets[0] {
		t.Fatalf("evicted %p, want the oldest %p", ev, tickets[0])
	}
	if q.len() != 3 {
		t.Fatalf("len after eviction = %d, want 3", q.len())
	}
	got, ok := q.pop()
	if !ok || got != tickets[1] {
		t.Fatalf("pop = %v (ok=%v), want the second-oldest %p", got, ok, tickets[1])
	}
}

// TestSettleEvictionIsQuiet pins that an evicted ticket releases its
// flight with the quiet sentinel, wakes its caller, and does NOT record a
// provider-failure cooldown (eviction is not the provider's fault).
func TestSettleEvictionIsQuiet(t *testing.T) {
	e := &Engine{flights: map[string]*flight{}}
	f := &flight{done: make(chan struct{})}
	e.flights["a"] = f
	tk := &hydrateTicket{
		ids:     []string{"a"},
		flights: map[string]*flight{"a": f},
		done:    make(chan error, 1),
	}
	e.settleTicket(tk, evictedFails(tk))
	if !errors.Is(f.err, errHydrationEvicted) {
		t.Fatalf("flight err = %v, want errHydrationEvicted", f.err)
	}
	select {
	case err := <-tk.done:
		if !isHydrationSkip(err) {
			t.Fatalf("caller woken with %v, want a quiet skip", err)
		}
	default:
		t.Fatal("caller was not woken")
	}
	if len(e.hydrateFail) != 0 {
		t.Fatalf("eviction recorded a failure cooldown: %v", e.hydrateFail)
	}
}

// TestHydrationCooldownSparesFailedBody pins the negative cache: a body
// that just failed is served from cache for the cooldown window, then
// retried.
func TestHydrationCooldownSparesFailedBody(t *testing.T) {
	e := &Engine{hydrateFail: map[string]time.Time{}}
	e.noteFailure("a")
	if got := e.filterCooling([]string{"a", "b"}); len(got) != 1 || got[0] != "b" {
		t.Fatalf("filterCooling = %v, want [b]", got)
	}
	// Once the window lapses the id is retried and its entry pruned.
	e.hydrateFailMu.Lock()
	e.hydrateFail["a"] = time.Now().Add(-hydrationCooldown - time.Second)
	e.hydrateFailMu.Unlock()
	if got := e.filterCooling([]string{"a"}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("filterCooling after cooldown = %v, want [a]", got)
	}
	if _, ok := e.hydrateFail["a"]; ok {
		t.Fatal("expired cooldown entry was not pruned")
	}
}

// TestEnsureSkipsWhenAuthFailed pins the read-path auth gate: while an
// account needs re-consent, reads answer from cache instead of retrying
// the provider on every request.
func TestEnsureSkipsWhenAuthFailed(t *testing.T) {
	e := &Engine{cfg: Config{Account: "acct"}}
	e.authFailed.Store(true)
	if err := e.Ensure(context.Background(), "acct", nil, []string{"a"}); err != nil {
		t.Fatalf("Ensure = %v, want nil (cache answer)", err)
	}
}

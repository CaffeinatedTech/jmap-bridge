// Package push is the change-notification fan-out between the store
// and the SSE endpoint (PLAN §3, FR-J.8). It carries no data: a
// publish means "something changed for this account", and subscribers
// re-read type states themselves, which keeps bursts coalesced and
// makes dropped notifications harmless — clients recover via /changes
// (PLAN §10).
package push

import "sync"

// Hub fans account-scoped change signals out to subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{subs: map[string]map[chan struct{}]struct{}{}}
}

// Publish signals every subscriber of account without blocking: a
// subscriber that is slow misses signals, never the publish (they will
// see the resulting states on their next read anyway).
func (h *Hub) Publish(account string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[account] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe returns a signal channel and a cancel function. Signals
// are per account (FR-A.11: one account's changes never wake another
// account's stream).
func (h *Hub) Subscribe(account string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[account] == nil {
		h.subs[account] = map[chan struct{}]struct{}{}
	}
	h.subs[account][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set, ok := h.subs[account]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, account)
			}
		}
	}
}

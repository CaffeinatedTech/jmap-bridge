package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// defaultHydrationQueueCap bounds how many interactive body hydrations may
// be outstanding at once (FR-S.8). A client that scrolls faster than the
// provider can serve would otherwise pile up one blocked goroutine, one
// multi-minute fetch budget, and one contender for the single hydration
// session per keystroke — starving the header backfill and, on a provider
// refusal, hammering it. The queue is drop-oldest: the newest request wins
// and an evicted one answers from cache instead of failing the read.
const defaultHydrationQueueCap = 10

// hydrationCooldown spares a body that just failed a retry for a short
// window, so repeatedly opening an unfetchable message serves the cache
// instead of hammering the provider (FR-S.12). It is a mitigation, not a
// cache: the id is retried once the window passes.
const hydrationCooldown = 30 * time.Second

// hydrationFailCap bounds the failure-cooldown map so a large mailbox of
// unfetchable messages cannot grow it without limit.
const hydrationFailCap = 1024

// errHydrationEvicted reports that a queued interactive hydration was
// pushed out of the bounded queue by a newer request, or that its caller
// went away before it ran. Callers answer from the cache and treat it as a
// normal, quiet outcome.
var errHydrationEvicted = errors.New("sync: hydration evicted")

// isHydrationSkip reports whether err is a quiet, cache-serving outcome
// rather than a provider failure.
func isHydrationSkip(err error) bool { return errors.Is(err, errHydrationEvicted) }

// hydrateTicket is one queued interactive hydration: the ids a single
// Ensure asked for, the flights that own them, and the channel its caller
// waits on. Exactly one settle happens per ticket.
type hydrateTicket struct {
	ctx     context.Context
	ids     []string
	flights map[string]*flight
	done    chan error
}

// hydrationQueue is the bounded, drop-oldest FIFO of interactive
// hydrations. Producers may race; the sole worker is the consumer.
type hydrationQueue struct {
	mu    sync.Mutex
	items []*hydrateTicket
	cap   int
	wake  chan struct{}
}

func newHydrationQueue(capacity int) *hydrationQueue {
	if capacity <= 0 {
		capacity = defaultHydrationQueueCap
	}
	return &hydrationQueue{cap: capacity, wake: make(chan struct{}, 1)}
}

// push enqueues t at the tail, evicting and returning the oldest pending
// ticket when the queue is full.
func (q *hydrationQueue) push(t *hydrateTicket) *hydrateTicket {
	q.mu.Lock()
	var evicted *hydrateTicket
	if len(q.items) >= q.cap {
		evicted = q.items[0]
		q.items = q.items[1:]
	}
	q.items = append(q.items, t)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return evicted
}

func (q *hydrationQueue) pop() (*hydrateTicket, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	t := q.items[0]
	q.items = q.items[1:]
	return t, true
}

func (q *hydrationQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// hydrationWorkers starts the interactive hydration worker. Called from
// Run so it shares the engine context; a direct Ensure before Run
// processes its ticket inline instead (tests and non-running callers).
func (e *Engine) hydrationWorkers(ctx context.Context) {
	e.hydrateStarted.Store(true)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.hydrateQ.wake:
			}
			for {
				t, ok := e.hydrateQ.pop()
				if !ok {
					break
				}
				e.runHydrateTicket(t)
			}
		}
	}()
}

// enqueueInteractive queues a batch of body ids and blocks until they are
// hydrated, evicted, or the caller goes away (FR-S.8). Single-flight is
// preserved: ids another caller already owns are waited on, never
// refetched. An evicted or cooling ticket is a quiet cache answer, not an
// error.
func (e *Engine) enqueueInteractive(ctx context.Context, ids []string) error {
	ids = e.filterCooling(ids)
	if len(ids) == 0 {
		return nil
	}
	t := &hydrateTicket{ctx: ctx, done: make(chan error, 1), flights: map[string]*flight{}}
	var waiters []*flight
	for _, id := range ids {
		f, owner := e.claim(id)
		if owner {
			t.ids = append(t.ids, id)
			t.flights[id] = f
		} else {
			waiters = append(waiters, f)
		}
	}
	var first error
	if len(t.flights) > 0 {
		e.hydratePending.Add(1)
		if !e.hydrateStarted.Load() {
			e.runHydrateTicket(t)
		} else if ev := e.hydrateQ.push(t); ev != nil {
			// A newer read displaced the oldest pending one: settle it
			// quietly so its caller answers from cache.
			e.settleTicket(ev, evictedFails(ev))
		}
		select {
		case err := <-t.done:
			first = err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := e.waitFlights(ctx, waiters); err != nil && first == nil {
		first = err
	}
	if isHydrationSkip(first) {
		return nil
	}
	return first
}

// runHydrateTicket resolves and fetches one queued ticket's bodies, then
// settles its flights.
func (e *Engine) runHydrateTicket(t *hydrateTicket) {
	// An abandoned caller's request is dropped: with a bounded queue the
	// slot belongs to an active read, not a scrolled-past one.
	if t.ctx.Err() != nil {
		e.settleTicket(t, evictedFails(t))
		return
	}
	fails := map[string]error{}
	var want []string
	for _, id := range t.ids {
		done, err := e.st.IsHydrated(context.Background(), e.cfg.Account, id)
		if err == nil && done {
			continue // already cached: released with a nil error below
		}
		want = append(want, id)
	}
	if len(want) > 0 {
		locs, err := e.st.Locations(context.Background(), e.cfg.Account, want)
		if err != nil {
			for _, id := range want {
				fails[id] = err
			}
		} else {
			byFolder := map[string]map[uint32]string{} // folder → uid → email id
			for _, id := range want {
				loc, ok := locs[id]
				if !ok {
					fails[id] = fmt.Errorf("%w: %s has no backend location", errNotHydrated, id)
					continue
				}
				if byFolder[loc.Folder] == nil {
					byFolder[loc.Folder] = map[uint32]string{}
				}
				byFolder[loc.Folder][loc.UID] = id
			}
			e.fetchBodyFolders(t.ctx, byFolder, fails)
		}
	}
	e.settleTicket(t, fails)
}

// settleTicket releases a ticket's flights with their per-id errors,
// records failures for the cooldown, clears the pending gauge, and wakes
// the waiting caller.
func (e *Engine) settleTicket(t *hydrateTicket, fails map[string]error) {
	var first error
	for id, f := range t.flights {
		err := fails[id]
		e.release(id, f, err)
		if err == nil {
			continue
		}
		// A cancelled or evicted fetch is not the provider's fault: no
		// cooldown, and the caller treats it as a cache answer.
		if !isHydrationSkip(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			e.noteFailure(id)
		}
		if first == nil {
			first = err
		}
	}
	e.hydratePending.Add(-1)
	t.done <- first
}

func evictedFails(t *hydrateTicket) map[string]error {
	fails := make(map[string]error, len(t.flights))
	for id := range t.flights {
		fails[id] = errHydrationEvicted
	}
	return fails
}

// filterCooling drops ids inside their post-failure cooldown so a read
// answers from cache rather than re-fetching a message the provider just
// refused.
func (e *Engine) filterCooling(ids []string) []string {
	e.hydrateFailMu.Lock()
	defer e.hydrateFailMu.Unlock()
	if len(e.hydrateFail) == 0 {
		return ids
	}
	now := time.Now()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if when, ok := e.hydrateFail[id]; ok {
			if now.Sub(when) <= hydrationCooldown {
				continue
			}
			delete(e.hydrateFail, id)
		}
		out = append(out, id)
	}
	return out
}

// noteFailure records a failed hydration for the cooldown window. If the
// map is at its cap with entries all still live, the new failure is not
// recorded: a retry is less bad than unbounded growth.
func (e *Engine) noteFailure(id string) {
	e.hydrateFailMu.Lock()
	defer e.hydrateFailMu.Unlock()
	if e.hydrateFail == nil {
		e.hydrateFail = map[string]time.Time{}
	}
	if _, ok := e.hydrateFail[id]; !ok && len(e.hydrateFail) >= hydrationFailCap {
		now := time.Now()
		for k, when := range e.hydrateFail {
			if now.Sub(when) > hydrationCooldown {
				delete(e.hydrateFail, k)
			}
		}
		if len(e.hydrateFail) >= hydrationFailCap {
			return
		}
	}
	e.hydrateFail[id] = time.Now()
}

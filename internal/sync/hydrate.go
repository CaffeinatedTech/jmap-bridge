package sync

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// errNotHydrated means the backend fetch for a body did not produce
// data; the read path serves what the cache has.
var errNotHydrated = errors.New("sync: body unavailable")

// hydrationFetchTimeout bounds one hydration download. It is deliberately
// not the caller's request context: an interactive read that gives up must
// not cancel a fetch a later re-read (or another waiter) can still use.
// Under a cold API backfill a body fetch can wait behind the shared quota
// pacer; without this the client's timeout would abort the download and
// nothing would ever cache, so every open would fetch again and fail.
const hydrationFetchTimeout = 2 * time.Minute

// flight is one in-progress hydration of one email (FR-S.8: concurrent
// requests for the same body wait on a single IMAP fetch).
type flight struct {
	done chan struct{}
	err  error
}

// Ensure is the store's hook (FR-M.4/FR-S.8): fill previews for
// previewIDs and bodies for bodyIDs before the read answers. Interactive
// bodies are queued on a bounded, drop-oldest lane so a client that
// scrolls faster than the provider can serve cannot queue unbounded work;
// an evicted body (or one in its post-failure cooldown) answers from
// cache. A caller that goes away stops waiting, and a queued ticket whose
// caller already left is dropped rather than fetched — the next read
// re-requests it.
func (e *Engine) Ensure(ctx context.Context, account string, previewIDs, bodyIDs []string) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: ensure for foreign account %q", account)
	}
	// An account that needs re-consent serves the cache until a pass
	// clears the flag: retrying the provider on every read would only
	// hammer it (FR-D.4, FR-A.7).
	if e.authFailed.Load() {
		return nil
	}
	var firstErr error
	if len(previewIDs) > 0 {
		if err := e.fetchPreviews(ctx, previewIDs); err != nil {
			if isAuthFailure(err) {
				e.authFailed.Store(true)
			}
			firstErr = err
		}
	}
	if len(bodyIDs) > 0 {
		err := e.enqueueInteractive(ctx, bodyIDs)
		if err != nil && !errors.Is(err, context.Canceled) {
			if isAuthFailure(err) {
				e.authFailed.Store(true)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// fetchBodyFolders fills bodies for a set of ids grouped by folder with
// one round trip per folder chunk instead of one per message (NFR-1's
// ≥ 15 msg/s floor assumes it: a per-message select/fetch/unselect makes
// folder-size server bookkeeping the bottleneck). It records per-id
// failures in fails. ctx is consulted between folders only — the fetch
// itself runs on its own bounded context so a caller that walks away does
// not waste a completed download.
func (e *Engine) fetchBodyFolders(ctx context.Context, byFolder map[string]map[uint32]string, fails map[string]error) {
	for folder, uids := range byFolder {
		if ctx.Err() != nil {
			for _, id := range uids {
				fails[id] = ctx.Err()
			}
			return
		}
		// Hydrate on the dedicated session (FR-X.6): a sync pass holds
		// the work session for a whole backfill, so reading through it
		// would starve this request.
		refs := make([]mb.Ref, 0, len(uids))
		for uid := range uids {
			refs = append(refs, mb.NewRef(folder, 0, uid))
		}
		if e.cfg.LogHydration {
			e.log.Info("sync: hydrating", "account", e.cfg.Account, "folder", folder, "messages", len(uids))
		}
		opCtx, cancel := context.WithTimeout(context.Background(), hydrationFetchTimeout)
		var bodies map[mb.Ref][]byte
		err := e.rd.withBackend(opCtx, func(b mb.Backend) error {
			var ferr error
			bodies, ferr = b.FetchRawBatch(opCtx, folder, refs)
			return ferr
		})
		cancel()
		if err != nil {
			if e.cfg.LogHydration {
				e.log.Info("sync: hydration fetch failed",
					"account", e.cfg.Account, "folder", folder, "messages", len(uids), "err", err)
			}
			for _, id := range uids {
				fails[id] = err
			}
			continue
		}
		e.hydrateFetches.Add(int64(len(bodies)))
		for uid, id := range uids {
			raw, ok := bodies[mb.NewRef(folder, 0, uid)]
			if !ok {
				fails[id] = fmt.Errorf("%w: %s not in fetch response", errNotHydrated, id)
				continue
			}
			if err := e.storeBody(id, raw); err != nil {
				fails[id] = err
			}
		}
	}
}

// hydrationYieldCap bounds how long a background backfill defers to a
// pending interactive fetch, so a stuck hydration cannot stall sync.
const hydrationYieldCap = 15 * time.Second

// yieldToHydration pauses a background backfill batch while an interactive
// body fetch is pending (FR-S.8's read responsiveness). API mode shares one
// quota pacer between the work and hydration sessions, so without this a
// cold backfill can keep the pacer busy and starve the body a client is
// waiting on. It returns false only when ctx has ended.
func (e *Engine) yieldToHydration(ctx context.Context) bool {
	if e.hydratePending.Load() == 0 {
		return ctx.Err() == nil
	}
	deadline := time.Now().Add(hydrationYieldCap)
	for e.hydratePending.Load() > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ctx.Err() == nil
}

// waitFlights blocks until each flight settles (or the caller's context
// ends) and surfaces the first failure.
func (e *Engine) waitFlights(ctx context.Context, flights []*flight) error {
	var first error
	for _, f := range flights {
		select {
		case <-f.done:
			if f.err != nil && first == nil {
				first = f.err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return first
}

// storeBody parses one fetched message and commits it — the tail both
// the single and the batched hydration path share. An unparseable or
// partless message is cached as hydrated-but-empty rather than failed:
// the bytes were fetched, and re-fetching the same bytes on every read
// would be a permanent retry loop and a warning per list view (FR-S.8,
// FR-X.6).
//
// The parse error text is deliberately NOT logged: go-message embeds the
// offending message bytes in its errors ("malformed MIME header key:
// <bytes>"), and FR-D.12 forbids message bodies in logs. The envelope id
// is enough to find the raw message in the blob store.
func (e *Engine) storeBody(id string, raw []byte) error {
	res, perr := convert.ParseBody(raw)
	if perr != nil {
		e.log.Debug("sync: body parsed with warnings", "email", id)
	}
	if len(res.Values) == 0 && len(res.Attachments) == 0 {
		e.log.Warn("sync: body has no readable parts; caching empty", "email", id)
	}
	if err := e.st.PutHydrated(context.Background(), e.cfg.Account, id, res); err != nil {
		return err
	}
	if e.cfg.LogHydration {
		e.log.Info("sync: hydrated", "account", e.cfg.Account, "email", id,
			"bytes", len(raw), "values", len(res.Values), "attachments", len(res.Attachments))
	}
	e.log.Debug("sync: hydrated", "email", id)
	return nil
}

// claim starts (or joins) the single-flight for one email id. The
// second result reports ownership: only the owner performs the fetch.
func (e *Engine) claim(id string) (*flight, bool) {
	e.flightMu.Lock()
	defer e.flightMu.Unlock()
	if f, ok := e.flights[id]; ok {
		return f, false
	}
	f := &flight{done: make(chan struct{})}
	e.flights[id] = f
	return f, true
}

func (e *Engine) release(id string, f *flight, err error) {
	e.flightMu.Lock()
	delete(e.flights, id)
	e.flightMu.Unlock()
	f.err = err
	close(f.done)
}

// hydrate fetches and stores one message body, single-flight per id.
// Waiters stop waiting when their own context ends; the owner's fetch
// runs under the engine-independent IMAP context so cancelling an HTTP
// request does not waste a completed download.
func (e *Engine) hydrate(ctx context.Context, id string) error {
	f, owner := e.claim(id)
	if !owner {
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := e.doHydrate(id)
	e.release(id, f, err)
	return err
}

// doHydrate is the owner's fetch: get the bytes, parse them, commit
// them (FR-S.8). Already-hydrated ids short-circuit: the search
// backfill lane re-enqueues candidates on every text query, so without
// this check a completed body would be re-downloaded on each repeat.
func (e *Engine) doHydrate(id string) error {
	if done, err := e.st.IsHydrated(context.Background(), e.cfg.Account, id); err == nil && done {
		return nil
	}
	raw, err := e.fetchRawBody(id)
	if err != nil {
		return err
	}
	return e.storeBody(id, raw)
}

// fetchRawBody downloads one message's bytes over the work connection:
// the fetch half of hydration (FR-S.8), and how a submission gets the
// raw message of a draft the bridge did not build itself (FR-M.15). An
// id the cache cannot yet address on the server yields
// [errNotHydrated]; the fetch runs under the engine's own context, so a
// caller that walks away does not waste a completed download.
func (e *Engine) fetchRawBody(id string) ([]byte, error) {
	locs, err := e.st.Locations(context.Background(), e.cfg.Account, []string{id})
	if err != nil {
		return nil, err
	}
	loc, ok := locs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s has no backend location", errNotHydrated, id)
	}
	e.hydrateFetches.Add(1)
	opCtx, cancel := context.WithTimeout(context.Background(), hydrationFetchTimeout)
	defer cancel()
	var raw []byte
	err = e.rd.withBackend(opCtx, func(b mb.Backend) error {
		var ferr error
		raw, ferr = b.FetchRaw(opCtx, mb.NewRef(loc.Folder, 0, loc.UID))
		return ferr
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// fetchPreviews fills missing previews for a batch of ids, grouped by
// folder so one EXAMINE serves each group (PLAN §5).
func (e *Engine) fetchPreviews(ctx context.Context, ids []string) error {
	locs, err := e.st.Locations(ctx, e.cfg.Account, ids)
	if err != nil {
		return err
	}
	refs := make([]mb.Ref, 0, len(ids))
	byRef := map[mb.Ref]string{}
	for _, id := range ids {
		if loc, ok := locs[id]; ok {
			r := mb.NewRef(loc.Folder, 0, loc.UID)
			refs = append(refs, r)
			byRef[r] = id
		}
	}
	if len(refs) == 0 {
		return nil
	}
	// Previews run on the dedicated session too (FR-X.6), so listing a
	// folder never waits out a backfill on the work session.
	return e.rd.withBackend(ctx, func(b mb.Backend) error {
		previews, err := b.FetchPreviews(ctx, refs)
		if err != nil {
			return err
		}
		byID := map[string]string{}
		for r, p := range previews {
			if p != "" {
				byID[byRef[r]] = p
			}
		}
		return e.st.SetPreviews(ctx, e.cfg.Account, byID)
	})
}

// prefetch enqueues background hydration for messages inside the
// prefetch window (FR-S.9): bounded by search.concurrency, rate
// limited with jitter, skipped entirely when the window is 0, and
// never blocking a pass — full slots just retry on the next pass.
func (e *Engine) prefetch(ctx context.Context) {
	if e.cfg.PrefetchWindow <= 0 {
		return
	}
	ids, err := e.st.UnhydratedRecent(ctx, e.cfg.Account, e.cfg.PrefetchWindow, e.cfg.BatchSize)
	if err != nil {
		e.log.Warn("sync: prefetch scan failed", "err", err)
		return
	}
	for _, id := range ids {
		select {
		case e.prefetchSem <- struct{}{}:
		default:
			return // workers busy: try again next pass
		}
		go func(id string) {
			defer func() { <-e.prefetchSem }()
			// Jitter keeps a cold start from hammering the provider
			// (FR-S.12) and yields the work connection to reads.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(rand.Intn(200)) * time.Millisecond): //nolint:gosec // jitter
			}
			if err := e.hydrate(ctx, id); err != nil && !errors.Is(err, errNotConnected) {
				e.log.Debug("sync: prefetch skipped", "email", id, "err", err)
			}
		}(id)
	}
}

// searchBackfill is the store's search-driven backfill hook (FR-X.5):
// a text query just answered with header matches and these ids are the
// unhydrated candidates it found in scope. The ids land in the
// backfill lane — non-blocking, bounded — and workers hydrate them
// one fetch at a time; each finished body re-enters the search index
// and bumps queryState, so the client's next query (or SSE nudge)
// picks up the late matches. An overflowing lane drops ids, never
// blocks a response: the next query re-enqueues whatever is still
// unhydrated, which is also what makes backfill resumable across
// restarts (FR-X.6).
func (e *Engine) searchBackfill(account string, ids []string) {
	if account != e.cfg.Account {
		return
	}
	for _, id := range ids {
		select {
		case e.backfill <- id:
		default:
			return
		}
	}
}

// backfillWorkers starts the bounded hydration lane; called from Run
// so the workers share the engine context and stop with it. Interactive
// Email/get hydrations never enter this lane — they hydrate inline —
// and both lanes serialise on the work connection, so backfill can
// delay a read by at most one in-flight fetch (FR-X.6).
func (e *Engine) backfillWorkers(ctx context.Context) {
	for range e.cfg.Concurrency {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-e.backfill:
					// Same jitter as prefetch: a search over a large
					// unhydrated folder must not hammer the provider
					// (FR-S.12).
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(rand.Intn(200)) * time.Millisecond): //nolint:gosec // jitter
					}
					if err := e.hydrate(ctx, id); err != nil &&
						!errors.Is(err, errNotConnected) && !errors.Is(err, context.Canceled) {
						e.log.Debug("sync: backfill hydrate failed", "email", id, "err", err)
					}
				}
			}
		}()
	}
}

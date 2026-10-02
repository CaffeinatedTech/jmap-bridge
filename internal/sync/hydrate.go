package sync

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
)

// errNotHydrated means the backend fetch for a body did not produce
// data; the read path serves what the cache has.
var errNotHydrated = errors.New("sync: body unavailable")

// flight is one in-progress hydration of one email (FR-S.8: concurrent
// requests for the same body wait on a single IMAP fetch).
type flight struct {
	done chan struct{}
	err  error
}

// Ensure is the store's hook (FR-M.4/FR-S.8): fill previews for
// previewIDs and bodies for bodyIDs before the read answers. It blocks
// until the bodies it was asked for are cached (or failed), and a
// caller that goes away mid-wait simply stops waiting — the fetch
// itself runs on the engine's context so another waiter still wins.
func (e *Engine) Ensure(ctx context.Context, account string, previewIDs, bodyIDs []string) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: ensure for foreign account %q", account)
	}
	var firstErr error
	if len(previewIDs) > 0 {
		if err := e.fetchPreviews(ctx, previewIDs); err != nil {
			firstErr = err
		}
	}
	if len(bodyIDs) > 0 {
		if err := e.hydrateBatch(ctx, bodyIDs); err != nil && !errors.Is(err, context.Canceled) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// hydrateBatch fills a set of bodies with one IMAP round trip per
// folder chunk instead of one per message (NFR-1's ≥ 15 msg/s floor
// assumes it: a per-message select/fetch/unselect makes folder-size
// server bookkeeping the bottleneck). Single-flight per id is kept:
// ids another caller already owns are left to that owner, and ids the
// store already shows hydrated are answered locally.
func (e *Engine) hydrateBatch(ctx context.Context, ids []string) error {
	type owned struct {
		id string
		f  *flight
	}
	var mine []owned
	var waiters []*flight
	for _, id := range ids {
		f, owner := e.claim(id)
		if owner {
			mine = append(mine, owned{id, f})
		} else {
			// Another caller owns this fetch: wait for it, exactly as
			// the single-id path does, so this read never answers
			// before the body it asked for is cached (FR-S.8).
			waiters = append(waiters, f)
		}
	}
	if len(mine) == 0 {
		return e.waitFlights(ctx, waiters)
	}
	fails := make(map[string]error, len(mine))
	defer func() {
		for _, o := range mine {
			e.release(o.id, o.f, fails[o.id])
		}
	}()
	var want []string
	for _, o := range mine {
		done, err := e.st.IsHydrated(context.Background(), e.cfg.Account, o.id)
		if err == nil && done {
			continue // done: released with a nil error below
		}
		want = append(want, o.id)
	}
	locs, err := e.st.Locations(context.Background(), e.cfg.Account, want)
	if err != nil {
		for _, o := range mine {
			if !wanted(want, o.id) {
				continue // already hydrated: stays a nil error
			}
			fails[o.id] = err
		}
		return err
	}
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
	for folder, uids := range byFolder {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Hydrate on the dedicated session (FR-X.6): a sync pass holds
		// the work connection for a whole backfill, so reading through
		// it would starve this request.
		var bodies map[uint32][]byte
		err := e.rd.withConn(ctx, func(conn *imapdrv.Conn) error {
			var ferr error
			bodies, ferr = conn.FetchBodies(ctx, folder, uidKeys(uids))
			return ferr
		})
		if err != nil {
			for _, id := range uids {
				fails[id] = err
			}
			continue
		}
		e.hydrateFetches.Add(int64(len(bodies)))
		for uid, id := range uids {
			raw, ok := bodies[uid]
			if !ok {
				fails[id] = fmt.Errorf("%w: %s not in fetch response", errNotHydrated, id)
				continue
			}
			if err := e.storeBody(id, raw); err != nil {
				fails[id] = err
			}
		}
	}
	return firstErrOf(fails)
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

func wanted(want []string, id string) bool {
	for _, w := range want {
		if w == id {
			return true
		}
	}
	return false
}

func uidKeys(m map[uint32]string) []uint32 {
	out := make([]uint32, 0, len(m))
	for uid := range m {
		out = append(out, uid)
	}
	return out
}

func firstErrOf(fails map[string]error) error {
	for _, err := range fails {
		if err != nil {
			return err
		}
	}
	return nil
}

// storeBody parses one fetched message and commits it — the tail both
// the single and the batched hydration path share. An unparseable or
// partless message is cached as hydrated-but-empty rather than failed:
// the bytes were fetched, and re-fetching the same bytes on every read
// would be a permanent retry loop and a warning per list view (FR-S.8,
// FR-X.6). The reason is logged once, at warn.
func (e *Engine) storeBody(id string, raw []byte) error {
	res, perr := convert.ParseBody(raw)
	if perr != nil {
		e.log.Debug("sync: body parsed with warnings", "email", id, "err", perr)
	}
	if len(res.Values) == 0 && len(res.Attachments) == 0 {
		e.log.Warn("sync: body has no readable parts; caching empty",
			"email", id, "err", perr)
	}
	if err := e.st.PutHydrated(context.Background(), e.cfg.Account, id, res); err != nil {
		return err
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
	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var raw []byte
	err = e.rd.withConn(opCtx, func(conn *imapdrv.Conn) error {
		if _, err := conn.Examine(opCtx, loc.Folder, nil); err != nil {
			return err
		}
		if raw, err = conn.FetchBody(opCtx, loc.UID); err != nil {
			return err
		}
		return conn.Unselect(opCtx)
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
	byFolder := map[string][]uint32{}
	for _, id := range ids {
		if loc, ok := locs[id]; ok {
			byFolder[loc.Folder] = append(byFolder[loc.Folder], loc.UID)
		}
	}
	if len(byFolder) == 0 {
		return nil
	}
	// Previews run on the dedicated session too (FR-X.6), so listing a
	// folder never waits out a backfill on the work connection.
	return e.rd.withConn(ctx, func(conn *imapdrv.Conn) error {
		defer func() { _ = conn.Unselect(context.Background()) }()
		for folder, uids := range byFolder {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := conn.Examine(ctx, folder, nil); err != nil {
				return err
			}
			previews, err := conn.FetchPreviews(ctx, uids)
			if err != nil {
				return err
			}
			byID := map[string]string{}
			for _, id := range ids {
				if loc, ok := locs[id]; ok && loc.Folder == folder {
					if p, ok := previews[loc.UID]; ok && p != "" {
						byID[id] = p
					}
				}
			}
			if err := e.st.SetPreviews(ctx, e.cfg.Account, byID); err != nil {
				return err
			}
		}
		return nil
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

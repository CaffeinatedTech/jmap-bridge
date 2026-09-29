package sync

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
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
	for _, id := range bodyIDs {
		if err := e.hydrate(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
// them (FR-S.8).
func (e *Engine) doHydrate(id string) error {
	raw, err := e.fetchRawBody(id)
	if err != nil {
		return err
	}
	res, soft := convert.ParseBody(raw)
	if soft != nil {
		e.log.Debug("sync: body parsed with warnings", "email", id, "err", soft)
	}
	if len(res.Values) == 0 && len(res.Attachments) == 0 {
		return fmt.Errorf("%w: %s parsed empty", errNotHydrated, id)
	}
	if err := e.st.PutHydrated(context.Background(), e.cfg.Account, id, res); err != nil {
		return err
	}
	e.log.Debug("sync: hydrated", "email", id)
	return nil
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
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work == nil {
		return nil, errNotConnected
	}
	e.hydrateFetches.Add(1)
	opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := e.work.Examine(opCtx, loc.Folder, nil); err != nil {
		return nil, err
	}
	raw, err := e.work.FetchBody(opCtx, loc.UID)
	if err != nil {
		return nil, err
	}
	_ = e.work.Unselect(opCtx)
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
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work == nil {
		return errNotConnected
	}
	defer func() { _ = e.work.Unselect(context.Background()) }()
	for folder, uids := range byFolder {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := e.work.Examine(ctx, folder, nil); err != nil {
			return err
		}
		previews, err := e.work.FetchPreviews(ctx, uids)
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

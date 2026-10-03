package sync

import (
	"context"
	"math/rand"
	"time"

	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// idleLoop owns the watch session: connect, watch the inbox, and on any
// change wake a pass for that folder — the ≤ 2 s foreign-change path
// (FR-S.4, FR-S.7). It reconnects with exponential backoff on its own so
// a dead watch session never takes the work session down with it.
func (e *Engine) idleLoop(ctx context.Context) {
	backoffDur := time.Second
	for ctx.Err() == nil {
		e.countReconnect("idle")
		backend := e.cfg.NewBackend()
		if err := backend.Connect(ctx); err != nil {
			e.log.Warn("sync: idle connect failed", "err", err)
			if !sleepCtx(ctx, jitter(backoffDur)) {
				return
			}
			backoffDur = grow(backoffDur)
			continue
		}
		backoffDur = time.Second
		err := e.idleOnce(ctx, backend)
		_ = backend.Close()
		if ctx.Err() != nil {
			return
		}
		e.log.Warn("sync: idle session ended", "err", err)
		if !sleepCtx(ctx, jitter(backoffDur)) {
			return
		}
		backoffDur = grow(backoffDur)
	}
}

// idleOnce watches the watched folder until the session dies or ctx
// ends. Notifications do not end the watch; they queue a pass and the
// session keeps watching, so a burst of changes costs one wake, not one
// per change (FR-S.7). A nil channel means the backend has no push and
// the engine's poll ticker is the only path.
func (e *Engine) idleOnce(ctx context.Context, backend mb.Backend) error {
	folder := e.currentIdleFolder()
	notes, err := backend.Watch(ctx, folder)
	if err != nil {
		return err
	}
	if notes == nil {
		return nil // push unsupported: poll fallback
	}
	e.idleWatching.Store(true)
	defer e.idleWatching.Store(false)
	for {
		select {
		case _, ok := <-notes:
			if !ok {
				return nil
			}
			// Another client touched this folder: sync now (FR-S.7's
			// latency budget starts here, not at the next poll).
			e.requestPass(folder)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func grow(d time.Duration) time.Duration {
	d *= 2
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(d-half)+1)) //nolint:gosec // jitter
}

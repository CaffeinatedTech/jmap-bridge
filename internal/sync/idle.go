package sync

import (
	"context"
	"math/rand"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
)

// idleLoop owns the idle connection: dial, select the inbox, IDLE, and
// on any change wake a pass for that folder — the ≤ 2 s foreign-change
// path (FR-S.4, FR-S.7). It reconnects with exponential backoff on its
// own so a dead idle socket never takes the work connection down with
// it.
func (e *Engine) idleLoop(ctx context.Context) {
	backoffDur := time.Second
	for ctx.Err() == nil {
		conn, err := imapdrv.Dial(ctx, e.cfg.IMAP)
		if err != nil {
			e.log.Warn("sync: idle dial failed", "err", err)
			if !sleepCtx(ctx, jitter(backoffDur)) {
				return
			}
			backoffDur = grow(backoffDur)
			continue
		}
		backoffDur = time.Second
		err = e.idleOnce(ctx, conn)
		_ = conn.Close()
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

// idleOnce selects the watched folder and idles until the session dies
// or ctx ends. Notifications do not end IDLE (RFC 2177 lets the
// session continue): they queue a pass and the connection keeps
// watching, so a burst of changes costs one DONE, not one per change.
func (e *Engine) idleOnce(ctx context.Context, conn *imapdrv.Conn) error {
	folder := e.currentIdleFolder()
	if _, err := conn.Examine(ctx, folder, nil); err != nil {
		return err
	}
	session, err := conn.StartIdle()
	if err != nil {
		return err
	}
	e.idleWatching.Store(true)
	defer e.idleWatching.Store(false)
	defer func() { _ = conn.EndIdle(session) }()
	for {
		select {
		case <-conn.Notes():
			// Another client touched this folder: sync now (FR-S.7's
			// latency budget starts here, not at the next poll).
			e.requestPass(folder)
		case err := <-session.IdleDone():
			return err
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

package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
)

// reader owns one account's dedicated hydration session: a second IMAP
// connection, behind its own mutex, never shared with the sync pass. A
// pass holds the work connection for a whole backfill, and without a
// separate session every Email/get that needed a preview or body would
// queue behind it and time out (FR-S.8, FR-X.6, NFR-1). It mirrors
// writer: one connection, dialed lazily, ping-checked, dropped on a
// transport failure.
type reader struct {
	mu   sync.Mutex
	cfg  imapdrv.Config
	log  *slog.Logger
	conn *imapdrv.Conn
}

func newReader(cfg imapdrv.Config, log *slog.Logger) *reader {
	return &reader{cfg: cfg, log: log}
}

func (r *reader) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked()
}

func (r *reader) dropLocked() {
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
}

// withConn runs fn on a healthy hydration connection. Reads are
// idempotent, so a transport failure drops the socket and retries once
// on a fresh one; a server refusal is returned as is. The caller's
// context bounds each attempt, so a request that goes away stops waiting
// rather than holding the session.
func (r *reader) withConn(ctx context.Context, fn func(*imapdrv.Conn) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempt := 0; ; attempt++ {
		conn, err := r.ensureLocked(ctx)
		if err != nil {
			return err
		}
		err = fn(conn)
		if err == nil || imapdrv.ServerRejected(err) {
			return err
		}
		// Transport failure: drop the socket and, if the request is
		// still live, try once more on a fresh one.
		r.dropLocked()
		if attempt >= 1 || ctx.Err() != nil {
			return err
		}
	}
}

func (r *reader) ensureLocked(ctx context.Context) (*imapdrv.Conn, error) {
	if r.conn != nil {
		if err := r.conn.Ping(ctx); err == nil {
			return r.conn, nil
		} else {
			r.log.Debug("sync: hydration session stale", "err", err)
		}
		r.dropLocked()
	}
	conn, err := imapdrv.Dial(ctx, r.cfg)
	if err != nil {
		return nil, fmt.Errorf("sync: hydration connect: %w", err)
	}
	r.conn = conn
	r.log.Info("sync: hydration session up",
		"tier", conn.Tier().String(), "compressed", conn.Compressed())
	return conn, nil
}

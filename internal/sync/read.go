package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// reader owns one account's dedicated hydration session: a second
// backend session, behind its own mutex, never shared with the sync
// pass. A pass holds the work session for a whole backfill, and without
// a separate session every Email/get that needed a preview or body would
// queue behind it and time out (FR-S.8, FR-X.6, NFR-1). It mirrors
// writer: one session, dialed lazily, ping-checked, dropped on a
// transport failure.
type reader struct {
	mu         sync.Mutex
	newBackend func() mb.Backend
	log        *slog.Logger
	backend    mb.Backend
	count      reconnectCounter // nil disables the FR-D.6 reconnect count
}

func newReader(newBackend func() mb.Backend, log *slog.Logger, count reconnectCounter) *reader {
	return &reader{newBackend: newBackend, log: log, count: count}
}

func (r *reader) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked()
}

func (r *reader) dropLocked() {
	if r.backend != nil {
		_ = r.backend.Close()
		r.backend = nil
	}
}

// withBackend runs fn on a healthy hydration session. Reads are
// idempotent, so a transport failure drops the socket and retries once
// on a fresh one; a server refusal or an authentication failure is
// returned as is — redialing cannot mint new credentials, and retrying a
// 401 on every read is what turns a dead token into a request storm
// (FR-A.7). The caller's context bounds each attempt, so a request that
// goes away stops waiting rather than holding the session.
func (r *reader) withBackend(ctx context.Context, fn func(mb.Backend) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempt := 0; ; attempt++ {
		backend, err := r.ensureLocked(ctx)
		if err != nil {
			return err
		}
		err = fn(backend)
		if err == nil || mb.IsRejected(err) || isAuthFailure(err) {
			return err
		}
		r.dropLocked()
		if attempt >= 1 || ctx.Err() != nil {
			return err
		}
	}
}

func (r *reader) ensureLocked(ctx context.Context) (mb.Backend, error) {
	if r.backend != nil {
		if err := r.backend.Ping(ctx); err == nil {
			return r.backend, nil
		} else {
			r.log.Debug("sync: hydration session stale", "err", err)
		}
		r.dropLocked()
	}
	if r.count != nil {
		r.count("hydrate")
	}
	backend := r.newBackend()
	if err := backend.Connect(ctx); err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("sync: hydration connect: %w", err)
	}
	r.backend = backend
	r.log.Info("sync: hydration session up", "backend", backend.Kind())
	return backend, nil
}

// Package store is the SQLite-backed cache and the M1 implementation of
// [jmapapi.Store] (PLAN §3, §4). It owns ids, state strings, /changes
// replay, mailbox counts and thread derivation; the sync engine feeds it
// IMAP observations through the mutation methods in emails.go, and the
// HTTP layer reads through the jmapapi.Store seam. Every mutation takes
// the next per-account modseq, commits before it publishes a change, and
// is account-scoped (FR-A.11).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	_ "modernc.org/sqlite" // no-cgo driver (NFR-10), WAL via pragmas below
)

// Store is the durable cache. One Store per process, shared by the sync
// engine and the HTTP handlers (PLAN §3 process model: single SQLite
// database, single writer).
type Store struct {
	db    *sql.DB
	log   *slog.Logger
	data  string
	blobs *BlobStore
	// publish signals "something changed for this account" after a
	// commit (PLAN §10). It runs outside the transaction: delivery is
	// best-effort and clients recover via /changes, so a dropped
	// notification is a latency event. The SSE layer re-reads type
	// states on the signal and emits only the ones that moved.
	publish func(account string)
	// Ensure lets the sync engine fill summary data before a read
	// answers: previews for previewIDs, full bodies for bodyIDs
	// (FR-S.8 single-flight lives on that side). Nil serves from cache.
	Ensure func(ctx context.Context, account string, previewIDs, bodyIDs []string) error
}

// Options configures Open.
type Options struct {
	// DataDir holds bridge.db, its WAL, and blobs/ (PLAN §11).
	DataDir string
	Logger  *slog.Logger
	// Publish signals the account after a committed change.
	Publish func(account string)
}

// Open opens or creates the database under opts.DataDir and runs the
// forward-only migrations (FR-D.9).
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.DataDir == "" {
		return nil, errors.New("store: DataDir is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	dsn := "file:" + filepath.Join(opts.DataDir, "bridge.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One connection keeps the single-writer discipline obvious and makes
	// PRAGMA behaviour uniform (they are per-connection); reads and writes
	// interleave inside WAL anyway.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, log: log, data: opts.DataDir, blobs: newBlobStore(opts.DataDir), publish: opts.Publish}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close checkpoints and closes the database.
func (s *Store) Close() error {
	// A final WAL checkpoint makes a plain directory copy a valid backup
	// (FR-D.9) while we are stopped.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Warn("store: final checkpoint failed", "err", err)
	}
	return s.db.Close()
}

// tx runs fn in one transaction. When notify is set, subscribers are
// signalled after a successful commit — never before, so a rolled back
// change is never announced (PLAN §10).
func (s *Store) tx(ctx context.Context, account string, notify bool, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	if notify && account != "" && s.publish != nil {
		s.publish(account)
	}
	return nil
}

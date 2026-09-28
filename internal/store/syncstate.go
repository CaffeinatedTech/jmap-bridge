package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FolderSync is the engine's per-folder sync bookkeeping, persisted in
// sync_state with scope "folder:<name>" (PLAN §4): the QRESYNC/CONDSTORE
// anchor, the backfill cursor (FR-S.3 resumable across restarts), and
// the uidnext we have already swept for new messages (FR-S.5).
type FolderSync struct {
	UIDValidity   uint32 `json:"uidvalidity"`
	UIDNext       uint64 `json:"uidnext"`
	HighestModSeq uint64 `json:"highestmodseq"`
	// BackfillUID is the highest uid whose headers have been ingested;
	// BackfillDone marks the folder fully covered up to UIDNext.
	BackfillUID  uint32 `json:"backfill_uid"`
	BackfillDone bool   `json:"backfill_done"`
}

// FolderSync reads one folder's bookkeeping; a missing row is the
// zero value (never backfilled).
func (s *Store) FolderSync(ctx context.Context, account, folder string) (FolderSync, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT sync_token FROM sync_state WHERE account = ? AND scope = ?`,
		account, "folder:"+folder).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return FolderSync{}, nil
	}
	if err != nil {
		return FolderSync{}, fmt.Errorf("store: folder sync %q: %w", folder, err)
	}
	var fs FolderSync
	if raw != "" {
		if err := jsonUnmarshal(raw, &fs); err != nil {
			return FolderSync{}, fmt.Errorf("store: decode folder sync %q: %w", folder, err)
		}
	}
	return fs, nil
}

// SaveFolderSync writes one folder's bookkeeping (no modseq, no
// publish: this is engine state, not a client-visible change).
func (s *Store) SaveFolderSync(ctx context.Context, account, folder string, fs FolderSync) error {
	raw, err := jsonString(fs)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_state(account, scope, sync_token) VALUES (?, ?, ?)
		 ON CONFLICT(account, scope) DO UPDATE SET sync_token = excluded.sync_token`,
		account, "folder:"+folder, raw); err != nil {
		return fmt.Errorf("store: save folder sync %q: %w", folder, err)
	}
	return tx.Commit()
}

// ClearFolderSync forgets a folder's bookkeeping — the UIDVALIDITY
// reset path, where every cursor must start over (FR-S.6).
func (s *Store) ClearFolderSync(ctx context.Context, account, folder string) error {
	return s.SaveFolderSync(ctx, account, folder, FolderSync{})
}

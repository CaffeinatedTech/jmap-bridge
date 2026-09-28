package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
)

// Loc is where the store currently addresses one email on the backend:
// a folder name and a uid inside it (hydration needs both, because
// UIDs only exist inside a selected mailbox).
type Loc struct {
	Folder string
	UID    uint32
}

// FlagUpdate is one message's fresh FLAGS as a folder reports them.
type FlagUpdate struct {
	UID   uint32
	Flags []string
}

// FolderUIDs lists the folder's current uids (its uidvalidity only —
// stale rows cannot exist after FR-S.6 resets drop them).
func (s *Store) FolderUIDs(ctx context.Context, account, folder string) ([]uint32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT uid FROM imap_uids WHERE account = ? AND folder = ? ORDER BY uid`, account, folder)
	if err != nil {
		return nil, fmt.Errorf("store: folder uids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []uint32
	for rows.Next() {
		var u uint32
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FolderUIDCount is the number of mapped uids — the store's side of
// FR-S.5's expunge detection (compare against STATUS MESSAGES).
func (s *Store) FolderUIDCount(ctx context.Context, account, folder string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM imap_uids WHERE account = ? AND folder = ?`,
		account, folder).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: folder uid count: %w", err)
	}
	return n, nil
}

// Locations maps emails to a folder/uid pair for backend fetches.
func (s *Store) Locations(ctx context.Context, account string, emailIDs []string) (map[string]Loc, error) {
	out := map[string]Loc{}
	if len(emailIDs) == 0 {
		return out, nil
	}
	ph, args := idPlaceholders(emailIDs)
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.email_id, u.folder, u.uid FROM imap_uids u
		 JOIN mailboxes m ON m.name = u.folder AND m.account = u.account
		 WHERE u.account = ? AND m.deleted IS NULL AND u.email_id IN (`+ph+`)`,
		append([]any{account}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store: locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, folder string
		var uid uint32
		if err := rows.Scan(&id, &folder, &uid); err != nil {
			return nil, err
		}
		if _, ok := out[id]; !ok {
			out[id] = Loc{Folder: folder, UID: uid}
		}
	}
	return out, rows.Err()
}

// UnhydratedRecent lists ids inside the prefetch window whose bodies
// have not been fetched yet, newest first (FR-S.9).
func (s *Store) UnhydratedRecent(ctx context.Context, account string, window time.Duration, limit int) ([]string, error) {
	cutoff := time.Now().Add(-window).UnixMicro()
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id FROM emails e JOIN email_content c ON c.id = e.id
		 WHERE e.account = ? AND e.deleted IS NULL
		   AND c.hydrated_at IS NULL AND e.received_at > ?
		 ORDER BY e.received_at DESC LIMIT ?`, account, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("store: unhydrated recent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpdateFlagsBulk applies a folder's whole flag snapshot in one
// transaction — the baseline tier's per-pass diff (FR-S.5 tier 3) and
// the cheapest correct way to fold a large change set into counts and
// state exactly once.
func (s *Store) UpdateFlagsBulk(ctx context.Context, account, folder string, uidValidity uint32, updates []FlagUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		touched := false
		for _, u := range updates {
			emailID, err := uidEmailID(ctx, tx, account, folder, uidValidity, u.UID)
			if err != nil {
				return err
			}
			if emailID == "" {
				continue
			}
			if err := applyFlags(ctx, tx, account, emailID, keyword.FromIMAP(u.Flags), seq); err != nil {
				return err
			}
			touched = true
		}
		if !touched {
			return nil
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// IsHydrated reports whether a body has been fetched and committed
// (FR-S.8 bookkeeping; readiness and tests read it).
func (s *Store) IsHydrated(ctx context.Context, account, emailID string) (bool, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT c.hydrated_at FROM email_content c JOIN emails e ON e.id = c.id
		 WHERE c.id = ? AND e.account = ?`, emailID, account).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return at.Valid, nil
}

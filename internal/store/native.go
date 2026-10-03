package store

import (
	"context"
	"database/sql"
	"fmt"
)

// NativeID/message mapping for the Gmail API backend (D-API-9). The
// engine's cache is keyed by an integer UID inside a folder; the Gmail
// API addresses messages by an opaque string id. The adapter allocates a
// stable synthetic UID per native id here and persists the pairing, so
// the whole existing store/commit/hydration path is reused unchanged and
// the Gmail API is just another provider behind the mailbackend seam.
//
// The mapping is backend-neutral: kind discriminates message/label/thread
// so a future Graph adapter reuses the same table.

// kindMessage is the native_ids kind for mail messages.
const kindMessage = "message"

// nextNativeUID allocates a fresh synthetic UID inside tx. A dedicated
// counter (not the id-minting 'seq') keeps UIDs small and independent of
// state-string allocation; it is process-global like 'seq' so ids cannot
// collide across accounts.
func nextNativeUID(ctx context.Context, tx *sql.Tx) (uint32, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO counters(name, value) VALUES ('native_uid', 1)
		 ON CONFLICT(name) DO UPDATE SET value = value + 1`); err != nil {
		return 0, fmt.Errorf("store: allocate native uid: %w", err)
	}
	var v uint32
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE name = 'native_uid'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read native uid: %w", err)
	}
	return v, nil
}

// NativeUID returns the synthetic UID allocated for a native message id,
// and whether one exists.
func (s *Store) NativeUID(ctx context.Context, account, native string) (uint32, bool, error) {
	var uid uint32
	err := s.db.QueryRowContext(ctx,
		`SELECT uid FROM native_ids WHERE account = ? AND kind = ? AND native_id = ?`,
		account, kindMessage, native).Scan(&uid)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: native uid: %w", err)
	}
	return uid, true, nil
}

// AllocNativeUID returns the synthetic UID for a native message id,
// allocating one on first sight. It is idempotent, so concurrent
// sessions (work and idle) converge on one UID per message.
func (s *Store) AllocNativeUID(ctx context.Context, account, native string) (uint32, error) {
	var uid uint32
	err := s.tx(ctx, account, false, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT uid FROM native_ids WHERE account = ? AND kind = ? AND native_id = ?`,
			account, kindMessage, native).Scan(&uid)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("store: native uid lookup: %w", err)
		}
		uid, err = nextNativeUID(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO native_ids(account, kind, native_id, uid) VALUES (?, ?, ?, ?)`,
			account, kindMessage, native, uid)
		if err != nil {
			return fmt.Errorf("store: map native uid: %w", err)
		}
		return nil
	})
	return uid, err
}

// AllocNativeUIDs returns stable synthetic UIDs for a batch of native
// message ids in one transaction, allocating for any unseen. Lookup and
// allocation share one connection so a large backfill batch costs one
// write, not one per message.
func (s *Store) AllocNativeUIDs(ctx context.Context, account string, natives []string) (map[string]uint32, error) {
	out := make(map[string]uint32, len(natives))
	if len(natives) == 0 {
		return out, nil
	}
	err := s.tx(ctx, account, false, func(tx *sql.Tx) error {
		for _, native := range natives {
			if native == "" {
				continue
			}
			var uid uint32
			err := tx.QueryRowContext(ctx,
				`SELECT uid FROM native_ids WHERE account = ? AND kind = ? AND native_id = ?`,
				account, kindMessage, native).Scan(&uid)
			if err == nil {
				out[native] = uid
				continue
			}
			if err != sql.ErrNoRows {
				return fmt.Errorf("store: native uid lookup: %w", err)
			}
			uid, err = nextNativeUID(ctx, tx)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO native_ids(account, kind, native_id, uid) VALUES (?, ?, ?, ?)`,
				account, kindMessage, native, uid); err != nil {
				return fmt.Errorf("store: map native uid: %w", err)
			}
			out[native] = uid
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NativeByUID resolves a synthetic UID back to its native message id; ok
// is false when the UID was not allocated by this account.
func (s *Store) NativeByUID(ctx context.Context, account string, uid uint32) (string, bool, error) {
	var native string
	err := s.db.QueryRowContext(ctx,
		`SELECT native_id FROM native_ids WHERE account = ? AND kind = ? AND uid = ?`,
		account, kindMessage, uid).Scan(&native)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: native by uid: %w", err)
	}
	return native, true, nil
}

// NativeByUIDs resolves a batch of synthetic UIDs to native ids.
func (s *Store) NativeByUIDs(ctx context.Context, account string, uids []uint32) (map[uint32]string, error) {
	out := make(map[uint32]string, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(uids); start += chunk {
		end := min(start+chunk, len(uids))
		q := `SELECT uid, native_id FROM native_ids WHERE account = ? AND kind = ? AND uid IN (`
		args := []any{account, kindMessage}
		for i, u := range uids[start:end] {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, u)
		}
		q += ")"
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: native by uids: %w", err)
		}
		for rows.Next() {
			var uid uint32
			var native string
			if err := rows.Scan(&uid, &native); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[uid] = native
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// KnownMemberUIDs reports which of uids already sit live in the named
// container. The Gmail API backfill uses it to skip re-fetching metadata
// for messages a restart has already ingested.
func (s *Store) KnownMemberUIDs(ctx context.Context, account, container string, uids []uint32) (map[uint32]bool, error) {
	out := make(map[uint32]bool, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(uids); start += chunk {
		end := min(start+chunk, len(uids))
		q := `SELECT iu.uid FROM imap_uids iu JOIN emails e ON e.id = iu.email_id
		      WHERE iu.account = ? AND iu.folder = ? AND e.deleted IS NULL AND iu.uid IN (`
		args := []any{account, container}
		for i, u := range uids[start:end] {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, u)
		}
		q += ")"
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("store: known member uids: %w", err)
		}
		for rows.Next() {
			var uid uint32
			if err := rows.Scan(&uid); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[uid] = true
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

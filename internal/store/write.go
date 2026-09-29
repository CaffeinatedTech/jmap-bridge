package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
)

// The commit functions here are the local half of an IMAP-first
// mutation (PLAN §7.1, D-14): the server has already answered OK, and
// this is where the cache, the modseq floors and the SSE notification
// catch up so the client's read-your-writes state is true the moment the
// /set response is written (FR-M.13). Nothing here talks to the network.

// ErrMailboxUnknown is returned when a commit names a mailbox id the
// cache does not hold for this account (it was deleted between the API
// layer's validation and the commit — a race the server's own refusal
// normally wins).
var ErrMailboxUnknown = errors.New("store: unknown mailbox")

// Copy is one live membership of an email: the mailbox it sits in, the
// folder path behind that mailbox, and the uid addressing it there.
type Copy struct {
	MailboxID   string
	MailboxUID  int64 // mailboxes.rowid, for the membership math
	Folder      string
	UID         uint32
	UIDValidity uint32
}

// MembershipAdd is a membership a completed IMAP COPY/MOVE created. UID
// is 0 when the server sent no COPYUID: the membership still commits,
// and the uid mapping waits for the next sync pass.
type MembershipAdd struct {
	MailboxID   string
	UID         uint32
	UIDValidity uint32
}

// EmailLive reports whether an email exists and is not tombstoned — the
// existence check behind "unknown id ⇒ notFound" (FR-M.13).
func (s *Store) EmailLive(ctx context.Context, account, emailID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM emails WHERE id = ? AND account = ? AND deleted IS NULL`,
		emailID, account).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: email live: %w", err)
	}
	return true, nil
}

// EmailCopies returns every folder copy of one email, including the uid
// in each. It is what a mutation iterates: keywords apply to all copies
// (IMAP flags are per-copy), membership changes address one copy at a
// time, and destroy expunges every one of them.
func (s *Store) EmailCopies(ctx context.Context, account, emailID string) ([]Copy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.rowid, m.id, m.name, COALESCE(m.uidvalidity, 0)
		   FROM email_mailbox em
		   JOIN mailboxes m ON m.rowid = em.mailbox_uid AND m.account = em.account
		  WHERE em.account = ? AND em.email_id = ? AND em.removed_modseq = 0
		    AND m.deleted IS NULL
		  ORDER BY m.name`, account, emailID)
	if err != nil {
		return nil, fmt.Errorf("store: email copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Copy
	for rows.Next() {
		var c Copy
		var uv int64
		if err := rows.Scan(&c.MailboxUID, &c.MailboxID, &c.Folder, &uv); err != nil {
			return nil, err
		}
		c.UIDValidity = uint32(uv)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	uidRows, err := s.db.QueryContext(ctx,
		`SELECT folder, uid FROM imap_uids WHERE account = ? AND email_id = ? ORDER BY uid`,
		account, emailID)
	if err != nil {
		return nil, fmt.Errorf("store: email uids: %w", err)
	}
	defer func() { _ = uidRows.Close() }()
	uidByFolder := map[string]uint32{}
	for uidRows.Next() {
		var folder string
		var uid uint32
		if err := uidRows.Scan(&folder, &uid); err != nil {
			return nil, err
		}
		if _, dup := uidByFolder[folder]; !dup {
			uidByFolder[folder] = uid
		}
	}
	if err := uidRows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].UID = uidByFolder[out[i].Folder]
	}
	return out, nil
}

// CommitKeywordPatch applies a keyword change the server already
// accepted (FR-M.9): add sets, remove clears, and the unread counts and
// type states follow exactly as they would for a foreign flag change.
// It reports whether anything actually moved — an unchanged patch still
// belongs in the response's `updated`, but must not manufacture a
// state-string bump or an SSE event.
func (s *Store) CommitKeywordPatch(ctx context.Context, account, emailID string, add, remove []string) (bool, error) {
	changed := false
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		var kwJSON string
		err := tx.QueryRowContext(ctx,
			`SELECT keywords FROM emails WHERE id = ? AND account = ? AND deleted IS NULL`,
			emailID, account).Scan(&kwJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: keyword patch: %s is not live", emailID)
		}
		if err != nil {
			return fmt.Errorf("store: keyword patch: %w", err)
		}
		kw := map[string]bool{}
		if kwJSON != "" {
			if err := jsonUnmarshal(kwJSON, &kw); err != nil {
				return fmt.Errorf("store: decode keywords: %w", err)
			}
		}
		for _, k := range add {
			if !kw[k] {
				kw[k] = true
				changed = true
			}
		}
		for _, k := range remove {
			if kw[k] {
				delete(kw, k)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if err := applyFlags(ctx, tx, account, emailID, kw, seq); err != nil {
			return err
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
	return changed, err
}

// CommitMembershipPatch applies a mailbox membership change the server
// already accepted: additions link (and map the new uid when COPYUID
// gave us one), removals unlink and move counts, and an email that ends
// up with no membership at all is tombstoned — the same code path a
// foreign expunge takes, so /changes can never tell the two apart.
//
// A patch can out-run the sync engine: if the engine saw our expunge
// and tombstoned the email before this transaction ran, an addition
// clears that tombstone again (the message demonstrably exists — we just
// put it there).
func (s *Store) CommitMembershipPatch(ctx context.Context, account, emailID string, adds []MembershipAdd, removes []string) error {
	if len(adds) == 0 && len(removes) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		for _, add := range adds {
			mailboxUID, path, err := mailboxByID(ctx, tx, account, add.MailboxID)
			if err != nil {
				return err
			}
			if err := addMembership(ctx, tx, account, mailboxUID, emailID, seq); err != nil {
				return err
			}
			if add.UID != 0 {
				if err := mapUID(ctx, tx, account, path, add.UIDValidity, add.UID, emailID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET deleted = NULL WHERE id = ? AND account = ?`,
				emailID, account); err != nil {
				return fmt.Errorf("store: restore email: %w", err)
			}
		}
		var orphans []orphan
		for _, mailboxID := range removes {
			mailboxUID, path, err := mailboxByID(ctx, tx, account, mailboxID)
			if err != nil {
				return err
			}
			removed, err := removeMembershipLocked(ctx, tx, account, mailboxUID, path, emailID, seq)
			if err != nil {
				return err
			}
			if removed {
				orphans = append(orphans, orphan{uid: mailboxUID, id: emailID})
			}
		}
		if len(orphans) > 0 {
			if err := orphanEmails(ctx, tx, account, seq, orphans); err != nil {
				return err
			}
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// CommitDestroy tombstones emails whose copies the server just expunged
// (FR-M.10): every membership goes with its uid mapping, counts move,
// and /changes reports the ids as destroyed. Ids that are already gone
// are left exactly as they are — destroying an absent message is a
// no-op success, not an error.
func (s *Store) CommitDestroy(ctx context.Context, account string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		for _, emailID := range ids {
			copies, err := emailCopiesLocked(ctx, tx, account, emailID)
			if err != nil {
				return err
			}
			var orphans []orphan
			for _, c := range copies {
				removed, err := removeMembershipLocked(ctx, tx, account, c.MailboxUID, c.Folder, emailID, seq)
				if err != nil {
					return err
				}
				if removed {
					orphans = append(orphans, orphan{uid: c.MailboxUID, id: emailID})
				}
			}
			if len(orphans) > 0 {
				if err := orphanEmails(ctx, tx, account, seq, orphans); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM imap_uids WHERE account = ? AND email_id = ?`, account, emailID); err != nil {
				return fmt.Errorf("store: drop uid mappings: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET deleted = ?, updated_modseq = ?
				 WHERE id = ? AND account = ? AND deleted IS NULL`,
				seq, seq, emailID, account); err != nil {
				return fmt.Errorf("store: tombstone email: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM email_msgid WHERE account = ? AND email_id = ?`, account, emailID); err != nil {
				return fmt.Errorf("store: release message-id: %w", err)
			}
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// CommitAppend inserts a message the bridge itself just APPENDed (the
// draft path, FR-M.11) and returns its JMAP id. The record carries the
// uid the server assigned, so cache and server agree from the first
// read instead of waiting for a sync pass to notice the new message.
func (s *Store) CommitAppend(ctx context.Context, account, folder string, rec MessageRec) (string, error) {
	var id string
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		id, err = createMessage(ctx, tx, account, mailboxUID, folder, &rec, keyword.FromIMAP(rec.Flags), seq)
		if err != nil {
			return err
		}
		if err := touchMailboxCounts(ctx, tx, account, mailboxUID, seq); err != nil {
			return err
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// --- mailbox resolution and commits (FR-M.12) ---

// MailboxPath resolves a mailbox id to its IMAP path.
func (s *Store) MailboxPath(ctx context.Context, account, id string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM mailboxes WHERE id = ? AND account = ? AND deleted IS NULL`,
		id, account).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrMailboxUnknown
	}
	if err != nil {
		return "", fmt.Errorf("store: mailbox path: %w", err)
	}
	return name, nil
}

// MailboxIDByPath resolves an IMAP path to its mailbox id ("" when the
// cache does not know the path yet).
func (s *Store) MailboxIDByPath(ctx context.Context, account, path string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mailboxes WHERE name = ? AND account = ? AND deleted IS NULL`,
		path, account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: mailbox id by path: %w", err)
	}
	return id, nil
}

// MailboxIDByRole resolves a JMAP role (inbox, trash, drafts, …) to the
// mailbox the server's SPECIAL-USE attributes or well-known name gave
// it, "" when the account has no such mailbox.
func (s *Store) MailboxIDByRole(ctx context.Context, account, role string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mailboxes WHERE role = ? AND account = ? AND deleted IS NULL
		  ORDER BY (name = 'INBOX') DESC, name LIMIT 1`,
		role, account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: mailbox by role: %w", err)
	}
	return id, nil
}

// CommitMailboxRename rewrites a mailbox's path in place after the
// server accepted RENAME, keeping its id — RFC 8621 §5.1: renaming is
// an update, never a new object — and rewriting every descendant path
// with it. Descendants keep their own parent ids; only the renamed row
// moves.
func (s *Store) CommitMailboxRename(ctx context.Context, account, id, oldPath, newPath, newParentID string, delim rune) error {
	if oldPath == newPath {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		var one int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM mailboxes WHERE id = ? AND account = ? AND deleted IS NULL`,
			id, account).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMailboxUnknown
		}
		if err != nil {
			return fmt.Errorf("store: rename lookup: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET name = ?, parent_id = ?, updated_modseq = ?
			 WHERE id = ? AND account = ?`,
			newPath, nullStr(newParentID), seq, id, account); err != nil {
			return fmt.Errorf("store: rename mailbox: %w", err)
		}
		if delim != 0 {
			prefix := escapeLike(oldPath+string(delim)) + "%"
			// length()/substr() keep the offset in characters, so a
			// non-ASCII folder name does not split a rune.
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET name = ? || substr(name, length(?) + 1), updated_modseq = ?
				 WHERE account = ? AND id <> ? AND name LIKE ? ESCAPE '\'`,
				newPath, oldPath, seq, account, id, prefix); err != nil {
				return fmt.Errorf("store: rename descendants: %w", err)
			}
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// --- shared row helpers ---

// mailboxByID resolves a mailbox id to its rowid and path inside tx.
func mailboxByID(ctx context.Context, tx *sql.Tx, account, id string) (rowID int64, path string, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT rowid, name FROM mailboxes WHERE id = ? AND account = ? AND deleted IS NULL`,
		id, account).Scan(&rowID, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrMailboxUnknown
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: mailbox %s: %w", id, err)
	}
	return rowID, path, nil
}

// emailCopiesLocked is EmailCopies inside a transaction.
func emailCopiesLocked(ctx context.Context, tx *sql.Tx, account, emailID string) ([]Copy, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT m.rowid, m.id, m.name, COALESCE(m.uidvalidity, 0)
		   FROM email_mailbox em
		   JOIN mailboxes m ON m.rowid = em.mailbox_uid AND m.account = em.account
		  WHERE em.account = ? AND em.email_id = ? AND em.removed_modseq = 0
		    AND m.deleted IS NULL
		  ORDER BY m.name`, account, emailID)
	if err != nil {
		return nil, fmt.Errorf("store: email copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Copy
	for rows.Next() {
		var c Copy
		var uv int64
		if err := rows.Scan(&c.MailboxUID, &c.MailboxID, &c.Folder, &uv); err != nil {
			return nil, err
		}
		c.UIDValidity = uint32(uv)
		out = append(out, c)
	}
	return out, rows.Err()
}

// removeMembershipLocked unlinks one email from one mailbox: the
// membership row, the uid mapping that addressed it, and the four
// counters (plus the thread edges) that move with it. It reports whether
// the membership was live, so the caller decides if a tombstone is due.
// Factored out of RemoveUIDs so a foreign expunge and our own write take
// byte-identical local code.
func removeMembershipLocked(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, folder, emailID string, seq int64) (bool, error) {
	mb, err := tx.ExecContext(ctx,
		`UPDATE email_mailbox SET removed_modseq = ?
		 WHERE mailbox_uid = ? AND email_id = ? AND removed_modseq = 0`,
		seq, mailboxUID, emailID)
	if err != nil {
		return false, fmt.Errorf("store: unlink: %w", err)
	}
	n, _ := mb.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM imap_uids WHERE account = ? AND folder = ? AND email_id = ?`,
		account, folder, emailID); err != nil {
		return false, fmt.Errorf("store: drop uid mapping: %w", err)
	}
	var threadID, kw string
	if err := tx.QueryRowContext(ctx,
		`SELECT thread_id, keywords FROM emails WHERE id = ? AND account = ?`,
		emailID, account).Scan(&threadID, &kw); err != nil {
		return false, fmt.Errorf("store: read email for counts: %w", err)
	}
	unread := !seenIn(kw)
	if _, err := tx.ExecContext(ctx,
		`UPDATE mailboxes SET total_emails = total_emails - 1,
		   unread_emails = unread_emails - ?, updated_modseq = ?
		 WHERE rowid = ? AND account = ?`,
		boolInt(unread), seq, mailboxUID, account); err != nil {
		return false, err
	}
	others, err := threadMembers(ctx, tx, account, mailboxUID, threadID, emailID, false)
	if err != nil {
		return false, err
	}
	if others == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET total_threads = total_threads - 1 WHERE rowid = ? AND account = ?`,
			mailboxUID, account); err != nil {
			return false, err
		}
	}
	if unread {
		othersUnseen, err := threadMembers(ctx, tx, account, mailboxUID, threadID, emailID, true)
		if err != nil {
			return false, err
		}
		if othersUnseen == 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET unread_threads = unread_threads - 1 WHERE rowid = ? AND account = ?`,
				mailboxUID, account); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// escapeLike quotes a string for use inside a LIKE pattern that is
// matched with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

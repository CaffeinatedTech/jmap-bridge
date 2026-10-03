package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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

// EmailState reports whether an email row exists at all and whether it
// is live (not tombstoned). The distinction matters: destroying an id
// the cache has already tombstoned is a no-op success (FR-M.10), while
// updating one — or destroying an id never seen — is notFound.
func (s *Store) EmailState(ctx context.Context, account, emailID string) (exists, live bool, err error) {
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM emails WHERE id = ? AND account = ?`,
		emailID, account).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("store: email state: %w", err)
	}
	var deleted sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT deleted FROM emails WHERE id = ? AND account = ?`,
		emailID, account).Scan(&deleted); err != nil {
		return false, false, fmt.Errorf("store: email state: %w", err)
	}
	return true, !deleted.Valid, nil
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

// CommitPatch applies an Email/set update the server already accepted
// (FR-M.9): keyword deltas and membership deltas in one transaction, so
// the response's state string, the counters and the single SSE
// notification all describe the same commit. IMAP-first means this runs
// only after the STORE/COPY/MOVE/EXPUNGE returned OK.
//
// A patch can also out-run the sync engine: if the engine saw our
// expunge and tombstoned the email before this transaction ran, an
// addition clears that tombstone again (the message demonstrably exists
// — we just put it there).
func (s *Store) CommitPatch(ctx context.Context, account, emailID string, kwAdd, kwRemove []string, adds []MembershipAdd, removes []string) (kwChanged bool, err error) {
	// Decide what actually moves before opening a transaction: a patch
	// that changes nothing must not bump a state floor, allocate a seq
	// or wake every connected client (the response still lists the id as
	// updated — RFC 8620 §5.3 — it just isn't a /changes event).
	cur, err := s.currentKeywords(ctx, account, emailID)
	if err != nil {
		return false, err
	}
	kwAdd = diffAdd(kwAdd, cur)
	kwRemove = diffRemove(kwRemove, cur)
	kwChanged = len(kwAdd) > 0 || len(kwRemove) > 0
	if !kwChanged && len(adds) == 0 && len(removes) == 0 {
		return false, nil
	}

	err = s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		// --- keywords ---
		if kwChanged {
			kw := map[string]bool{}
			for k, v := range cur {
				kw[k] = v
			}
			for _, k := range kwAdd {
				kw[k] = true
			}
			for _, k := range kwRemove {
				delete(kw, k)
			}
			if err := applyFlags(ctx, tx, account, emailID, kw, seq); err != nil {
				return err
			}
		}

		// --- membership ---
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
	return kwChanged, err
}

// EmailKeywords reads one email's keyword set (no hydration side
// effects — the backend needs it to resolve the whole-property patch
// form before it can talk to the server).
func (s *Store) EmailKeywords(ctx context.Context, account, emailID string) (map[string]bool, error) {
	return s.currentKeywords(ctx, account, emailID)
}

// currentKeywords reads one email's keyword set ("" row → error: the
// caller has already established the email exists).
func (s *Store) currentKeywords(ctx context.Context, account, emailID string) (map[string]bool, error) {
	var kwJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT keywords FROM emails WHERE id = ? AND account = ?`,
		emailID, account).Scan(&kwJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: patch: %s not found", emailID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: patch keywords: %w", err)
	}
	kw := map[string]bool{}
	if kwJSON != "" {
		if err := jsonUnmarshal(kwJSON, &kw); err != nil {
			return nil, fmt.Errorf("store: decode keywords: %w", err)
		}
	}
	return kw, nil
}

// diffAdd/diffRemove reduce a requested delta to the part that is
// actually a change.
func diffAdd(want []string, have map[string]bool) []string {
	var out []string
	for _, k := range want {
		if !have[k] {
			out = append(out, k)
		}
	}
	return out
}

func diffRemove(want []string, have map[string]bool) []string {
	var out []string
	for _, k := range want {
		if have[k] {
			out = append(out, k)
		}
	}
	return out
}

// EmptyFolder removes every message a folder holds from the cache —
// the local half of destroying a mailbox with onDestroyRemoveEmails
// (RFC 8621 §2.5): each copy is unlinked, mail that lived only there is
// tombstoned, and the uid mappings go with them.
func (s *Store) EmptyFolder(ctx context.Context, account, folder string) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT email_id FROM imap_uids WHERE account = ? AND folder = ?`,
			account, folder)
		if err != nil {
			return fmt.Errorf("store: folder members: %w", err)
		}
		var emailIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			emailIDs = append(emailIDs, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var orphans []orphan
		for _, emailID := range emailIDs {
			removed, err := removeMembershipLocked(ctx, tx, account, mailboxUID, folder, emailID, seq)
			if err != nil {
				return err
			}
			if removed {
				orphans = append(orphans, orphan{uid: mailboxUID, id: emailID})
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM imap_uids WHERE account = ? AND folder = ?`, account, folder); err != nil {
			return fmt.Errorf("store: drop folder uids: %w", err)
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

// AppendResult is what a committed APPEND produced: the new id plus the
// server-set properties RFC 8621 §4.6 requires in Email/set `created`.
type AppendResult struct {
	ID       string
	ThreadID string
	Size     int64
}

// CommitAppend inserts a message the bridge itself just APPENDed (the
// draft path, FR-M.11). The record carries the uid the server assigned,
// so cache and server agree from the first read instead of waiting for
// a sync pass to notice the new message.
func (s *Store) CommitAppend(ctx context.Context, account, folder string, rec MessageRec) (AppendResult, error) {
	var out AppendResult
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		id, err := createMessage(ctx, tx, account, mailboxUID, folder, &rec, keyword.FromIMAP(rec.Flags), seq)
		if err != nil {
			return err
		}
		out.ID = id
		// Backend-neutral native link (M11): for a Gmail API account the
		// synthetic uid has a native_ids row and this records the JMAP id
		// beside it; for IMAP there is no row and it is a no-op.
		if rec.UID != 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE native_ids SET jmap_id = ? WHERE account = ? AND kind = ? AND uid = ?`,
				id, account, kindMessage, rec.UID); err != nil {
				return fmt.Errorf("store: link native jmap id: %w", err)
			}
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT thread_id, size FROM emails WHERE id = ? AND account = ?`,
			id, account).Scan(&out.ThreadID, &out.Size); err != nil {
			return fmt.Errorf("store: read created email: %w", err)
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
		return AppendResult{}, err
	}
	return out, nil
}

// PutBlob stores one immutable blob (the raw bytes of a message we just
// built, so Email/set `created` can answer with a blobId and M3's
// submission can reuse the bytes) and returns its id.
func (s *Store) PutBlob(ctx context.Context, account, mediaType string, data []byte) (string, error) {
	var id string
	// No publish: a blob is not a JMAP object, and no type state moves.
	err := s.tx(ctx, "", false, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		id = newID(time.Now(), seq)
		return s.blobs.put(ctx, tx, account, id, mediaType, data)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// LinkRawBlob records blobID as the message's raw RFC 5322 copy
// (FR-M.4's blobId, RFC 8621 §4.1.1). Blob and message are already in
// the store; this only says they refer to the same bytes.
func (s *Store) LinkRawBlob(ctx context.Context, account, emailID, blobID string) error {
	// No publish: the blob id appears lazily like a body, and no type
	// state moves with it (the SSE layer only emits states that did).
	return s.tx(ctx, "", false, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE email_content SET raw_blob_id = ?
			 WHERE id = ? AND EXISTS (SELECT 1 FROM emails WHERE id = ? AND account = ?)`,
			blobID, emailID, emailID, account)
		if err != nil {
			return fmt.Errorf("store: link raw blob: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("store: link raw blob: email %s not found in account %s", emailID, account)
		}
		return nil
	})
}

// RawBlobID returns the blob holding the email's raw message, or ""
// when the bridge does not hold those bytes (they are fetched on
// demand, bodies being lazy — D-2).
func (s *Store) RawBlobID(ctx context.Context, account, emailID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(c.raw_blob_id, '') FROM email_content c
		 JOIN emails e ON e.id = c.id
		 WHERE c.id = ? AND e.account = ? AND e.deleted IS NULL`,
		emailID, account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: raw blob id: %w", err)
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

// SetMailboxSortOrder stamps a client-requested display order onto one
// mailbox (FR-M.12): IMAP folders carry no order, so the cache is the
// only place a client-requested order can live. A later discovery pass
// may re-derive it; the bump keeps the change visible to clients.
func (s *Store) SetMailboxSortOrder(ctx context.Context, account, mailboxID string, sortOrder int) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET sort_order = ?, updated_modseq = ? WHERE id = ? AND account = ?`,
			sortOrder, seq, mailboxID, account); err != nil {
			return fmt.Errorf("store: set sort order: %w", err)
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
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
// an update, never a new object. renameChildren reports what LIST said
// the server did to the subtree: conformant servers rename descendants
// too (RFC 3501 §6.3.5), others move only the node, and the cache must
// follow LIST either way (golden rule 1). Descendants keep their own
// parent ids; only the renamed row moves.
func (s *Store) CommitMailboxRename(ctx context.Context, account, id, oldPath, newPath, newParentID string, delim rune, renameChildren bool) error {
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
		if renameChildren && delim != 0 {
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

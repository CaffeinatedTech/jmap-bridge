package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
)

// ErrFolderUnknown is returned when a mutation names a folder discovery
// has not seen yet; the engine re-runs discovery instead of inventing a
// mailbox.
var ErrFolderUnknown = errors.New("store: unknown folder")

// MessageRec is one message's header-level summary after conversion
// from an IMAP ENVELOPE/BODYSTRUCTURE fetch. It never carries a body
// (golden rule 2); previews arrive separately from the lazy preview
// fetch.
type MessageRec struct {
	UID         uint32
	UIDValidity uint32
	Flags       []string // raw IMAP flags, as the server sent them
	ReceivedAt  time.Time
	Size        int64
	ModSeq      uint64

	// SharedUIDs marks a server whose UIDs identify a message across
	// every folder (Gmail: one UID per account). When set, a record
	// whose (uidvalidity, uid) is already mapped through another folder
	// dedupes to that email regardless of Message-ID presence — the
	// same server-side message cannot become two JMAP objects.
	SharedUIDs bool
	// GmThrid is Gmail's X-GM-THRID when the server reported one; it
	// seeds thread ids ahead of Message-ID/References (FR-S.10).
	GmThrid uint64

	// Canonical JMAP header object and its lowercase query mirrors.
	HeadersJSON string
	SubjectL    string
	FromL       string
	ToL         string

	Subject    string   // raw subject, for thread derivation (FR-M.7)
	MessageIDs []string // the message's own Message-ID header values
	References []string
	InReplyTo  []string
	SentAt     *time.Time

	Structure     string // JMAP EmailBodyPart tree (JSON)
	HasAttachment bool
	Preview       string // "" until the preview fetch fills it
}

// PutMessages ingests a batch of header records for one folder
// (FR-S.3 backfill and the new-message half of FR-S.5 incremental). It
// deduplicates by Message-ID across folders, creates JMAP ids for
// genuinely new messages, derives threads (FR-M.7), maintains mailbox
// counts, and bumps every state floor the batch touched.
func (s *Store) PutMessages(ctx context.Context, account, folder string, recs []MessageRec) error {
	if len(recs) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, folderUV, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		mailboxDirty := false
		for i := range recs {
			rec := &recs[i]
			if rec.UIDValidity != 0 && folderUV != 0 && rec.UIDValidity != folderUV {
				// The engine resets the folder (FR-S.6) before the next
				// batch; ingesting under the wrong validity would create
				// mappings that cannot be reconciled later.
				return fmt.Errorf("store: uidvalidity mismatch for %q: folder=%d rec=%d",
					folder, folderUV, rec.UIDValidity)
			}
			keywords := keyword.FromIMAP(rec.Flags)
			known, err := uidEmailID(ctx, tx, account, folder, rec.UIDValidity, rec.UID)
			if err != nil {
				return err
			}
			if known != "" {
				// Same uid re-fetched (flag refresh or re-backfill).
				if err := applyFlags(ctx, tx, account, known, keywords, seq); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE imap_uids SET email_id = email_id WHERE account = ? AND folder = ?
					   AND uidvalidity = ? AND uid = ?`,
					account, folder, rec.UIDValidity, rec.UID); err != nil {
					return err
				}
				mailboxDirty = true
				continue
			}

			// Shared-UID servers (Gmail): the same uidvalidity+uid in a
			// different folder is the same server-side message, whatever
			// its headers say. Flags are global per message there, so
			// applying the fetched flags is always truthful.
			if rec.SharedUIDs {
				if id := liveEmailBySharedUID(ctx, tx, account, rec.UIDValidity, rec.UID, folder); id != "" {
					if err := applyFlags(ctx, tx, account, id, keywords, seq); err != nil {
						return err
					}
					if err := addMembership(ctx, tx, account, mailboxUID, id, seq); err != nil {
						return err
					}
					if err := mapUID(ctx, tx, account, folder, rec.UIDValidity, rec.UID, id); err != nil {
						return err
					}
					mailboxDirty = true
					continue
				}
			}

			// Dedupe: a message already known through another folder keeps
			// its id — the same mail is one JMAP Email with two mailboxes.
			// Three limits keep that merge lossless (the cache never
			// reports state the server does not hold):
			//   - cross-folder only: a second copy in the same folder is a
			//     second delivery and gets its own message (reusing the id
			//     would inflate the membership the counts rely on);
			//   - flags-agree only: JMAP keywords are per-object, so two
			//     IMAP copies whose flags differ (self-sent mail: the Sent
			//     copy is \Seen, the delivered copy is not) cannot share
			//     one object truthfully — whichever folder synced last
			//     would win and the unread counts would flap by race;
			//   - \Recent is ignored in the comparison: it is per-session
			//     bookkeeping the server stamps on arrival, not message
			//     state a client sees consistently.
			if id := liveEmailByMsgID(ctx, tx, account, rec.MessageIDs); id != "" {
				liveHere, err := hasLiveMembership(ctx, tx, mailboxUID, id)
				if err != nil {
					return err
				}
				agree, err := flagsAgree(ctx, tx, account, id, keywords)
				if err != nil {
					return err
				}
				if !liveHere && agree {
					if err := applyFlags(ctx, tx, account, id, keywords, seq); err != nil {
						return err
					}
					if err := addMembership(ctx, tx, account, mailboxUID, id, seq); err != nil {
						return err
					}
					if err := mapUID(ctx, tx, account, folder, rec.UIDValidity, rec.UID, id); err != nil {
						return err
					}
					mailboxDirty = true
					continue
				}
			}

			if _, err := createMessage(ctx, tx, account, mailboxUID, folder, rec, keywords, seq); err != nil {
				return err
			}
			mailboxDirty = true
		}
		if mailboxDirty {
			if err := touchMailboxCounts(ctx, tx, account, mailboxUID, seq); err != nil {
				return err
			}
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// createMessage mints ids and writes the four rows for one new message,
// returning the JMAP id it created.
func createMessage(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, folder string, rec *MessageRec, keywords map[string]bool, seq int64) (string, error) {
	id, err := mintID(ctx, tx)
	if err != nil {
		return "", err
	}
	threadID, err := deriveThread(ctx, tx, account, ThreadInput{
		MessageID:  first(rec.MessageIDs),
		References: rec.References,
		InReplyTo:  rec.InReplyTo,
		Subject:    rec.Subject,
		GmThrid:    rec.GmThrid,
	}, seq)
	if err != nil {
		return "", err
	}
	kwJSON, err := jsonString(keywords)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO emails(id, account, thread_id, keywords, received_at, size,
		   has_attachment, preview, created_modseq, updated_modseq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, account, threadID, kwJSON, rec.ReceivedAt.UnixMicro(), rec.Size,
		boolInt(rec.HasAttachment), rec.Preview, seq, seq); err != nil {
		return "", fmt.Errorf("store: insert email: %w", err)
	}
	msgIDs, err := jsonString(rec.MessageIDs)
	if err != nil {
		return "", err
	}
	inReplyTo, err := jsonString(rec.InReplyTo)
	if err != nil {
		return "", err
	}
	refs, err := jsonString(rec.References)
	if err != nil {
		return "", err
	}
	var sentAt any
	if rec.SentAt != nil {
		sentAt = rec.SentAt.UnixMicro()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO email_content(id, headers, message_ids, in_reply_to, "references",
		   sent_at, subject_l, from_l, to_l, body_structure)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, rec.HeadersJSON, msgIDs, inReplyTo, refs,
		sentAt, rec.SubjectL, rec.FromL, rec.ToL, rec.Structure); err != nil {
		return "", fmt.Errorf("store: insert content: %w", err)
	}
	if err := addMembership(ctx, tx, account, mailboxUID, id, seq); err != nil {
		return "", err
	}
	if rec.UID != 0 || rec.UIDValidity != 0 {
		if err := mapUID(ctx, tx, account, folder, rec.UIDValidity, rec.UID, id); err != nil {
			return "", err
		}
	}
	if mid := firstNonEmpty(rec.MessageIDs); mid != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO email_msgid(account, msgid, email_id) VALUES (?, ?, ?)`,
			account, mid, id); err != nil {
			return "", fmt.Errorf("store: index message-id: %w", err)
		}
	}
	return id, nil
}

// addMembership links an email into a mailbox and moves the four counts
// (FR-M.1). The queries that decide thread counts start from the
// thread index, so a bulk backfill stays index-shaped (PLAN §4 risk).
// The counts move only when the membership is fresh: an upsert that
// hits an already-live row is a no-op for the counters, mirroring
// removeMembershipLocked's decrement-only-when-live (otherwise every
// duplicate arrival of the same uid set inflates the mailbox counts).
func addMembership(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, emailID string, seq int64) error {
	existed, err := hasLiveMembership(ctx, tx, mailboxUID, emailID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO email_mailbox(account, mailbox_uid, email_id, removed_modseq, added_modseq)
		 VALUES (?, ?, ?, 0, ?)
		 ON CONFLICT(mailbox_uid, email_id) WHERE removed_modseq = 0
		 DO UPDATE SET removed_modseq = 0, added_modseq = excluded.added_modseq`,
		account, mailboxUID, emailID, seq); err != nil {
		return fmt.Errorf("store: add membership: %w", err)
	}
	if existed {
		return nil
	}
	// Counts: this email's own contribution, plus thread edges that were
	// not present before this insert.
	var threadID, kw string
	if err := tx.QueryRowContext(ctx,
		`SELECT thread_id, keywords FROM emails WHERE id = ? AND account = ?`,
		emailID, account).Scan(&threadID, &kw); err != nil {
		return fmt.Errorf("store: read email for counts: %w", err)
	}
	unread := !seenIn(kw)
	if _, err := tx.ExecContext(ctx,
		`UPDATE mailboxes SET total_emails = total_emails + 1,
		   unread_emails = unread_emails + ?, updated_modseq = ?
		 WHERE rowid = ? AND account = ?`,
		boolInt(unread), seq, mailboxUID, account); err != nil {
		return err
	}
	// This thread had no other member in this mailbox?
	others, err := threadMembers(ctx, tx, account, mailboxUID, threadID, emailID, false)
	if err != nil {
		return err
	}
	if others == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET total_threads = total_threads + 1 WHERE rowid = ? AND account = ?`,
			mailboxUID, account); err != nil {
			return err
		}
	}
	if unread {
		othersUnseen, err := threadMembers(ctx, tx, account, mailboxUID, threadID, emailID, true)
		if err != nil {
			return err
		}
		if othersUnseen == 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET unread_threads = unread_threads + 1 WHERE rowid = ? AND account = ?`,
				mailboxUID, account); err != nil {
				return err
			}
		}
	}
	return nil
}

// threadMembers counts this email's siblings in a mailbox: members of
// the same thread, excluding emailID, optionally only the unseen ones.
// It starts from the per-thread email index so it does not scan the
// mailbox.
func threadMembers(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, threadID, emailID string, onlyUnseen bool) (int, error) {
	q := `SELECT COUNT(*) FROM email_mailbox em
	      JOIN emails e ON e.id = em.email_id
	      WHERE e.account = ? AND e.thread_id = ? AND e.id <> ?
	        AND em.mailbox_uid = ? AND em.removed_modseq = 0 AND e.deleted IS NULL`
	args := []any{account, threadID, emailID, mailboxUID}
	if onlyUnseen {
		q += ` AND COALESCE(json_extract(e.keywords, '$."$seen"'), 0) <> 1`
	}
	var n int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: thread siblings: %w", err)
	}
	return n, nil
}

// applyFlags replaces an email's keywords when they differ, moving the
// unread counts of every mailbox that holds it and bumping the Email
// state (FR-S.5 flag half, FR-M.8).
func applyFlags(ctx context.Context, tx *sql.Tx, account, emailID string, keywords map[string]bool, seq int64) error {
	var kwJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT keywords FROM emails WHERE id = ? AND account = ?`, emailID, account).Scan(&kwJSON)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	next, err := jsonString(keywords)
	if err != nil {
		return err
	}
	if next == kwJSON {
		return nil
	}
	wasUnread := !seenIn(kwJSON)
	nowUnread := !seenIn(next)
	var threadID string
	if err := tx.QueryRowContext(ctx,
		`SELECT thread_id FROM emails WHERE id = ? AND account = ?`, emailID, account).Scan(&threadID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE emails SET keywords = ?, updated_modseq = ? WHERE id = ? AND account = ?`,
		next, seq, emailID, account); err != nil {
		return fmt.Errorf("store: update keywords: %w", err)
	}
	if wasUnread == nowUnread {
		return nil // no count effect; the state bump below still lands
	}
	// Unread count moves for every mailbox holding this email; the
	// per-thread unread edge moves only when the thread crosses zero.
	mrows, err := tx.QueryContext(ctx,
		`SELECT mailbox_uid FROM email_mailbox
		 WHERE email_id = ? AND removed_modseq = 0 AND account = ?`,
		emailID, account)
	if err != nil {
		return err
	}
	var boxes []int64
	for mrows.Next() {
		var uid int64
		if err := mrows.Scan(&uid); err != nil {
			_ = mrows.Close()
			return err
		}
		boxes = append(boxes, uid)
	}
	_ = mrows.Close()
	if err := mrows.Err(); err != nil {
		return err
	}
	var delta int
	if wasUnread && !nowUnread {
		delta = -1
	} else if !wasUnread && nowUnread {
		delta = 1
	}
	for _, box := range boxes {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mailboxes SET unread_emails = unread_emails + ?, updated_modseq = ?
			 WHERE rowid = ? AND account = ?`,
			delta, seq, box, account); err != nil {
			return err
		}
		othersUnseen, err := threadMembers(ctx, tx, account, box, threadID, emailID, true)
		if err != nil {
			return err
		}
		if othersUnseen == 0 {
			// This email was the thread's only unseen member (or now is):
			// the edge follows the direction of the change.
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET unread_threads = unread_threads + ? WHERE rowid = ? AND account = ?`,
				delta, box, account); err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateFlags is the incremental flag path (FR-S.5): the engine reports
// a UID's fresh FLAGS for a folder and the store reconciles keywords,
// counts and state. Unknown uids are reported as false — the engine
// fetches details for them instead.
func (s *Store) UpdateFlags(ctx context.Context, account, folder string, uidValidity, uid uint32, flags []string) (bool, error) {
	found := false
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		emailID, err := uidEmailID(ctx, tx, account, folder, uidValidity, uid)
		if err != nil {
			return err
		}
		if emailID == "" {
			return nil
		}
		found = true
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if err := applyFlags(ctx, tx, account, emailID, keyword.FromIMAP(flags), seq); err != nil {
			return err
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
	return found, err
}

// RemoveUIDs tombstones expunged messages of one folder (FR-S.5: the
// missing UIDs become tombstones, not silent row deletions). Emails that
// still live in another folder keep their id and only lose this
// membership.
func (s *Store) RemoveUIDs(ctx context.Context, account, folder string, uidValidity uint32, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		var orphans []orphan
		for _, uid := range uids {
			emailID, err := uidEmailID(ctx, tx, account, folder, uidValidity, uid)
			if err != nil {
				return err
			}
			if emailID == "" {
				continue
			}
			removed, err := removeMembershipLocked(ctx, tx, account, mailboxUID, folder, emailID, seq)
			if err != nil {
				return err
			}
			if removed {
				orphans = append(orphans, orphan{uid: mailboxUID, id: emailID})
			}
		}
		if err := orphanEmails(ctx, tx, account, seq, orphans); err != nil {
			return err
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// ResetFolder prepares a folder whose UIDVALIDITY changed (FR-S.6):
// every uid mapping is dropped, mail that lived only in this folder is
// tombstoned with its Message-ID index released so the re-backfill mints
// fresh ids, and mail still visible elsewhere keeps its id and loses
// only this membership. The caller re-backfills afterwards.
func (s *Store) ResetFolder(ctx context.Context, account, folder string, newUIDValidity uint32) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if err := resetFolderLocked(ctx, tx, account, mailboxUID, newUIDValidity, seq); err != nil {
			return err
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// resetFolderLocked is ResetFolder's body, callable from SyncFolders'
// discovery transaction (FR-S.6).
func resetFolderLocked(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, newUIDValidity uint32, seq int64) error {
	var emailIDs []string
	rows, err := tx.QueryContext(ctx,
		`SELECT email_id FROM imap_uids WHERE account = ?
		   AND folder = (SELECT name FROM mailboxes WHERE rowid = ? AND account = ?)`,
		account, mailboxUID, account)
	if err != nil {
		return err
	}
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
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM imap_uids WHERE account = ?
		   AND folder = (SELECT name FROM mailboxes WHERE rowid = ? AND account = ?)`,
		account, mailboxUID, account); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_mailbox SET removed_modseq = ?
		 WHERE mailbox_uid = ? AND removed_modseq = 0`, seq, mailboxUID); err != nil {
		return err
	}
	for _, id := range emailIDs {
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM email_mailbox
			 WHERE email_id = ? AND removed_modseq = 0`, id).Scan(&others); err != nil {
			return err
		}
		if others > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET updated_modseq = ? WHERE id = ? AND account = ?`,
				seq, id, account); err != nil {
				return err
			}
			continue
		}
		// Only this folder knew it: destroy and release its ids.
		if _, err := tx.ExecContext(ctx,
			`UPDATE emails SET deleted = ?, updated_modseq = ? WHERE id = ? AND account = ? AND deleted IS NULL`,
			seq, seq, id, account); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM email_msgid WHERE account = ? AND email_id = ?`, account, id); err != nil {
			return err
		}
	}
	if err := recountMailbox(ctx, tx, account, mailboxUID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE mailboxes SET uidvalidity = ?, uidnext = 0, highestmodseq = 0,
		   updated_modseq = ? WHERE rowid = ? AND account = ?`,
		int64(newUIDValidity), seq, mailboxUID, account)
	return err
}

// PurgeTombstones removes email tombstones older than retention days and
// raises the /changes replay floor so an older sinceState correctly
// degrades to cannotCalculateChanges (NFR-4, FR-J.7).
func (s *Store) PurgeTombstones(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().Add(-retention).UnixMicro()
	return s.tx(ctx, "", true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT account FROM emails WHERE deleted IS NOT NULL AND deleted <= ?`, cutoff)
		if err != nil {
			return err
		}
		var accounts []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				_ = rows.Close()
				return err
			}
			accounts = append(accounts, a)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, account := range accounts {
			// The highest modseq among purged tombstones is the oldest
			// change we can no longer replay.
			var through int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(deleted), 0) FROM emails
				 WHERE account = ? AND deleted IS NOT NULL AND deleted <= ?`,
				account, cutoff).Scan(&through); err != nil {
				return err
			}
			m, err := loadMeta(ctx, tx, account)
			if err != nil {
				return err
			}
			if through > m.PurgedThrough {
				m.PurgedThrough = through
				if err := saveMeta(ctx, tx, account, m); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM emails WHERE account = ? AND deleted IS NOT NULL AND deleted <= ?`,
				account, cutoff); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM email_content WHERE id NOT IN (SELECT id FROM emails)`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM email_msgid WHERE account = ?
				   AND email_id NOT IN (SELECT id FROM emails)`, account); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM imap_uids WHERE account = ?
				   AND email_id NOT IN (SELECT id FROM emails)`, account); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- shared row helpers ---

// folderRow resolves a folder to its mailbox rowid and stored
// UIDVALIDITY.
func folderRow(ctx context.Context, tx *sql.Tx, account, folder string) (mailboxUID int64, uidValidity uint32, err error) {
	var uv int64
	err = tx.QueryRowContext(ctx,
		`SELECT rowid, COALESCE(uidvalidity, 0) FROM mailboxes
		 WHERE account = ? AND name = ? AND deleted IS NULL`, account, folder).Scan(&mailboxUID, &uv)
	if err == sql.ErrNoRows {
		return 0, 0, ErrFolderUnknown
	}
	if err != nil {
		return 0, 0, fmt.Errorf("store: folder %q: %w", folder, err)
	}
	return mailboxUID, uint32(uv), nil
}

func uidEmailID(ctx context.Context, tx *sql.Tx, account, folder string, uidValidity, uid uint32) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT email_id FROM imap_uids
		 WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
		account, folder, uidValidity, uid).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: uid map: %w", err)
	}
	return id, nil
}

func mapUID(ctx context.Context, tx *sql.Tx, account, folder string, uidValidity, uid uint32, emailID string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO imap_uids(account, folder, uidvalidity, uid, email_id)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(account, folder, uidvalidity, uid) DO UPDATE SET email_id = excluded.email_id`,
		account, folder, uidValidity, uid, emailID); err != nil {
		return fmt.Errorf("store: map uid: %w", err)
	}
	return nil
}

// hasLiveMembership reports whether an email currently sits live in a
// mailbox (removed_modseq = 0). The transaction holding it is exclusive,
// so the answer stays valid through the membership write that follows.
func hasLiveMembership(ctx context.Context, tx *sql.Tx, mailboxUID int64, emailID string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_mailbox
		 WHERE mailbox_uid = ? AND email_id = ? AND removed_modseq = 0`,
		mailboxUID, emailID).Scan(&n); err != nil {
		return false, fmt.Errorf("store: membership check: %w", err)
	}
	return n > 0, nil
}

// flagsAgree reports whether an incoming copy's keywords match the
// stored email's, ignoring \Recent (the ephemeral per-session flag).
// Only then can the copies share one JMAP object without the cache
// misrepresenting one of them.
func flagsAgree(ctx context.Context, tx *sql.Tx, account, emailID string, incoming map[string]bool) (bool, error) {
	var kwJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT keywords FROM emails WHERE id = ? AND account = ?`, emailID, account).Scan(&kwJSON); err != nil {
		return false, fmt.Errorf("store: read keywords: %w", err)
	}
	stored := map[string]bool{}
	if err := json.Unmarshal([]byte(kwJSON), &stored); err != nil {
		return false, fmt.Errorf("store: decode keywords: %w", err)
	}
	return sameKeywords(stored, incoming), nil
}

// sameKeywords compares two keyword sets for equality, ignoring \Recent
// and false entries.
func sameKeywords(a, b map[string]bool) bool {
	norm := func(m map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k, v := range m {
			if v && k != `\Recent` {
				out[k] = true
			}
		}
		return out
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for k := range na {
		if !nb[k] {
			return false
		}
	}
	return true
}

// liveEmailBySharedUID finds a live email whose (uidvalidity, uid)
// mapping exists in another folder — the shared-UID dedupe for servers
// whose UIDs identify a message account-wide (FR-S.10). Same-folder hits
// are excluded: uidEmailID already handled those as flag refreshes.
func liveEmailBySharedUID(ctx context.Context, tx *sql.Tx, account string, uidValidity, uid uint32, folder string) string {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT iu.email_id FROM imap_uids iu JOIN emails e ON e.id = iu.email_id
		 WHERE iu.account = ? AND iu.uidvalidity = ? AND iu.uid = ?
		   AND iu.folder <> ? AND e.deleted IS NULL
		 ORDER BY iu.rowid LIMIT 1`,
		account, uidValidity, uid, folder).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// liveEmailByMsgID finds a live email by any of the message's own
// Message-IDs (cross-folder dedupe, FR-S.6 re-backfill).
func liveEmailByMsgID(ctx context.Context, tx *sql.Tx, account string, msgIDs []string) string {
	for _, m := range msgIDs {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		var id string
		err := tx.QueryRowContext(ctx,
			`SELECT e.id FROM email_msgid m JOIN emails e ON e.id = m.email_id
			 WHERE m.account = ? AND m.msgid = ? AND e.deleted IS NULL`,
			account, m).Scan(&id)
		if err == nil {
			return id
		}
	}
	return ""
}

// touchMailboxCounts marks a mailbox's counts as changed at seq.
func touchMailboxCounts(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64, seq int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE mailboxes SET updated_modseq = ? WHERE rowid = ? AND account = ?`,
		seq, mailboxUID, account); err != nil {
		return fmt.Errorf("store: touch mailbox: %w", err)
	}
	return nil
}

// seenIn decodes whether a stored keywords JSON marks $seen.
func seenIn(keywordsJSON string) bool {
	return strings.Contains(keywordsJSON, `"$seen":true`)
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func firstNonEmpty(v []string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// KnownSharedUIDs maps each uid to the live email the account already
// knows under that shared uid, whichever folder mapped it (FR-S.10: on
// Gmail one uid identifies a message account-wide). Unmapped uids are
// simply absent; the caller fetches their headers instead.
func (s *Store) KnownSharedUIDs(ctx context.Context, account string, uidValidity uint32, uids []uint32) (map[uint32]string, error) {
	out := make(map[uint32]string, len(uids))
	const chunk = 200
	for start := 0; start < len(uids); start += chunk {
		end := min(start+chunk, len(uids))
		q := `SELECT iu.uid, iu.email_id FROM imap_uids iu JOIN emails e ON e.id = iu.email_id
		      WHERE iu.account = ? AND iu.uidvalidity = ? AND e.deleted IS NULL
		        AND iu.uid IN (`
		args := []any{account, uidValidity}
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
			return nil, fmt.Errorf("store: known shared uids: %w", err)
		}
		for rows.Next() {
			var uid uint32
			var id string
			if err := rows.Scan(&uid, &id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[uid] = id
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LinkSharedUIDs records membership and the uid mapping for messages the
// account already knows under their shared uid — the Gmail backfill's
// cheap half: headers and keywords are already right (Gmail flags are
// per-message), so only the membership and the folder mapping are
// missing. Uids the account does not know are left alone; the engine
// fetches those with headers instead. The store re-checks every mapping
// inside the transaction, so a mapping that vanished between the
// caller's lookup and here cannot produce a membership with no email.
func (s *Store) LinkSharedUIDs(ctx context.Context, account, folder string, uidValidity uint32, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		mailboxUID, _, err := folderRow(ctx, tx, account, folder)
		if err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		mailboxDirty := false
		for _, uid := range uids {
			id, err := uidEmailID(ctx, tx, account, folder, uidValidity, uid)
			if err != nil {
				return err
			}
			if id != "" {
				continue // already a member of this folder
			}
			if id := liveEmailBySharedUID(ctx, tx, account, uidValidity, uid, folder); id != "" {
				if err := addMembership(ctx, tx, account, mailboxUID, id, seq); err != nil {
					return err
				}
				if err := mapUID(ctx, tx, account, folder, uidValidity, uid, id); err != nil {
					return err
				}
				mailboxDirty = true
			}
		}
		if mailboxDirty {
			if err := touchMailboxCounts(ctx, tx, account, mailboxUID, seq); err != nil {
				return err
			}
		}
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

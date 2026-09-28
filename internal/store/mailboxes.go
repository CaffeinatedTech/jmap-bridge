package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// Folder is one IMAP folder as discovery reports it (FR-S.1). imapdrv
// fills it from LIST + SPECIAL-USE + NAMESPACE + STATUS.
type Folder struct {
	// Name is the full mailbox path with the server's hierarchy
	// separator preserved (FR-M.1: hierarchy exactly as exposed).
	Name string
	// Delim is the hierarchy separator used inside Name.
	Delim rune
	// Role is the JMAP role derived from SPECIAL-USE attributes, or ""
	// when the server assigned none.
	Role string
	// NoSelect marks a hierarchy container the server refuses to select.
	NoSelect bool
	// UIDValidity, UIDNext and HighestModSeq come from STATUS/SELECT.
	UIDValidity   uint32
	UIDNext       uint64
	HighestModSeq uint64
}

// SyncFolders reconciles the account's mailboxes with a fresh discovery
// result: creates new folders, updates roles and hierarchy, recomputes
// sort order, and tombstones folders the server no longer lists (one
// transaction, one modseq, FR-M.1/FR-M.3).
func (s *Store) SyncFolders(ctx context.Context, account string, folders []Folder) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		existing := map[string]*mailboxRow{}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, name, parent_id, role, sort_order, deleted FROM mailboxes WHERE account = ?`, account)
		if err != nil {
			return fmt.Errorf("store: list mailboxes: %w", err)
		}
		for rows.Next() {
			var r mailboxRow
			var parent, role sql.NullString
			var deleted sql.NullInt64
			if err := rows.Scan(&r.ID, &r.Name, &parent, &role, &r.SortOrder, &deleted); err != nil {
				_ = rows.Close()
				return err
			}
			r.ParentID, r.Role = parent.String, role.String
			r.Deleted = deleted.Valid
			existing[r.Name] = &r
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Index the incoming set by path so parents resolve before use.
		incoming := make([]Folder, len(folders))
		copy(incoming, folders)
		incomingByName := make(map[string]Folder, len(incoming))
		for _, f := range incoming {
			incomingByName[f.Name] = f
		}

		// Sort order: role bucket then name, recomputed over the whole
		// set so sibling ordering stays deterministic as folders come
		// and go (FR-M.2 sortOrder).
		sortOrder := folderSortOrder(incoming)

		seen := map[string]bool{}
		for _, f := range incoming {
			seen[f.Name] = true
			parent := parentID(f, incomingByName, existing, sortOrder)
			role := f.Role
			if role == "" && strings.EqualFold(f.Name, "INBOX") {
				role = "inbox"
			}
			mayAdd := !f.NoSelect
			row, ok := existing[f.Name]
			if !ok {
				id, err := mintID(ctx, tx)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO mailboxes(id, account, parent_id, role, name, sort_order,
					   uidvalidity, uidnext, highestmodseq,
					   may_read_items, may_add_items, may_remove_items, may_create_child,
					   may_rename, may_delete,
					   created_modseq, updated_modseq, updated_not_counts_modseq)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1, 1, ?, ?, 0)`,
					id, account, nullStr(parent), nullStr(role), f.Name, sortOrder[f.Name],
					int64(f.UIDValidity), int64(f.UIDNext), int64(f.HighestModSeq),
					boolInt(true), boolInt(mayAdd), boolInt(true),
					seq, seq); err != nil {
					return fmt.Errorf("store: create mailbox %q: %w", f.Name, err)
				}
				existing[f.Name] = &mailboxRow{
					ID: id, Name: f.Name, ParentID: parent,
					Role: role, SortOrder: sortOrder[f.Name],
				}
				continue
			}
			// Update whatever drifted. Counts are untouched here: they
			// move with membership, not with discovery.
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET parent_id = ?, role = ?, sort_order = ?,
				   uidvalidity = ?, uidnext = ?, highestmodseq = ?,
				   may_add_items = ?, deleted = NULL, updated_modseq = ?
				 WHERE id = ? AND account = ?`,
				nullStr(parent), nullStr(role), sortOrder[f.Name],
				int64(f.UIDValidity), int64(f.UIDNext), int64(f.HighestModSeq),
				boolInt(mayAdd), seq, row.ID, account); err != nil {
				return fmt.Errorf("store: update mailbox %q: %w", f.Name, err)
			}
		}

		// Second pass: every row now exists, so hierarchy is resolved
		// regardless of the order LIST returned the folders in.
		for _, f := range incoming {
			row := existing[f.Name]
			if row == nil {
				continue
			}
			parent := parentID(f, incomingByName, existing, sortOrder)
			if parent == row.ParentID {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET parent_id = ? WHERE id = ? AND account = ?`,
				nullStr(parent), row.ID, account); err != nil {
				return fmt.Errorf("store: reparent %q: %w", f.Name, err)
			}
			row.ParentID = parent
		}

		// Folders the server no longer lists: tombstone the mailbox and
		// drop its memberships; mail that lived only there goes with it.
		var vanished []orphan
		for name, row := range existing {
			if row.Deleted || seen[name] {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE mailboxes SET deleted = ?, updated_modseq = ? WHERE id = ? AND account = ?`,
				seq, seq, row.ID, account); err != nil {
				return fmt.Errorf("store: tombstone mailbox %q: %w", name, err)
			}
			mrows, err := tx.QueryContext(ctx,
				`SELECT em.mailbox_uid, em.email_id FROM email_mailbox em
				 WHERE em.account = ? AND em.mailbox_uid = (
				   SELECT rowid FROM mailboxes WHERE id = ? AND account = ?)
				   AND em.removed_modseq = 0`, account, row.ID, account)
			if err != nil {
				return err
			}
			for mrows.Next() {
				var v orphan
				if err := mrows.Scan(&v.uid, &v.id); err != nil {
					_ = mrows.Close()
					return err
				}
				vanished = append(vanished, v)
			}
			_ = mrows.Close()
			if err := mrows.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE email_mailbox SET removed_modseq = ?
				 WHERE account = ? AND mailbox_uid = (
				   SELECT rowid FROM mailboxes WHERE id = ? AND account = ?)
				 AND removed_modseq = 0`,
				seq, account, row.ID, account); err != nil {
				return err
			}
		}
		if len(vanished) > 0 {
			if err := orphanEmails(ctx, tx, account, seq, vanished); err != nil {
				return err
			}
		}
		return bumpMailboxState(ctx, tx, account, seq)
	})
}

// orphan is an (email id, mailbox rowid) pair that just lost a membership.
type orphan struct {
	uid int64
	id  string
}

// orphanEmails tombstones emails that just lost their last live mailbox
// membership and raises the Email state floor when it does.
func orphanEmails(ctx context.Context, tx *sql.Tx, account string, seq int64, candidates []orphan) error {
	touched := false
	for _, c := range candidates {
		var others int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM email_mailbox
			 WHERE email_id = ? AND removed_modseq = 0
			   AND mailbox_uid <> ?`, c.id, c.uid).Scan(&others); err != nil {
			return err
		}
		if others > 0 {
			// Still visible elsewhere: membership removal is the change.
			if _, err := tx.ExecContext(ctx,
				`UPDATE emails SET updated_modseq = ? WHERE id = ? AND account = ?`,
				seq, c.id, account); err != nil {
				return err
			}
			touched = true
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE emails SET deleted = ?, updated_modseq = ? WHERE id = ? AND account = ? AND deleted IS NULL`,
			seq, seq, c.id, account); err != nil {
			return err
		}
		touched = true
	}
	if touched {
		if err := bumpEmailState(ctx, tx, account, seq); err != nil {
			return err
		}
	}
	return nil
}

// mailboxRow is the scan shape shared by discovery and reads.
type mailboxRow struct {
	ID        string
	Name      string
	ParentID  string
	Role      string
	SortOrder int
	Deleted   bool
}

// folderSortOrder assigns each folder a deterministic sort position:
// role bucket (so Inbox leads) then name.
func folderSortOrder(folders []Folder) map[string]int {
	buckets := map[string]int{"inbox": 0, "drafts": 10, "sent": 20, "junk": 30, "trash": 40, "archive": 50}
	byBucket := map[int][]string{}
	for _, f := range folders {
		role := f.Role
		if role == "" && strings.EqualFold(f.Name, "INBOX") {
			role = "inbox"
		}
		b := buckets[role]
		if _, ok := buckets[role]; !ok {
			b = 60
		}
		byBucket[b] = append(byBucket[b], f.Name)
	}
	out := make(map[string]int, len(folders))
	for b, names := range byBucket {
		sort.Strings(names)
		for i, n := range names {
			out[n] = b*1000 + i
		}
	}
	return out
}

// parentID resolves the JMAP parent id for a folder from its path.
func parentID(f Folder, incoming map[string]Folder, existing map[string]*mailboxRow, order map[string]int) string {
	if f.Delim == 0 {
		return ""
	}
	idx := strings.LastIndex(f.Name, string(f.Delim))
	if idx <= 0 {
		return ""
	}
	parentPath := f.Name[:idx]
	if _, ok := incoming[parentPath]; !ok {
		// The parent is not on the server (or not listed): top-level.
		return ""
	}
	if row, ok := existing[parentPath]; ok && !row.Deleted {
		return row.ID
	}
	// Parent exists on the server but not yet in our table — cannot
	// happen with a full LIST, but keep the invariant: unknown parent
	// means top-level rather than a dangling id.
	return ""
}

// Mailboxes implements [jmapapi.Store]: every live mailbox, plus the
// Mailbox type state.
func (s *Store) Mailboxes(ctx context.Context, account string) ([]*jmapapi.Mailbox, string, error) {
	state, err := s.MailboxStateString(ctx, account)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent_id, role, name, sort_order,
		        total_emails, unread_emails, total_threads, unread_threads,
		        may_read_items, may_add_items, may_remove_items,
		        may_create_child, may_rename, may_delete
		 FROM mailboxes WHERE account = ? AND deleted IS NULL
		 ORDER BY sort_order, name`, account)
	if err != nil {
		return nil, "", fmt.Errorf("store: list mailboxes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*jmapapi.Mailbox
	for rows.Next() {
		mb, err := scanMailbox(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return out, state, nil
}

// MailboxesByID implements [jmapapi.Store].
func (s *Store) MailboxesByID(ctx context.Context, account string, ids []string) ([]*jmapapi.Mailbox, string, []string, error) {
	if ids == nil {
		mbs, state, err := s.Mailboxes(ctx, account)
		return mbs, state, nil, err
	}
	state, err := s.MailboxStateString(ctx, account)
	if err != nil {
		return nil, "", nil, err
	}
	byID := map[string]*jmapapi.Mailbox{}
	args := make([]any, 0, len(ids)+1)
	args = append(args, account)
	ph := make([]string, 0, len(ids))
	for _, id := range ids {
		ph = append(ph, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent_id, role, name, sort_order,
		        total_emails, unread_emails, total_threads, unread_threads,
		        may_read_items, may_add_items, may_remove_items,
		        may_create_child, may_rename, may_delete
		 FROM mailboxes WHERE account = ? AND deleted IS NULL
		   AND id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, "", nil, fmt.Errorf("store: get mailboxes: %w", err)
	}
	for rows.Next() {
		mb, err := scanMailbox(rows)
		if err != nil {
			_ = rows.Close()
			return nil, "", nil, err
		}
		byID[mb.ID] = mb
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", nil, err
	}
	var out []*jmapapi.Mailbox
	var notFound []string
	for _, id := range ids {
		if mb, ok := byID[id]; ok {
			out = append(out, mb)
		} else {
			notFound = append(notFound, id)
		}
	}
	return out, state, notFound, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMailbox(rows scanner) (*jmapapi.Mailbox, error) {
	var (
		mb          jmapapi.Mailbox
		parent, rol sql.NullString
	)
	err := rows.Scan(&mb.ID, &parent, &rol, &mb.Name, &mb.SortOrder,
		&mb.TotalEmails, &mb.UnreadEmails, &mb.TotalThreads, &mb.UnreadThreads,
		&mb.MayRead, &mb.MayAddItems, &mb.MayRemoveItems,
		&mb.MayCreateChild, &mb.MayRename, &mb.MayDelete)
	if err != nil {
		return nil, fmt.Errorf("store: scan mailbox: %w", err)
	}
	mb.ParentID, mb.Role = parent.String, rol.String
	return &mb, nil
}

// recountMailbox recomputes a mailbox's four counters from membership.
// It deliberately does not bump modseq — callers that let counts drift
// decide whether the recount is a change worth announcing (discovery and
// the tests both need a side-effect-free reference recount).
func recountMailbox(ctx context.Context, tx *sql.Tx, account string, mailboxUID int64) error {
	const live = `FROM email_mailbox em JOIN emails e ON e.id = em.email_id
		 WHERE em.mailbox_uid = ? AND em.removed_modseq = 0 AND e.deleted IS NULL`
	const unseen = ` AND COALESCE(json_extract(e.keywords, '$."$seen"'), 0) <> 1`
	_, err := tx.ExecContext(ctx,
		`UPDATE mailboxes SET
		   total_emails   = (SELECT COUNT(*) `+live+`),
		   unread_emails  = (SELECT COUNT(*) `+live+unseen+`),
		   total_threads  = (SELECT COUNT(DISTINCT thread_id) `+live+`),
		   unread_threads = (SELECT COUNT(DISTINCT thread_id) `+live+unseen+`)
		 WHERE rowid = ? AND account = ?`,
		mailboxUID, mailboxUID, mailboxUID, mailboxUID, mailboxUID, account)
	if err != nil {
		return fmt.Errorf("store: recount mailbox: %w", err)
	}
	return nil
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mintID allocates the next globally unique JMAP id inside tx.
func mintID(ctx context.Context, tx *sql.Tx) (string, error) {
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return "", err
	}
	return newID(time.Now(), seq), nil
}

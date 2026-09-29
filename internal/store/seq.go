package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// b62Alphabet is digits-then-letters so lexicographic order over fixed
// width ids matches numeric order (PLAN §4.1: id order approximates date
// order for Email/query fast paths).
const b62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const (
	idTimeWidth = 6 // base62 unix seconds, ample past year 3000
	idSeqWidth  = 7 // base62 sequence numbers per account
)

// accountMeta is the per-account bookkeeping kept in the sync_state row
// with scope 'account' (PLAN §4): the state-string high-water marks that
// keep type states monotonic even after tombstone retention expires
// (FR-J.7), and the replay floor for /changes (NFR-4).
type accountMeta struct {
	EmailState    int64 `json:"emailState"`
	MailboxState  int64 `json:"mailboxState"`
	PurgedThrough int64 `json:"purgedThrough"`
}

// nextSeq allocates the next sequence number inside tx. The counter is
// process-global rather than per-account so that ids — which embed it —
// can never collide across accounts on the same bridge (PLAN §4
// bookkeeping). Per-account monotonicity of type states is preserved:
// every mutation of an account raises that account's state floors with
// the same global value, and a value shared with another account is
// still strictly greater than anything the account had before.
func nextSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO counters(name, value) VALUES ('seq', 1)
		 ON CONFLICT(name) DO UPDATE SET value = value + 1`); err != nil {
		return 0, fmt.Errorf("store: allocate seq: %w", err)
	}
	var v int64
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE name = 'seq'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read seq: %w", err)
	}
	return v, nil
}

// newID mints a 13 char base62 id: a62(unix seconds) ++ a62(seq).
func newID(now time.Time, seq int64) string {
	return encodeB62(now.Unix(), idTimeWidth) + encodeB62(seq, idSeqWidth)
}

func encodeB62(v int64, width int) string {
	if v < 0 {
		v = 0
	}
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = b62Alphabet[v%62]
		v /= 62
	}
	return string(out)
}

// loadMeta reads the account bookkeeping row.
func loadMeta(ctx context.Context, q queryer, account string) (accountMeta, error) {
	var raw sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT sync_token FROM sync_state WHERE account = ? AND scope = 'account'`,
		account).Scan(&raw)
	var m accountMeta
	if err == sql.ErrNoRows {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("store: read account meta: %w", err)
	}
	if raw.Valid && raw.String != "" {
		if err := jsonUnmarshal(raw.String, &m); err != nil {
			return m, fmt.Errorf("store: decode account meta: %w", err)
		}
	}
	return m, nil
}

// saveMeta writes the account bookkeeping row.
func saveMeta(ctx context.Context, tx *sql.Tx, account string, m accountMeta) error {
	raw, err := jsonMarshal(m)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sync_state(account, scope, highestmodseq, sync_token)
		 VALUES (?, 'account', ?, ?)
		 ON CONFLICT(account, scope) DO UPDATE SET highestmodseq = excluded.highestmodseq,
                                                   sync_token = excluded.sync_token`,
		account, m.EmailState, string(raw)); err != nil {
		return fmt.Errorf("store: write account meta: %w", err)
	}
	return nil
}

// bumpMailboxState raises the Mailbox state floor to seq.
func bumpMailboxState(ctx context.Context, tx *sql.Tx, account string, seq int64) error {
	m, err := loadMeta(ctx, tx, account)
	if err != nil {
		return err
	}
	if seq > m.MailboxState {
		m.MailboxState = seq
	}
	return saveMeta(ctx, tx, account, m)
}

// bumpEmailState raises the Email (and therefore Thread, and query
// counter — PLAN §4.1 companions) state floor to seq.
func bumpEmailState(ctx context.Context, tx *sql.Tx, account string, seq int64) error {
	m, err := loadMeta(ctx, tx, account)
	if err != nil {
		return err
	}
	if seq > m.EmailState {
		m.EmailState = seq
	}
	return saveMeta(ctx, tx, account, m)
}

// MintID allocates a fresh object id for something the bridge does not
// persist: EmailSubmission ids, which RFC 8621 §7 allows a server to
// destroy as soon as the message has been relayed. The global seq
// counter still allocates it, so such an id can never collide with a
// stored one.
func (s *Store) MintID(ctx context.Context) (string, error) {
	var id string
	err := s.tx(ctx, "", false, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		id = newID(time.Now(), seq)
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EmailStateString returns the Email type state (FR-J.7, FR-M.4). The
// Thread state is the same value: any email change can change threads
// (PLAN §4.1 companions).
func (s *Store) EmailStateString(ctx context.Context, account string) (string, error) {
	m, err := loadMeta(ctx, s.db, account)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(m.EmailState, 10), nil
}

// States implements [jmapapi.Store]: the pushable type states for one
// account (FR-J.8).
func (s *Store) States(ctx context.Context, account string) (map[string]string, error) {
	email, err := s.EmailStateString(ctx, account)
	if err != nil {
		return nil, err
	}
	mailbox, err := s.MailboxStateString(ctx, account)
	if err != nil {
		return nil, err
	}
	// Thread state IS the Email state (PLAN §4.1 companions).
	return map[string]string{"Mailbox": mailbox, "Email": email, "Thread": email}, nil
}

// MailboxStateString returns the Mailbox type state.
func (s *Store) MailboxStateString(ctx context.Context, account string) (string, error) {
	m, err := loadMeta(ctx, s.db, account)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(m.MailboxState, 10), nil
}

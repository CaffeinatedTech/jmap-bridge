package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// The search index (FR-X.1, FR-X.2) lives in the email_search FTS5
// table: subject/sender/recipient are indexed at ingest from the
// header mirrors, body fills in as messages hydrate (FR-X.5). Rows are
// one per live email; tombstoning deletes them so the index never
// outlives the messages it describes.
//
// The fts scope in sync_state holds a modseq of its own, bumped on
// every index write. Email/query folds it into queryState (FR-X.7):
// hydration adds body tokens — a possible result change — without
// moving the Email state, which must stay quiet so clients do not
// refetch bodies they already hold.

const ftsScope = "fts"

// ftsInsert writes a brand-new index row inside tx and records its
// rowid on the email's content row — the pointer hydration needs to
// replace the body column without a scan (email_id is UNINDEXED in
// FTS5, so locating a row by it means reading the whole table).
func ftsInsert(ctx context.Context, tx *sql.Tx, emailID, subject, sender, recipient, body string) error {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO email_search(email_id, subject, sender, recipient, body)
		 VALUES (?, ?, ?, ?, ?)`,
		emailID, subject, sender, recipient, body)
	if err != nil {
		return fmt.Errorf("store: fts insert: %w", err)
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: fts rowid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_content SET fts_rowid = ? WHERE id = ?`, rowid, emailID); err != nil {
		return fmt.Errorf("store: fts rowid store: %w", err)
	}
	return nil
}

// ftsReplace rewrites one email's index row (hydration fills the body
// column). The previous row is found through the stored rowid pointer;
// a row without one (should not exist — every writer records it) falls
// back to the email_id scan.
func ftsReplace(ctx context.Context, tx *sql.Tx, emailID, subject, sender, recipient, body string) error {
	var rowid sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT fts_rowid FROM email_content WHERE id = ?`, emailID).Scan(&rowid); err != nil {
		return fmt.Errorf("store: fts rowid read: %w", err)
	}
	if rowid.Valid {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM email_search WHERE rowid = ?`, rowid.Int64); err != nil {
			return fmt.Errorf("store: fts clear: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`DELETE FROM email_search WHERE email_id = ?`, emailID); err != nil {
		return fmt.Errorf("store: fts clear scan: %w", err)
	}
	return ftsInsert(ctx, tx, emailID, subject, sender, recipient, body)
}

// ftsDelete drops an email's index row (tombstones, purges) through
// its rowid pointer.
func ftsDelete(ctx context.Context, tx *sql.Tx, emailID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM email_search WHERE rowid = (SELECT fts_rowid FROM email_content WHERE id = ?)`,
		emailID); err != nil {
		return fmt.Errorf("store: fts delete: %w", err)
	}
	return nil
}

// bumpFTSState moves the index modseq inside the caller's transaction.
func bumpFTSState(ctx context.Context, tx *sql.Tx, account string, seq int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sync_state(account, scope, highestmodseq) VALUES (?, ?, ?)
		 ON CONFLICT(account, scope) DO UPDATE SET highestmodseq = excluded.highestmodseq
		 WHERE excluded.highestmodseq > sync_state.highestmodseq`,
		account, ftsScope, seq)
	if err != nil {
		return fmt.Errorf("store: fts state: %w", err)
	}
	return nil
}

// FTSState reads the index modseq (0 when never written). QueryEmails
// folds it into the query counter.
func (s *Store) FTSState(ctx context.Context, account string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(highestmodseq, 0) FROM sync_state WHERE account = ? AND scope = ?`,
		account, ftsScope).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read fts state: %w", err)
	}
	return seq, nil
}

// indexBodyText reduces a hydration result to indexable text: the
// text/plain parts when there are any, otherwise the HTML parts with
// markup stripped (a tag soup index would match "div" everywhere).
func indexBodyText(structure string, values map[string]string) string {
	textParts, htmlParts, _ := analyzeStructure(json.RawMessage(structure))
	var b strings.Builder
	for _, pid := range textParts {
		b.WriteString(values[pid])
		b.WriteByte(' ')
	}
	if b.Len() > 0 {
		return b.String()
	}
	for _, pid := range htmlParts {
		b.WriteString(stripHTML(values[pid]))
		b.WriteByte(' ')
	}
	return b.String()
}

// stripHTML drops tags and collapses the whitespace they leave behind.
// Entities stay verbatim: &amp; indexing as "amp" is noise, not a
// wrong answer, and a full entity decoder is out of proportion here.
func stripHTML(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ftsMatch translates the text-ish filters into one FTS5 MATCH
// expression: fields AND together, words within a field AND together,
// and a word matches subject as a whole token, sender/recipient as a
// token prefix (FR-X.2: prefix/domain matching is for from/to), and
// body as a whole token. Subject is whole-token like text so that a
// partial word returns nothing, letting clients with substring-search
// fallback (jmap-tui) degrade to their own scan; a server-side subject
// prefix would shadow that path and answer partially.
//
// A text filter whose words tokenize to nothing gets a sentinel term:
// it must match nothing, never everything.
func ftsMatch(f jmapapi.EmailFilter) (string, bool) {
	var terms []string
	for _, w := range words(f.Subject) {
		terms = append(terms, "subject:"+ftsExact(w))
	}
	for _, w := range words(f.From) {
		terms = append(terms, ftsPrefix("sender", w))
	}
	for _, w := range words(f.To) {
		terms = append(terms, ftsPrefix("recipient", w))
	}
	if strings.TrimSpace(f.Text) != "" {
		textWords := words(f.Text)
		if len(textWords) == 0 {
			terms = append(terms, ftsExact("ftsnomatchsentinel"))
		} else {
			// Whole tokens on every column: exact term lookups hit
			// the FTS index directly, while a prefix alternative
			// would scan each column's full term dictionary on a
			// 100k corpus. Prefix matching stays where FR-X.2 asks
			// for it — the from/to filters above.
			for _, w := range textWords {
				terms = append(terms,
					"(subject:"+ftsExact(w)+" OR sender:"+ftsExact(w)+
						" OR recipient:"+ftsExact(w)+" OR body:"+ftsExact(w)+")")
			}
		}
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " AND "), true
}

// ftsPrefix renders a quoted prefix term for one column; col "" omits
// the column filter (the OR-alternatives above already name columns).
func ftsPrefix(col, word string) string {
	if col == "" {
		return `"` + ftsQuote(word) + `"*`
	}
	return col + `:` + `"` + ftsQuote(word) + `"*`
}

// ftsExact renders a quoted whole-token term.
func ftsExact(word string) string {
	return `"` + ftsQuote(word) + `"`
}

// ftsQuote makes a word safe inside an FTS5 string term.
func ftsQuote(w string) string {
	return strings.ReplaceAll(w, `"`, `""`)
}

// words splits a filter value into matchable tokens: whitespace-split,
// then kept only when they contain a letter or digit (a punctuation
// blob would produce a zero-token phrase, which FTS5 rejects).
func words(s string) []string {
	fields := strings.Fields(s)
	out := fields[:0]
	for _, f := range fields {
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, strings.ToLower(f))
				break
			}
		}
	}
	return out
}

// buildFTSIndex fills the index for emails stored before the v5
// migration (their FTS rows never existed). It runs once per database:
// the marker row in sync_state records completion, so restarts never
// rebuild (FR-X.1: incremental, never a full rebuild on restart).
func (s *Store) buildFTSIndex(ctx context.Context) error {
	var done string
	err := s.db.QueryRowContext(ctx,
		`SELECT sync_token FROM sync_state WHERE account = '' AND scope = ?`,
		ftsScope+":built").Scan(&done)
	if err == nil && done == "done" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("store: fts marker: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.subject_l, c.from_l, c.to_l FROM email_content c
		 JOIN emails e ON e.id = c.id
		 WHERE e.deleted IS NULL
		   AND c.id NOT IN (SELECT email_id FROM email_search)`)
	if err != nil {
		return fmt.Errorf("store: fts backfill scan: %w", err)
	}
	type doc struct{ id, subject, sender, recipient string }
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.subject, &d.sender, &d.recipient); err != nil {
			_ = rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(docs); start += 500 {
		end := min(start+500, len(docs))
		if err := s.tx(ctx, "", false, func(tx *sql.Tx) error {
			for _, d := range docs[start:end] {
				if err := ftsInsert(ctx, tx, d.id, d.subject, d.sender, d.recipient, ""); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_state(account, scope, sync_token) VALUES ('', ?, 'done')
		 ON CONFLICT(account, scope) DO UPDATE SET sync_token = 'done'`,
		ftsScope+":built"); err != nil {
		return fmt.Errorf("store: fts marker: %w", err)
	}
	s.log.Info("store: search index backfilled", "emails", len(docs))
	return nil
}

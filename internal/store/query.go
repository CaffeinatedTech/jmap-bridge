package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// QueryEmails implements [jmapapi.Store]: filters narrow an index-backed
// scan (inMailbox → the live membership index, dates →
// emails.received_at, keyword → the keywords JSON), the ordered id list
// is collapsed and paged exactly like the M0 fixture (anchor clamps to
// the end, collapseThreads keeps the first exemplar in sort order), and
// the counter is the Email state so queryState moves when — and only
// when — email data moves (FR-M.5, FR-X.7).
func (s *Store) QueryEmails(ctx context.Context, account string, q jmapapi.EmailQuery) ([]string, int, int, string, error) {
	counter, err := s.EmailStateString(ctx, account)
	if err != nil {
		return nil, 0, 0, "", err
	}

	var (
		join  strings.Builder
		where strings.Builder
		args  []any
	)
	join.WriteString(` JOIN email_content c ON c.id = e.id`)
	where.WriteString(` WHERE e.account = ? AND e.deleted IS NULL`)
	args = append(args, account)

	if q.Filter.InMailbox != "" {
		var mailboxUID int64
		err := s.db.QueryRowContext(ctx,
			`SELECT rowid FROM mailboxes WHERE account = ? AND id = ? AND deleted IS NULL`,
			account, q.Filter.InMailbox).Scan(&mailboxUID)
		if err == sql.ErrNoRows {
			return []string{}, 0, 0, counter, nil // unknown mailbox matches nothing
		}
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: query mailbox: %w", err)
		}
		// Prepend the membership join: argument order follows FROM order.
		joinStr := ` JOIN email_mailbox em ON em.email_id = e.id AND em.removed_modseq = 0 AND em.mailbox_uid = ?` + join.String()
		join.Reset()
		join.WriteString(joinStr)
		args = append([]any{mailboxUID}, args...)
	}

	f := q.Filter
	if f.After != nil {
		where.WriteString(` AND e.received_at > ?`)
		args = append(args, f.After.UnixMicro())
	}
	if f.Before != nil {
		where.WriteString(` AND e.received_at < ?`)
		args = append(args, f.Before.UnixMicro())
	}
	if f.HasAttachment != nil {
		where.WriteString(` AND e.has_attachment = ?`)
		args = append(args, boolInt(*f.HasAttachment))
	}
	if f.HasKeyword != "" {
		where.WriteString(` AND json_extract(e.keywords, '$."` + escapeJSONPath(f.HasKeyword) + `"') = 1`)
	}
	if f.Subject != "" {
		for _, w := range strings.Fields(strings.ToLower(f.Subject)) {
			where.WriteString(` AND c.subject_l LIKE ? ESCAPE '\'`)
			args = append(args, likeArg(w))
		}
	}
	if f.From != "" {
		for _, w := range strings.Fields(strings.ToLower(f.From)) {
			where.WriteString(` AND c.from_l LIKE ? ESCAPE '\'`)
			args = append(args, likeArg(w))
		}
	}
	if f.To != "" {
		for _, w := range strings.Fields(strings.ToLower(f.To)) {
			where.WriteString(` AND c.to_l LIKE ? ESCAPE '\'`)
			args = append(args, likeArg(w))
		}
	}
	if f.Text != "" {
		for _, w := range strings.Fields(strings.ToLower(f.Text)) {
			like := likeArg(w)
			where.WriteString(` AND (c.subject_l LIKE ? ESCAPE '\' OR c.from_l LIKE ? ESCAPE '\' OR c.to_l LIKE ? ESCAPE '\')`)
			args = append(args, like, like, like)
		}
	}

	keyExpr, dir := "e.received_at", "DESC"
	asc := false
	if len(q.Sort) > 0 {
		asc = q.Sort[0].Ascending
		switch q.Sort[0].Property {
		case "receivedAt":
			keyExpr = "e.received_at"
		case "subject":
			keyExpr = "c.subject_l"
		case "from":
			keyExpr = "c.from_l"
		case "size":
			keyExpr = "e.size"
		case "hasAttachment":
			keyExpr = "e.has_attachment"
		default:
			keyExpr = "e.received_at"
		}
		if asc {
			dir = "ASC"
		}
	}

	query := `SELECT e.id, e.thread_id FROM emails e` + join.String() + where.String() +
		` ORDER BY ` + keyExpr + ` ` + dir + `, e.id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("store: query emails: %w", err)
	}
	type hit struct{ id, thread string }
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.thread); err != nil {
			_ = rows.Close()
			return nil, 0, 0, "", err
		}
		hits = append(hits, h)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, 0, "", err
	}

	if q.CollapseThreads {
		seen := map[string]bool{}
		kept := hits[:0]
		for _, h := range hits {
			if seen[h.thread] {
				continue
			}
			seen[h.thread] = true
			kept = append(kept, h)
		}
		hits = kept
	}

	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	position, window := paginateIDs(ids, q.Anchor, q.AnchorOffset, q.Position, q.Limit)
	s.log.Debug("store: query",
		"mailbox", q.Filter.InMailbox, "collapse", q.CollapseThreads,
		"position", q.Position, "limit", q.Limit,
		"hits", len(ids), "window", len(window))
	return window, position, len(ids), counter, nil
}

// paginateIDs mirrors the M0 fixture's window maths, anchor included
// (jmap-tui's window repair relies on the clamp-to-end behaviour).
func paginateIDs(ids []string, anchor string, anchorOffset, position, limit int) (int, []string) {
	if anchor != "" {
		position = len(ids)
		for i, id := range ids {
			if id == anchor {
				position = i + anchorOffset
				break
			}
		}
	}
	if position < 0 {
		position = 0
	}
	if position > len(ids) {
		position = len(ids)
	}
	end := len(ids)
	if limit > 0 && position+limit < end {
		end = position + limit
	}
	window := make([]string, end-position)
	copy(window, ids[position:end])
	return position, window
}

// headerJSON is the canonical header subset convert writes into
// email_content.headers; store reads it back into the JMAP view.
type headerJSON struct {
	Subject string            `json:"subject,omitempty"`
	From    []jmapapi.Address `json:"from,omitempty"`
	To      []jmapapi.Address `json:"to,omitempty"`
	Cc      []jmapapi.Address `json:"cc,omitempty"`
	Bcc     []jmapapi.Address `json:"bcc,omitempty"`
	ReplyTo []jmapapi.Address `json:"replyTo,omitempty"`
}

// EmailsByID implements [jmapapi.Store]. When wantBodies is set the
// Ensure hook hydrates first (FR-M.4, FR-S.8); previews are filled the
// same way for any email that still lacks one (PLAN §5). Unknown and
// tombstoned ids land in notFound.
func (s *Store) EmailsByID(ctx context.Context, account string, ids []string, wantBodies bool) ([]*jmapapi.Email, string, []string, error) {
	target := ids
	if target == nil {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id FROM emails WHERE account = ? AND deleted IS NULL ORDER BY received_at`, account)
		if err != nil {
			return nil, "", nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, "", nil, err
			}
			target = append(target, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, "", nil, err
		}
	}
	if len(target) > 0 && s.Ensure != nil {
		needPreview, needBody, err := s.needsEnsuring(ctx, account, target, wantBodies)
		if err != nil {
			return nil, "", nil, err
		}
		if len(needPreview) > 0 || len(needBody) > 0 {
			if err := s.Ensure(ctx, account, needPreview, needBody); err != nil {
				// Serve what the cache has: a backend hiccup must not turn
				// a read into a 500 (the next request retries).
				s.log.Warn("store: ensure failed", "account", account, "err", err)
			}
		}
	}
	return s.loadEmails(ctx, account, target)
}

// needsEnsuring splits the requested ids into "preview missing" and
// "body missing (and wanted)".
func (s *Store) needsEnsuring(ctx context.Context, account string, ids []string, wantBodies bool) (preview, body []string, err error) {
	ph, args := idPlaceholders(ids)
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.preview, c.hydrated_at FROM emails e
		 JOIN email_content c ON c.id = e.id
		 WHERE e.account = ? AND e.deleted IS NULL AND e.id IN (`+ph+`)`,
		append([]any{account}, args...)...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: scan for ensure: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, prev string
		var hydrated sql.NullInt64
		if err := rows.Scan(&id, &prev, &hydrated); err != nil {
			return nil, nil, err
		}
		if !hydrated.Valid {
			if wantBodies {
				body = append(body, id)
				continue
			}
			if prev == "" {
				preview = append(preview, id)
			}
		}
	}
	return preview, body, rows.Err()
}

// loadEmails reads the final rows, joining membership in one query.
func (s *Store) loadEmails(ctx context.Context, account string, ids []string) ([]*jmapapi.Email, string, []string, error) {
	state, err := s.EmailStateString(ctx, account)
	if err != nil {
		return nil, "", nil, err
	}
	byID := map[string]*jmapapi.Email{}
	var notFound []string
	if len(ids) > 0 {
		ph, args := idPlaceholders(ids)
		rows, err := s.db.QueryContext(ctx,
			`SELECT e.id, e.thread_id, e.keywords, e.received_at, e.size, e.has_attachment,
			        e.preview, c.headers, c.message_ids, c.in_reply_to, c."references",
			        c.body_structure, c.body_values, c.hydrated_at, c.raw_blob_id
			 FROM emails e JOIN email_content c ON c.id = e.id
			 WHERE e.account = ? AND e.deleted IS NULL AND e.id IN (`+ph+`)`,
			append([]any{account}, args...)...)
		if err != nil {
			return nil, "", nil, fmt.Errorf("store: get emails: %w", err)
		}
		for rows.Next() {
			em, err := scanEmail(rows)
			if err != nil {
				_ = rows.Close()
				return nil, "", nil, err
			}
			byID[em.ID] = em
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, "", nil, err
		}

		// Mailbox ids for the whole set in one membership query.
		if len(byID) > 0 {
			live := make([]string, 0, len(byID))
			for id := range byID {
				live = append(live, id)
			}
			ph2, args2 := idPlaceholders(live)
			mrows, err := s.db.QueryContext(ctx,
				`SELECT em.email_id, m.id FROM email_mailbox em
				 JOIN mailboxes m ON m.rowid = em.mailbox_uid
				 WHERE m.account = ? AND em.removed_modseq = 0
				   AND m.deleted IS NULL AND em.email_id IN (`+ph2+`)`,
				append([]any{account}, args2...)...)
			if err != nil {
				return nil, "", nil, err
			}
			for mrows.Next() {
				var emailID, mailboxID string
				if err := mrows.Scan(&emailID, &mailboxID); err != nil {
					_ = mrows.Close()
					return nil, "", nil, err
				}
				if em, ok := byID[emailID]; ok {
					em.MailboxIDs = append(em.MailboxIDs, mailboxID)
				}
			}
			_ = mrows.Close()
			if err := mrows.Err(); err != nil {
				return nil, "", nil, err
			}
		}
	}

	out := make([]*jmapapi.Email, 0, len(ids))
	for _, id := range ids {
		em, ok := byID[id]
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		out = append(out, em)
	}
	return out, state, notFound, nil
}

func scanEmail(rows *sql.Rows) (*jmapapi.Email, error) {
	var (
		em        jmapapi.Email
		kwJSON    string
		headers   string
		msgIDs    sql.NullString
		inReply   sql.NullString
		refs      sql.NullString
		structure string
		values    string
		hydrated  sql.NullInt64
		rawBlob   sql.NullString
		received  int64
		size      int64
		hasAtt    int
		preview   string
	)
	err := rows.Scan(&em.ID, &em.ThreadID, &kwJSON, &received, &size, &hasAtt,
		&preview, &headers, &msgIDs, &inReply, &refs, &structure, &values, &hydrated, &rawBlob)
	if err != nil {
		return nil, fmt.Errorf("store: scan email: %w", err)
	}
	em.ReceivedAt = time.UnixMicro(received)
	em.Size = size
	em.HasAttachment = hasAtt != 0
	em.Preview = preview
	if rawBlob.Valid {
		em.BlobID = rawBlob.String
	}
	if err := json.Unmarshal([]byte(kwJSON), &em.Keywords); err != nil {
		return nil, fmt.Errorf("store: decode keywords: %w", err)
	}
	var h headerJSON
	if err := json.Unmarshal([]byte(headers), &h); err != nil {
		return nil, fmt.Errorf("store: decode headers: %w", err)
	}
	em.Subject, em.From, em.To, em.Cc, em.Bcc, em.ReplyTo = h.Subject, h.From, h.To, h.Cc, h.Bcc, h.ReplyTo
	em.MessageID = decodeStringSlice(msgIDs)
	em.InReplyTo = decodeStringSlice(inReply)
	em.References = decodeStringSlice(refs)
	if structure != "" && structure != "{}" {
		em.Structure = json.RawMessage(structure)
		textParts, htmlParts, attachments := analyzeStructure(em.Structure)
		em.TextParts, em.HTMLParts, em.Attachments = textParts, htmlParts, attachments
	}
	if values != "" && values != "{}" {
		if err := json.Unmarshal([]byte(values), &em.BodyValues); err != nil {
			return nil, fmt.Errorf("store: decode body values: %w", err)
		}
	}
	return &em, nil
}

func decodeStringSlice(ns sql.NullString) []string {
	if !ns.Valid || ns.String == "" || ns.String == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(ns.String), &out); err != nil {
		return nil
	}
	return out
}

func idPlaceholders(ids []string) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		ph = append(ph, "?")
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}

// escapeJSONPath keeps a keyword inside a JSON path literal.
func escapeJSONPath(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

// likeArg lowercases and escapes a LIKE needle.
func likeArg(word string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(word) + "%"
}

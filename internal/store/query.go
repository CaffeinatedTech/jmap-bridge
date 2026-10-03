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

// QueryEmails implements [jmapapi.Store]: filters narrow either the
// live-membership index (inMailbox → email_mailbox, dates →
// emails.received_at, keyword → the keywords JSON) or the FTS5 search
// index (text/from/to/subject → email_search, FR-X.2); the ordered id
// list is collapsed and paged like the M0 fixture (an unknown anchor is
// anchorNotFound, collapseThreads keeps the first exemplar in sort
// order), and the counter folds the Email state with the search-index
// state so queryState moves when — and only when — results may have
// changed, hydration included (FR-X.7).
func (s *Store) QueryEmails(ctx context.Context, account string, q jmapapi.EmailQuery) ([]string, int, int, string, error) {
	counter, err := s.EmailStateString(ctx, account)
	if err != nil {
		return nil, 0, 0, "", err
	}
	fts, err := s.FTSState(ctx, account)
	if err != nil {
		return nil, 0, 0, "", err
	}
	counter = fmt.Sprintf("%s:%d", counter, fts)

	// Scope clauses shared by the query and the backfill candidate
	// scan (the scan omits the text clause: an unhydrated message
	// cannot match body tokens yet — that is the point of backfill).
	type clause struct {
		sql  string
		args []any
	}
	var (
		memberJoin string // mailbox membership, with its own arg first
		memberArg  any
		filters    []clause
	)
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
		memberJoin = ` JOIN email_mailbox em ON em.email_id = e.id AND em.removed_modseq = 0 AND em.mailbox_uid = ?`
		memberArg = mailboxUID
	}

	f := q.Filter
	if f.After != nil {
		filters = append(filters, clause{` AND e.received_at > ?`, []any{f.After.UnixMicro()}})
	}
	if f.Before != nil {
		filters = append(filters, clause{` AND e.received_at < ?`, []any{f.Before.UnixMicro()}})
	}
	if f.HasAttachment != nil {
		filters = append(filters, clause{` AND e.has_attachment = ?`, []any{boolInt(*f.HasAttachment)}})
	}
	if f.HasKeyword != "" {
		filters = append(filters, clause{
			` AND json_extract(e.keywords, '$."` + escapeJSONPath(f.HasKeyword) + `"') = 1`, nil,
		})
	}

	// Header-ish text filters route through the FTS index: every live
	// email has a row (ingest writes it, open() backfills rows that
	// predate the index), and token/prefix matching is what FR-X.2
	// asks for.
	match, hasMatch := ftsMatch(f)
	orderBy := " ORDER BY " + sortClause(q.Sort)

	// The covering path serves the hot browse shape — one mailbox, no
	// text/keyword filters, receivedAt ordering — entirely from the
	// membership table's denormalised columns (schema v6). It is what
	// keeps a 100k-message folder inside NFR-1: no per-row lookup back
	// into emails is needed to order, count or collapse. Anything else
	// falls back to the joined form.
	covering := q.Filter.InMailbox != "" && !hasMatch && f.HasKeyword == ""
	for _, so := range q.Sort {
		if so.Property != "receivedAt" {
			covering = false
		}
	}

	// A bounded page without collapse never needs the full ordered
	// list: total, position and the window all come from the covering
	// index (and the maintained mailbox counters) without touching
	// 100k rows in Go. Unlimited queries and collapseThreads keep the
	// streaming evaluation below — collapse must see every row to pick
	// each thread's first exemplar in sort order (FR-X.4).
	if covering && q.Limit > 0 && memberArg != nil {
		if q.CollapseThreads {
			return s.queryEmailsCollapsedPage(ctx, q, f, memberArg.(int64), counter)
		}
		return s.queryEmailsCoveredPage(ctx, q, f, memberArg.(int64), counter)
	}

	scope := func() (string, []any) {
		if covering {
			where := ` WHERE em.mailbox_uid = ? AND em.removed_modseq = 0`
			args := []any{memberArg}
			if f.After != nil {
				where += ` AND em.received_at > ?`
				args = append(args, f.After.UnixMicro())
			}
			if f.Before != nil {
				where += ` AND em.received_at < ?`
				args = append(args, f.Before.UnixMicro())
			}
			if f.HasAttachment != nil {
				where += ` AND em.has_attachment = ?`
				args = append(args, boolInt(*f.HasAttachment))
			}
			dir := "DESC"
			if len(q.Sort) > 0 && q.Sort[0].Ascending {
				dir = "ASC"
			}
			return `SELECT em.email_id, em.thread_id FROM email_mailbox em` + where +
				` ORDER BY em.received_at ` + dir + `, em.email_id ASC`, args
		}
		var (
			from  string
			where string
			args  []any
		)
		if hasMatch {
			// FTS5 wants the match table outermost: the MATCH
			// constraint drives the plan.
			from = ` FROM email_search fts
				  JOIN emails e ON e.id = fts.email_id
				  JOIN email_content c ON c.id = e.id` + memberJoin
			where = ` WHERE email_search MATCH ? AND e.account = ? AND e.deleted IS NULL`
			if memberArg != nil {
				args = append(args, memberArg)
			}
			args = append(args, match, account)
		} else {
			from = ` FROM emails e
				  JOIN email_content c ON c.id = e.id` + memberJoin
			where = ` WHERE e.account = ? AND e.deleted IS NULL`
			if memberArg != nil {
				args = append(args, memberArg)
			}
			args = append(args, account)
		}
		for _, cl := range filters {
			where += cl.sql
			args = append(args, cl.args...)
		}
		return `SELECT e.id, e.thread_id` + from + where, args
	}

	sqlText, args := scope()
	if !covering {
		// The covering path carries its own em-based ORDER BY; the
		// joined paths take the shared one.
		sqlText += orderBy
	}
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
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

	// Search-driven backfill (FR-X.5): header matches answered above;
	// unhydrated messages in the same scope hydrate in the background
	// and their body tokens arrive with a queryState bump, so clients
	// see late matches via /changes or SSE. Fire-and-forget: result
	// correctness never depends on it finishing. The scan drives from
	// the unhydrated index, so it stays cheap as the pool shrinks —
	// the opposite shape of a per-query membership sweep.
	if f.Text != "" && s.SearchBackfill != nil {
		candSQL := `SELECT c.id FROM email_content c
			  JOIN emails e ON e.id = c.id AND e.account = ? AND e.deleted IS NULL
			  WHERE c.hydrated_at IS NULL`
		candArgs := []any{account}
		if f.After != nil {
			candSQL += ` AND e.received_at > ?`
			candArgs = append(candArgs, f.After.UnixMicro())
		}
		if f.Before != nil {
			candSQL += ` AND e.received_at < ?`
			candArgs = append(candArgs, f.Before.UnixMicro())
		}
		if f.HasAttachment != nil {
			candSQL += ` AND e.has_attachment = ?`
			candArgs = append(candArgs, boolInt(*f.HasAttachment))
		}
		if f.HasKeyword != "" {
			candSQL += ` AND json_extract(e.keywords, '$."` + escapeJSONPath(f.HasKeyword) + `"') = 1`
		}
		if q.Filter.InMailbox != "" {
			candSQL += ` AND EXISTS (SELECT 1 FROM email_mailbox em
				   WHERE em.email_id = c.id AND em.mailbox_uid = ? AND em.removed_modseq = 0)`
			candArgs = append(candArgs, memberArg)
		}
		limit := s.BackfillScan
		if limit <= 0 {
			limit = 2000
		}
		crows, err := s.db.QueryContext(ctx, candSQL+` LIMIT ?`,
			append(candArgs, limit)...)
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: backfill scan: %w", err)
		}
		var candidates []string
		for crows.Next() {
			var id string
			if err := crows.Scan(&id); err != nil {
				_ = crows.Close()
				return nil, 0, 0, "", err
			}
			candidates = append(candidates, id)
		}
		_ = crows.Close()
		if err := crows.Err(); err != nil {
			return nil, 0, 0, "", err
		}
		if len(candidates) > 0 {
			s.SearchBackfill(account, candidates)
		}
	}

	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	if q.Anchor != "" && !containsID(ids, q.Anchor) {
		// RFC 8620 §5.5: an anchor that is not among the results is an
		// anchorNotFound error, not a silent clamp to the end.
		return nil, 0, 0, "", jmapapi.ErrAnchorNotFound
	}
	position, window := paginateIDs(ids, q.Anchor, q.AnchorOffset, q.Position, q.Limit)
	s.log.Debug("store: query",
		"mailbox", q.Filter.InMailbox, "collapse", q.CollapseThreads,
		"position", q.Position, "limit", q.Limit,
		"hits", len(ids), "window", len(window))
	return window, position, len(ids), counter, nil
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// sortClause renders every requested comparator (RFC 8620 §4.4: the
// list is applied in order), deterministically tied off by id.
func sortClause(sorts []jmapapi.EmailSort) string {
	if len(sorts) == 0 {
		return "e.received_at DESC, e.id ASC"
	}
	parts := make([]string, 0, len(sorts)+1)
	for _, so := range sorts {
		var expr string
		switch so.Property {
		case "receivedAt":
			expr = "e.received_at"
		case "subject":
			expr = "c.subject_l"
		case "from":
			expr = "c.from_l"
		case "size":
			expr = "e.size"
		case "hasAttachment":
			expr = "e.has_attachment"
		default:
			expr = "e.received_at"
		}
		dir := "DESC"
		if so.Ascending {
			dir = "ASC"
		}
		parts = append(parts, expr+" "+dir)
	}
	return strings.Join(parts, ", ") + ", e.id ASC"
}

// paginateIDs applies anchor/anchorOffset (which replace position) and
// limit to an ordered id list. The caller has already rejected an anchor
// that is not in the results (ErrAnchorNotFound); a negative position
// counts from the end of the list (RFC 8620 §5.5).
func paginateIDs(ids []string, anchor string, anchorOffset, position, limit int) (int, []string) {
	if anchor != "" {
		position = len(ids)
		for i, id := range ids {
			if id == anchor {
				position = i + anchorOffset
				break
			}
		}
	} else if position < 0 {
		position = len(ids) + position
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

// queryEmailsCoveredPage answers a bounded, collapse-free covering
// query without materialising the mailbox: total from the maintained
// mailbox counters (or an index count when filters narrow the scope),
// the window as a LIMIT/OFFSET slice of the covering index, and an
// anchor's position as an index-bounded count. The result must be
// indistinguishable from streaming the whole list through
// paginateIDs — that is what the anchor tie-counting reproduces.
func (s *Store) queryEmailsCoveredPage(ctx context.Context, q jmapapi.EmailQuery, f jmapapi.EmailFilter, mailboxUID int64, counter string) ([]string, int, int, string, error) {
	where := ` WHERE em.mailbox_uid = ? AND em.removed_modseq = 0`
	args := []any{mailboxUID}
	if f.After != nil {
		where += ` AND em.received_at > ?`
		args = append(args, f.After.UnixMicro())
	}
	if f.Before != nil {
		where += ` AND em.received_at < ?`
		args = append(args, f.Before.UnixMicro())
	}
	if f.HasAttachment != nil {
		where += ` AND em.has_attachment = ?`
		args = append(args, boolInt(*f.HasAttachment))
	}
	dir := "DESC"
	if len(q.Sort) > 0 && q.Sort[0].Ascending {
		dir = "ASC"
	}

	total := 0
	if f.After == nil && f.Before == nil && f.HasAttachment == nil {
		// No filters: the counters are the total (FR-M.1 maintains
		// them incrementally; recount tests pin them to membership).
		err := s.db.QueryRowContext(ctx,
			`SELECT total_emails FROM mailboxes WHERE rowid = ?`,
			mailboxUID).Scan(&total)
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: covered total: %w", err)
		}
	} else {
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM email_mailbox em`+where, args...).Scan(&total)
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: covered count: %w", err)
		}
	}

	position := q.Position
	if q.Anchor != "" {
		var recv int64
		err := s.db.QueryRowContext(ctx,
			`SELECT em.received_at FROM email_mailbox em
			 WHERE em.mailbox_uid = ? AND em.email_id = ? AND em.removed_modseq = 0`,
			mailboxUID, q.Anchor).Scan(&recv)
		if err == sql.ErrNoRows {
			return nil, 0, 0, "", jmapapi.ErrAnchorNotFound
		} else if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: covered anchor: %w", err)
		} else {
			// Rows strictly before the anchor in sort order: every
			// row with a more extreme received_at, plus ties ordered
			// ahead of it by email_id (the index's tiebreak).
			cmp := ">"
			if dir == "ASC" {
				cmp = "<"
			}
			var before int
			err := s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM email_mailbox em`+where+
					` AND (em.received_at `+cmp+` ? OR (em.received_at = ? AND em.email_id < ?))`,
				append(args, recv, recv, q.Anchor)...).Scan(&before)
			if err != nil {
				return nil, 0, 0, "", fmt.Errorf("store: covered anchor count: %w", err)
			}
			position = before + q.AnchorOffset
		}
	} else if position < 0 {
		position = total + position
	}
	if position < 0 {
		position = 0
	}
	if position > total {
		position = total
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT em.email_id FROM email_mailbox em`+where+
			` ORDER BY em.received_at `+dir+`, em.email_id ASC LIMIT ? OFFSET ?`,
		append(args, q.Limit, position)...)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("store: covered page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var window []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, 0, "", err
		}
		window = append(window, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, "", err
	}
	s.log.Debug("store: covered query",
		"mailbox", q.Filter.InMailbox, "position", position,
		"total", total, "window", len(window))
	return window, position, total, counter, nil
}

// queryEmailsCollapsedPage answers a bounded collapseThreads query on
// the covering path. Collapse must see every row to pick each
// thread's first exemplar in sort order (FR-X.4) — but only the rows
// up to the requested window: exemplars stream in sort order from the
// covering index and the scan stops at position+limit, so page 0 of a
// 100k mailbox reads a few dozen rows, not 100k. The total is the
// maintained thread counter (or an index count under filters), and an
// anchor's position comes from streaming until the anchor id appears
// as an exemplar — bounded by how deep the client is looking, which
// is exactly the semantics of paginateIDs, minus the full materialisation.
func (s *Store) queryEmailsCollapsedPage(ctx context.Context, q jmapapi.EmailQuery, f jmapapi.EmailFilter, mailboxUID int64, counter string) ([]string, int, int, string, error) {
	where := ` WHERE em.mailbox_uid = ? AND em.removed_modseq = 0`
	args := []any{mailboxUID}
	if f.After != nil {
		where += ` AND em.received_at > ?`
		args = append(args, f.After.UnixMicro())
	}
	if f.Before != nil {
		where += ` AND em.received_at < ?`
		args = append(args, f.Before.UnixMicro())
	}
	if f.HasAttachment != nil {
		where += ` AND em.has_attachment = ?`
		args = append(args, boolInt(*f.HasAttachment))
	}
	dir := "DESC"
	if len(q.Sort) > 0 && q.Sort[0].Ascending {
		dir = "ASC"
	}

	total := 0
	if f.After == nil && f.Before == nil && f.HasAttachment == nil {
		err := s.db.QueryRowContext(ctx,
			`SELECT total_threads FROM mailboxes WHERE rowid = ?`,
			mailboxUID).Scan(&total)
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: collapsed total: %w", err)
		}
	} else {
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT thread_id) FROM email_mailbox em`+where, args...).Scan(&total)
		if err != nil {
			return nil, 0, 0, "", fmt.Errorf("store: collapsed count: %w", err)
		}
	}

	position := q.Position
	if q.Anchor != "" {
		// Stream exemplars until the anchor id shows up; its exemplar
		// index is the true starting position.
		found := -1
		err := s.streamExemplars(ctx, where, args, dir, func(idx int, id string) bool {
			if id == q.Anchor {
				found = idx
				return false
			}
			return true
		})
		if err != nil {
			return nil, 0, 0, "", err
		}
		if found < 0 {
			return nil, 0, 0, "", jmapapi.ErrAnchorNotFound
		}
		position = found + q.AnchorOffset
	} else if position < 0 {
		position = total + position
	}
	if position < 0 {
		position = 0
	}
	if position > total {
		position = total
	}

	var window []string
	err := s.streamExemplars(ctx, where, args, dir, func(idx int, id string) bool {
		if idx-position >= q.Limit {
			return false
		}
		window = append(window, id)
		return true
	})
	if err != nil {
		return nil, 0, 0, "", err
	}
	s.log.Debug("store: collapsed query",
		"mailbox", q.Filter.InMailbox, "position", position,
		"total", total, "window", len(window))
	return window, position, total, counter, nil
}

// streamExemplars walks the mailbox in sort order, keeping the first
// id seen per thread (the exemplar), and calls yield for each exemplar
// with its index; yield returns false to stop the scan early.
func (s *Store) streamExemplars(ctx context.Context, where string, args []any, dir string, yield func(idx int, id string) bool) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT em.email_id, em.thread_id FROM email_mailbox em`+where+
			` ORDER BY em.received_at `+dir+`, em.email_id ASC`, args...)
	if err != nil {
		return fmt.Errorf("store: exemplar scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	idx := 0
	for rows.Next() {
		var id, thread string
		if err := rows.Scan(&id, &thread); err != nil {
			return err
		}
		if seen[thread] {
			continue
		}
		seen[thread] = true
		if !yield(idx, id) {
			return nil
		}
		idx++
	}
	return rows.Err()
}

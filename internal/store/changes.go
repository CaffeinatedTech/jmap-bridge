package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// Changes implements [jmapapi.Store]: replay of Mailbox/Email changes
// since sinceState (RFC 8620 §5.2, FR-M.3, FR-M.6, FR-J.7).
//
// Classification: a row modified after since is destroyed when its
// tombstone says so, created when its creation is also after since, and
// updated otherwise. A sinceState the bridge cannot replay — older than
// the tombstone retention floor, or newer than anything it has issued —
// yields ErrCannotCalculateChanges so dispatch answers
// cannotCalculateChanges and the client refetches with /get.
func (s *Store) Changes(ctx context.Context, account, kind, sinceState string) (jmapapi.ChangeSet, error) {
	since, err := strconv.ParseInt(sinceState, 10, 64)
	if err != nil || since < 0 {
		return jmapapi.ChangeSet{}, jmapapi.ErrCannotCalculateChanges
	}
	meta, err := loadMeta(ctx, s.db, account)
	if err != nil {
		return jmapapi.ChangeSet{}, err
	}
	var current int64
	table := "emails"
	switch kind {
	case "Mailbox":
		current = meta.MailboxState
		table = "mailboxes"
	case "Email":
		current = meta.EmailState
	default:
		return jmapapi.ChangeSet{}, jmapapi.ErrCannotCalculateChanges
	}
	if since > current || since < meta.PurgedThrough {
		return jmapapi.ChangeSet{}, jmapapi.ErrCannotCalculateChanges
	}
	out := jmapapi.ChangeSet{NewState: strconv.FormatInt(current, 10)}
	if since == current {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_modseq, deleted FROM `+table+`
		 WHERE account = ? AND updated_modseq > ?
		 ORDER BY updated_modseq, id`, account, since)
	if err != nil {
		return jmapapi.ChangeSet{}, fmt.Errorf("store: changes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var created int64
		var deleted sql.NullInt64
		if err := rows.Scan(&id, &created, &deleted); err != nil {
			return jmapapi.ChangeSet{}, err
		}
		switch {
		case deleted.Valid:
			out.Destroyed = append(out.Destroyed, id)
		case created > since:
			out.Created = append(out.Created, id)
		default:
			out.Updated = append(out.Updated, id)
		}
	}
	if err := rows.Err(); err != nil {
		return jmapapi.ChangeSet{}, err
	}
	return out, nil
}

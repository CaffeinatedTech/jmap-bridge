package store

import (
	"context"
	"crypto/sha1" //nolint:gosec // stable non-crypto thread keys, PLAN §4
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// ThreadInput is everything thread derivation needs from one message
// (FR-M.7): Message-ID/References chains plus the normalised subject.
type ThreadInput struct {
	MessageID  string
	References []string
	InReplyTo  []string
	Subject    string
}

// replyPrefixes are stripped (repeatedly, case-insensitively) when
// building the base-subject thread key.
var replyPrefixes = regexp.MustCompile(`^(?i:(?:re|fwd?|aw|antw|sv|vs)\s*:\s*)+`)

// deriveThread assigns a message to a thread, creating or merging
// threads as needed. The key space lives in `threads`:
//
//	"m:<sha1(Message-ID)>"  — one per RFC 5322 message id seen (own and
//	                          referenced), so References/In-Reply-To
//	                          chains join threads across arrival order;
//	"s:<sha1(base subject)>" — the normalised-subject fallback for
//	                          messages with no RFC linkage.
//
// Candidate keys are consulted first; only when none exist does the
// subject key get a vote. When two existing threads are joined by one
// message, they merge into the lexicographically smallest id and every
// member email is updated, so clients see it through /changes.
func deriveThread(ctx context.Context, tx *sql.Tx, account string, in ThreadInput, seq int64) (string, error) {
	keys := threadKeys(in)
	candidates := map[string]bool{}
	for _, k := range keys.rfc {
		var id string
		err := tx.QueryRowContext(ctx,
			`SELECT thread_id FROM threads WHERE account = ? AND thread_key = ?`,
			account, k).Scan(&id)
		if err == nil {
			candidates[id] = true
		} else if err != sql.ErrNoRows {
			return "", fmt.Errorf("store: thread lookup: %w", err)
		}
	}

	var threadID string
	if len(candidates) == 0 {
		// No RFC linkage: fall back to the base subject, first claimant
		// wins so unrelated same-subject mail keeps separate threads.
		if keys.subject != "" {
			err := tx.QueryRowContext(ctx,
				`SELECT thread_id FROM threads WHERE account = ? AND thread_key = ?`,
				account, keys.subject).Scan(&threadID)
			if err != nil && err != sql.ErrNoRows {
				return "", fmt.Errorf("store: thread subject lookup: %w", err)
			}
		}
	}
	if threadID == "" && len(candidates) > 0 {
		threadID = minString(candidates)
	}
	if threadID == "" {
		id, err := mintID(ctx, tx)
		if err != nil {
			return "", err
		}
		threadID = id
	} else if len(candidates) > 1 {
		if err := mergeThreads(ctx, tx, account, threadID, candidates, seq); err != nil {
			return "", err
		}
	}

	// Register this message's keys against the chosen thread.
	for _, k := range keys.rfc {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO threads(account, thread_key, thread_id, last_seen) VALUES (?, ?, ?, ?)
			 ON CONFLICT(account, thread_key) DO UPDATE SET thread_id = excluded.thread_id,
			                                                   last_seen = excluded.last_seen`,
			account, k, threadID, time.Now().Unix()); err != nil {
			return "", fmt.Errorf("store: register thread key: %w", err)
		}
	}
	if keys.subject != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO threads(account, thread_key, thread_id, last_seen) VALUES (?, ?, ?, ?)
			 ON CONFLICT(account, thread_key) DO NOTHING`,
			account, keys.subject, threadID, time.Now().Unix()); err != nil {
			return "", fmt.Errorf("store: register subject key: %w", err)
		}
	}
	return threadID, nil
}

// mergeThreads folds every candidate thread except the survivor into it.
func mergeThreads(ctx context.Context, tx *sql.Tx, account, survivor string, candidates map[string]bool, seq int64) error {
	for loser := range candidates {
		if loser == survivor {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE threads SET thread_id = ? WHERE account = ? AND thread_id = ?`,
			survivor, account, loser); err != nil {
			return fmt.Errorf("store: merge thread keys: %w", err)
		}
		// Live members move visibly; tombstones keep their historical
		// thread without generating change noise.
		if _, err := tx.ExecContext(ctx,
			`UPDATE emails SET thread_id = ?, updated_modseq = ?
			 WHERE account = ? AND thread_id = ? AND deleted IS NULL`,
			survivor, seq, account, loser); err != nil {
			return fmt.Errorf("store: merge thread members: %w", err)
		}
	}
	return nil
}

type threadKeySet struct {
	rfc     []string
	subject string
}

// threadKeys builds the registry keys for one message.
func threadKeys(in ThreadInput) threadKeySet {
	seen := map[string]bool{}
	var out threadKeySet
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out.rfc = append(out.rfc, "m:"+hashKey(id))
	}
	add(in.MessageID)
	for _, r := range in.References {
		add(r)
	}
	for _, r := range in.InReplyTo {
		add(r)
	}
	if base := baseSubject(in.Subject); base != "" {
		out.subject = "s:" + hashKey(base)
	}
	return out
}

func hashKey(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // lookup key, not security
	return hex.EncodeToString(sum[:])
}

// baseSubject normalises a subject for the fallback thread key: reply
// and forward prefixes are stripped repeatedly and case/spacing folded.
func baseSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		next := strings.TrimSpace(replyPrefixes.ReplaceAllString(s, ""))
		if next == s || next == "" {
			break
		}
		s = next
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func minString(set map[string]bool) string {
	out := ""
	for v := range set {
		if out == "" || v < out {
			out = v
		}
	}
	return out
}

// ThreadsByID implements [jmapapi.Store]: the named threads with their
// live member ids oldest-first (RFC 8621 §3.1); a nil ids slice means
// every thread the account has.
func (s *Store) ThreadsByID(ctx context.Context, account string, ids []string) ([]*jmapapi.Thread, string, []string, error) {
	state, err := s.EmailStateString(ctx, account)
	if err != nil {
		return nil, "", nil, err
	}
	if ids == nil {
		rows, err := s.db.QueryContext(ctx,
			`SELECT DISTINCT thread_id FROM emails
			 WHERE account = ? AND deleted IS NULL ORDER BY thread_id`, account)
		if err != nil {
			return nil, "", nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, "", nil, err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, "", nil, err
		}
	}
	out := make([]*jmapapi.Thread, 0, len(ids))
	var notFound []string
	for _, id := range ids {
		t, err := s.threadByID(ctx, account, id)
		if err != nil {
			return nil, "", nil, err
		}
		if t == nil {
			notFound = append(notFound, id)
			continue
		}
		out = append(out, t)
	}
	return out, state, notFound, nil
}

func (s *Store) threadByID(ctx context.Context, account, id string) (*jmapapi.Thread, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM emails
		 WHERE account = ? AND thread_id = ? AND deleted IS NULL
		 ORDER BY received_at, id`, account, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	t := &jmapapi.Thread{ID: id}
	for rows.Next() {
		var emailID string
		if err := rows.Scan(&emailID); err != nil {
			return nil, err
		}
		t.EmailIDs = append(t.EmailIDs, emailID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.EmailIDs) == 0 {
		return nil, nil
	}
	return t, nil
}

// sortStrings is kept next to the thread helpers for merge determinism
// tests; sort is otherwise used by queries.
var _ = sort.Strings

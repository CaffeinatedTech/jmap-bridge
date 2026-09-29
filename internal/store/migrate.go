package store

import (
	"context"
	"fmt"
)

// migrations are forward-only DDL batches applied in order; each entry
// bumps schema_version by one (FR-D.9, PLAN §11). Never edit a shipped
// entry — add a new one.
//
// v1 creates the PLAN §4 schema. The mail-side tables are M1's; the
// contacts tables (addressbooks, cards) ship with the same batch so the
// schema matches PLAN §4 as written — M6 only fills them. email_fts is
// created empty here and starts being written at M5 (FR-X.1).
var migrations = []string{
	`
CREATE TABLE mailboxes (            -- JMAP Mailbox ⇄ IMAP folder
  id TEXT PRIMARY KEY,              -- opaque JMAP id (stable across uidvalidity)
  account TEXT NOT NULL,
  parent_id TEXT, role TEXT,        -- \Inbox \Sent \Drafts \Trash \Junk \Archive via SPECIAL-USE
  name TEXT NOT NULL,               -- IMAP path (hierarchical, separator kept)
  sort_order INTEGER NOT NULL DEFAULT 0,
  uidvalidity INTEGER, uidnext INTEGER, highestmodseq INTEGER,
  total_emails INTEGER NOT NULL DEFAULT 0, unread_emails INTEGER NOT NULL DEFAULT 0,
  total_threads INTEGER NOT NULL DEFAULT 0, unread_threads INTEGER NOT NULL DEFAULT 0,
  may_read_items INTEGER NOT NULL DEFAULT 1, may_add_items INTEGER NOT NULL DEFAULT 1,
  may_remove_items INTEGER NOT NULL DEFAULT 1, may_create_child INTEGER NOT NULL DEFAULT 1,
  may_rename INTEGER NOT NULL DEFAULT 1, may_delete INTEGER NOT NULL DEFAULT 1,
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL,
  updated_not_counts_modseq INTEGER NOT NULL DEFAULT 0,
  deleted INTEGER                   -- deletion modseq (tombstone for /changes)
);
CREATE INDEX mailboxes_changes ON mailboxes(account, updated_modseq);
CREATE INDEX mailboxes_parent ON mailboxes(account, parent_id, deleted);

CREATE TABLE emails (               -- JMAP Email metadata (narrow, hot)
  id TEXT PRIMARY KEY,
  account TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  keywords TEXT NOT NULL,           -- JSON: keyword → true
  received_at INTEGER NOT NULL,     -- unix micros
  size INTEGER NOT NULL,
  has_attachment INTEGER NOT NULL,
  preview TEXT NOT NULL DEFAULT '',
  created_modseq INTEGER NOT NULL,
  updated_modseq INTEGER NOT NULL,
  deleted INTEGER                   -- deletion modseq (tombstone for /changes)
);
CREATE INDEX emails_changes ON emails(account, updated_modseq);
CREATE INDEX emails_thread ON emails(account, thread_id, received_at);
CREATE INDEX emails_recv ON emails(account, received_at);
CREATE INDEX emails_live ON emails(account, deleted, received_at);

CREATE TABLE email_mailbox (        -- MailboxEmailList: membership + history
  account TEXT NOT NULL,
  mailbox_uid INTEGER NOT NULL,     -- mailboxes.rowid: compact per-mailbox key
  email_id TEXT NOT NULL,
  removed_modseq INTEGER NOT NULL DEFAULT 0,
  added_modseq INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX email_mailbox_live ON email_mailbox(mailbox_uid, email_id)
  WHERE removed_modseq = 0;
CREATE INDEX email_mailbox_email ON email_mailbox(email_id);

CREATE TABLE email_content (        -- parsed at backfill; bodies at hydration
  id TEXT PRIMARY KEY,
  headers TEXT NOT NULL,            -- JSON subset (from/to/cc/bcc/replyTo/subject/…)
  message_ids TEXT, in_reply_to TEXT, "references" TEXT,  -- JSON arrays
  sent_at INTEGER,
  subject_l TEXT NOT NULL DEFAULT '',  -- lowercase mirrors for query filters/sorts
  from_l TEXT NOT NULL DEFAULT '',     -- (M5's FTS5 supersedes them for text)
  to_l TEXT NOT NULL DEFAULT '',
  body_structure TEXT NOT NULL DEFAULT '{}',  -- JSON EmailBodyPart tree
  body_values TEXT NOT NULL DEFAULT '{}',     -- partId → {value, isTruncated}
  hydrated_at INTEGER                -- null until the body was fetched
);

CREATE VIRTUAL TABLE email_fts USING fts5(
  email_id UNINDEXED, subject, people, body,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE threads (              -- thread key → thread id (FR-M.7)
  account TEXT NOT NULL,
  thread_key TEXT NOT NULL,         -- 'm:<sha1(Message-ID)>' | 's:<sha1(base subject)>'
  thread_id TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  PRIMARY KEY (account, thread_key)
);
CREATE INDEX threads_by_id ON threads(account, thread_id);

CREATE TABLE imap_uids (            -- (folder, uidvalidity, uid) → email id
  account TEXT NOT NULL, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL,
  uid INTEGER NOT NULL, email_id TEXT NOT NULL,
  PRIMARY KEY (account, folder, uidvalidity, uid)
);
CREATE INDEX imap_uids_email ON imap_uids(email_id);

CREATE TABLE addressbooks (         -- JMAP AddressBook ⇄ CardDAV collection
  id TEXT PRIMARY KEY, account TEXT, href TEXT NOT NULL,
  name TEXT, description TEXT, sort_order INTEGER,
  may_read INTEGER NOT NULL DEFAULT 1, may_write INTEGER NOT NULL DEFAULT 1,
  may_share INTEGER NOT NULL DEFAULT 1, may_delete INTEGER NOT NULL DEFAULT 1,
  sync_token TEXT, ctag TEXT, etag TEXT,
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL, deleted INTEGER
);
CREATE TABLE cards (                -- JSContact ⇄ vCard
  id TEXT PRIMARY KEY,              -- = vCard UID (RFC 9610 §3)
  account TEXT, book_id TEXT NOT NULL,
  uid TEXT NOT NULL, kind TEXT, name TEXT,   -- denormalised for search/sort
  jscontact TEXT NOT NULL,          -- full JSContact JSON
  vcard TEXT NOT NULL,              -- last known wire form
  href TEXT NOT NULL, etag TEXT,    -- CardDAV addressing
  created_modseq INTEGER NOT NULL, updated_modseq INTEGER NOT NULL, deleted INTEGER,
  UNIQUE (account, uid)
);

CREATE TABLE blobs (                -- blobId → relative file path, media type, size
  id TEXT PRIMARY KEY, account TEXT, path TEXT NOT NULL,
  media_type TEXT, size INTEGER, created_at INTEGER
);

CREATE TABLE sync_state (           -- per account/folder sync bookkeeping
  account TEXT NOT NULL, scope TEXT NOT NULL,   -- 'folder:<name>' | 'contacts' | 'account'
  highestmodseq INTEGER NOT NULL DEFAULT 0, sync_token TEXT,
  last_full_scan INTEGER,
  PRIMARY KEY (account, scope)
);

CREATE TABLE counters (             -- process-global monotonic allocators
  name TEXT PRIMARY KEY,            -- 'seq': change counter, feeds id minting
  value INTEGER NOT NULL
);

CREATE TABLE email_msgid (          -- a message's own Message-ID → its email id
  account TEXT NOT NULL, msgid TEXT NOT NULL, email_id TEXT NOT NULL,
  PRIMARY KEY (account, msgid)
);

CREATE TABLE tokens (               -- client-facing auth tokens
  account TEXT PRIMARY KEY, token_hash TEXT NOT NULL
);

`,
	// v2 (M3): the raw RFC 5322 copy of a message, when the bridge
	// holds it. Email/get answers `blobId` from it (RFC 8621 §4.1.1) and
	// EmailSubmission/set submits exactly these bytes, so an attachment
	// round-trips byte-exact through send. It is filled lazily — the
	// bytes exist once we built the message (draft create) or fetched it
	// to send it — never downloaded just to answer a list.
	`
ALTER TABLE email_content ADD COLUMN raw_blob_id TEXT;
`,
	// v3 (M4): the OAuth2 provider's tokens per account (FR-A.5, FR-A.7).
	// Values are opaque to the store — the OAuth layer seals them before
	// they land here and opens them on read (FR-A.8), so this table is
	// never the place a plaintext refresh token is found. access_expiry
	// is unix seconds; a zero value means "unknown, refresh before use".
	`
CREATE TABLE oauth_tokens (
  account TEXT PRIMARY KEY,
  refresh_token TEXT NOT NULL,
  access_token TEXT NOT NULL DEFAULT '',
  access_expiry INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
`,
}

// migrate applies every not-yet-applied migration and refuses a database
// from a future schema (forward-only, no downgrades: FR-D.9, NFR-9).
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: schema_version: %w", err)
	}
	var have int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&have); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if have > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this build (v%d)", have, len(migrations))
	}
	for i := have; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migrate begin: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration v%d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, i+1); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate commit: %w", err)
		}
		s.log.Info("store: applied migration", "version", i+1)
	}
	return nil
}

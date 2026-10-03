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
	// v4 (M4): mailboxes whose membership the server manages (Gmail's
	// \All mailbox). Detected from the LIST attribute, not the role: a
	// plain folder named "Archive" shares the archive role but is a
	// normal label-backed mailbox.
	`
ALTER TABLE mailboxes ADD COLUMN implicit INTEGER NOT NULL DEFAULT 0;
`,
	// v5 (M5): the real search index. The v1 email_fts shape (one
	// "people" column) was never written to — from/to filters need
	// separate columns or a from-filter would also match recipients —
	// and a prefix index keeps from:ada-style prefix queries off the
	// full term scan (FR-X.1, FR-X.2). Open() backfills it once for
	// rows ingested before this migration (fts.go).
	`
DROP TABLE IF EXISTS email_fts;
CREATE VIRTUAL TABLE email_search USING fts5(
  email_id UNINDEXED, subject, sender, recipient, body,
  tokenize = 'unicode61 remove_diacritics 2',
  prefix = '2 3'
);
`,
	// v6 (M5): the mailbox query's hot path (browse/collapse a full
	// mailbox in sort order) must not do 100k text-PK lookups back into
	// emails — NFR-1's 150 ms budget measured exactly that. The three
	// columns the ordering, collapsing and hasAttachment filter need
	// are denormalised onto the membership row, backed by a covering
	// index; the queries in query.go read them instead of joining.
	// Invariant: a live membership row's copies match its email's
	// values — membership is removed in the same transaction that
	// tombstones the last copy, and thread merges rewrite both.
	`
ALTER TABLE email_mailbox ADD COLUMN received_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE email_mailbox ADD COLUMN thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE email_mailbox ADD COLUMN has_attachment INTEGER NOT NULL DEFAULT 0;
UPDATE email_mailbox SET
  received_at = (SELECT received_at FROM emails WHERE id = email_id),
  thread_id = (SELECT thread_id FROM emails WHERE id = email_id),
  has_attachment = COALESCE((SELECT has_attachment FROM emails WHERE id = email_id), 0);
CREATE INDEX email_mailbox_box ON email_mailbox(mailbox_uid, removed_modseq, received_at, email_id);
CREATE INDEX email_mailbox_thread ON email_mailbox(mailbox_uid, removed_modseq, thread_id, received_at, email_id);
CREATE INDEX email_content_unhydrated ON email_content(hydrated_at);
ALTER TABLE email_content ADD COLUMN fts_rowid INTEGER;
`,
	// v7 (M10): native provider ids for the Gmail API backend
	// (D-API-9, GMAIL_API_PLAN §5). The engine's store path is keyed by
	// (folder, uidvalidity, uid); the Gmail API's opaque message ids
	// cannot be that uid, so the adapter allocates a stable synthetic
	// numeric handle per native id and keeps the mapping here. `kind`
	// discriminates message/label/thread so a future Graph backend reuses
	// the table rather than adding a third. `jmap_id` is filled where the
	// native object has a JMAP counterpart (writes, M11). mailboxes gains
	// the label id so a path resolves back to its Gmail label without a
	// discovery round trip.
	`
CREATE TABLE native_ids (
  account   TEXT NOT NULL,
  kind      TEXT NOT NULL,      -- 'message' | 'label' | 'thread'
  native_id TEXT NOT NULL,      -- Gmail message/label/thread id
  uid       INTEGER NOT NULL DEFAULT 0,
  jmap_id   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account, kind, native_id)
);
CREATE INDEX native_ids_uid ON native_ids(account, kind, uid);
CREATE INDEX native_ids_jmap ON native_ids(account, kind, jmap_id);

CREATE TABLE gmail_drafts (
  account  TEXT NOT NULL,
  jmap_id  TEXT NOT NULL,       -- the Email id
  draft_id TEXT NOT NULL,
  PRIMARY KEY (account, jmap_id)
);

ALTER TABLE mailboxes ADD COLUMN native_id TEXT;
`,
	// v8 (M11): the Gmail API draft handle. v7 keyed gmail_drafts by the
	// JMAP Email id, but the adapter learns the draft id at Append time —
	// before the engine has minted the Email id — and the uid is the one
	// handle both sides hold then. v0.1 does not edit draft content, so
	// Gmail never replaces the message id under us (GMAIL_API_PLAN §9);
	// keying by the stable synthetic uid is therefore sufficient and lets
	// a future submission resolve draft → send honestly. The table is
	// empty at this point (M10 is read-only), so recreating it loses
	// nothing.
	`
DROP TABLE gmail_drafts;
CREATE TABLE gmail_drafts (
  account  TEXT NOT NULL,
  uid      INTEGER NOT NULL,   -- synthetic message uid (native_ids)
  draft_id TEXT NOT NULL,      -- Gmail draft id
  PRIMARY KEY (account, uid)
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

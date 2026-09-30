package store

// Contacts tables (addressbooks, cards — PLAN §4, shipped with v1, filled
// by M6). Everything here mirrors what the CardDAV server said: the sync
// engine calls these only after the server accepted a change (D-14), and
// the reads serve the JMAP contacts methods (FR-P.4–.7). State strings
// follow the mail pattern: modseq floors bumped inside the same
// transaction that writes, so /changes and SSE can never disagree
// (golden rule 5).

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// BookRec is one address book as discovery reported it. Href is the
// server path (the identity across sync passes); id is minted here and
// stays stable while the href exists.
type BookRec struct {
	Href        string
	Name        string
	Description string
	SortOrder   int
	CTag        string
	SyncToken   string
	MayWrite    bool
	MayDelete   bool
}

// SyncBooks reconciles the account's books with a fresh discovery pass
// (FR-P.1, FR-P.5): creates, updates, and tombstones. The AddressBook
// state is bumped only when membership or a property clients see
// changed; server-side token bookkeeping (sync_token, ctag) never moves
// the JMAP state on its own — that would make every pass look changed.
func (s *Store) SyncBooks(ctx context.Context, account string, books []BookRec) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		type existingBook struct {
			id                  string
			name, description   sql.NullString
			sortOrder           sql.NullInt64
			mayWrite, mayDelete sql.NullInt64
			syncToken, ctag     sql.NullString
			deleted             sql.NullInt64
			created, updated    int64
		}
		existing := map[string]*existingBook{}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, href, name, description, sort_order, may_write, may_delete,
			        sync_token, ctag, created_modseq, updated_modseq, deleted
			 FROM addressbooks WHERE account = ?`, account)
		if err != nil {
			return fmt.Errorf("store: list books: %w", err)
		}
		for rows.Next() {
			var b existingBook
			var href string
			if err := rows.Scan(&b.id, &href, &b.name, &b.description, &b.sortOrder,
				&b.mayWrite, &b.mayDelete, &b.syncToken, &b.ctag,
				&b.created, &b.updated, &b.deleted); err != nil {
				_ = rows.Close()
				return err
			}
			existing[href] = &b
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		changed := false
		seen := map[string]bool{}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		for _, in := range books {
			seen[in.Href] = true
			cur, ok := existing[in.Href]
			if !ok {
				id, err := mintID(ctx, tx)
				if err != nil {
					return err
				}
				// sync_token starts NULL on purpose: the cursor is
				// only what our own last sync-collection returned,
				// never the token the PROPFIND happened to show (a
				// discovery token would silently skip the first pass).
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO addressbooks(id, account, href, name, description, sort_order,
					   may_read, may_write, may_share, may_delete,
					   created_modseq, updated_modseq)
					 VALUES (?, ?, ?, ?, ?, ?, 1, ?, 1, ?, ?, ?)`,
					id, account, in.Href, nullStr(in.Name), nullStr(in.Description),
					in.SortOrder, boolInt(in.MayWrite), boolInt(in.MayDelete),
					seq, seq); err != nil {
					return fmt.Errorf("store: create book %q: %w", in.Href, err)
				}
				changed = true
				continue
			}
			live := !cur.deleted.Valid
			if !live {
				// The book is back (re-created on the server under the
				// same href): resurrect with fresh properties.
				if _, err := tx.ExecContext(ctx,
					`UPDATE addressbooks SET deleted = NULL, created_modseq = ?, updated_modseq = ?,
					   name = ?, description = ?, sort_order = ?, may_write = ?, may_delete = ?,
					   sync_token = NULL, ctag = NULL
					 WHERE id = ?`,
					seq, seq, nullStr(in.Name), nullStr(in.Description), in.SortOrder,
					boolInt(in.MayWrite), boolInt(in.MayDelete),
					cur.id); err != nil {
					return fmt.Errorf("store: resurrect book %q: %w", in.Href, err)
				}
				changed = true
				continue
			}
			// Client-visible properties only (token/ctag are bookkeeping
			// and do not move the JMAP state).
			propsChanged := cur.name.String != in.Name ||
				cur.description.String != in.Description ||
				intVal(cur.sortOrder) != in.SortOrder ||
				intVal(cur.mayWrite) != boolInt(in.MayWrite) ||
				intVal(cur.mayDelete) != boolInt(in.MayDelete)
			if _, err := tx.ExecContext(ctx,
				`UPDATE addressbooks SET sync_token = ?, ctag = ? WHERE id = ?`,
				nullStr(in.SyncToken), nullStr(in.CTag), cur.id); err != nil {
				return fmt.Errorf("store: book bookkeeping %q: %w", in.Href, err)
			}
			if propsChanged {
				if _, err := tx.ExecContext(ctx,
					`UPDATE addressbooks SET name = ?, description = ?, sort_order = ?,
					   may_write = ?, may_delete = ?, updated_modseq = ?
					 WHERE id = ?`,
					nullStr(in.Name), nullStr(in.Description), in.SortOrder,
					boolInt(in.MayWrite), boolInt(in.MayDelete), seq, cur.id); err != nil {
					return fmt.Errorf("store: update book %q: %w", in.Href, err)
				}
				changed = true
			}
		}
		cardsGone := false
		for href, cur := range existing {
			if cur.deleted.Valid || seen[href] {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE addressbooks SET deleted = ?, updated_modseq = ? WHERE id = ?`,
				seq, seq, cur.id); err != nil {
				return fmt.Errorf("store: tombstone book %q: %w", href, err)
			}
			// A vanished book takes its cards with it (FR-P.5's
			// destroyed list plus the matching ContactCard destroys).
			res, err := tx.ExecContext(ctx,
				`UPDATE cards SET deleted = ?, updated_modseq = ?
				 WHERE account = ? AND book_id = ? AND deleted IS NULL`,
				seq, seq, account, cur.id)
			if err != nil {
				return fmt.Errorf("store: cascade cards of book %q: %w", href, err)
			}
			if c, _ := res.RowsAffected(); c > 0 {
				cardsGone = true
			}
			changed = true
		}
		if !changed {
			return nil
		}
		if err := bumpAddressBookState(ctx, tx, account, seq); err != nil {
			return err
		}
		if cardsGone {
			return bumpContactCardState(ctx, tx, account, seq)
		}
		return nil
	})
}

// StoreCard is one card as the sync loop found it on the server. JSContact
// is the canonical conversion; VCard the exact wire bytes (the store's
// "last known" form). PhotoBytes, when set, are inline photo bytes the
// caller extracted: they land in the blob store as part of the same
// transaction (FR-P.11) and the reference in JSContact already names the
// id the caller minted.
type StoreCard struct {
	UID       string
	Kind      string
	Name      string
	JSContact string
	VCard     string
	Href      string
	ETag      string

	// PhotoBlobID/PhotoBytes/PhotoMediaType travel together: the engine
	// ran the blob conversion before handing the card over (FR-P.11).
	PhotoBlobID    string
	PhotoBytes     []byte
	PhotoMediaType string
}

// PutCards writes sync-observed cards in one pass per book (FR-P.2's
// fold step): created, updated (etag or content moved) or untouched.
// A uid the cache already holds at another href is re-pointed — the
// server's addressing wins, because a card's identity is its UID
// (RFC 9610 §3).
func (s *Store) PutCards(ctx context.Context, account, bookID string, cards []StoreCard) (int, error) {
	if len(cards) == 0 {
		return 0, nil
	}
	touched := 0
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		for i := range cards {
			c := cards[i]
			n, err := putCard(ctx, tx, s.blobs, account, bookID, c, seq)
			if err != nil {
				return err
			}
			touched += n
		}
		if touched == 0 {
			return nil
		}
		return bumpContactCardState(ctx, tx, account, seq)
	})
	if err != nil {
		return 0, err
	}
	return touched, nil
}

// putCard upserts one card; returns 1 when the JMAP-visible state moved.
func putCard(ctx context.Context, tx *sql.Tx, blobs *BlobStore, account, bookID string, c StoreCard, seq int64) (int, error) {
	var id, href string
	var etag, js, kind, name sql.NullString
	var deleted sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT id, href, COALESCE(etag, ''), jscontact, kind, name, deleted
		 FROM cards WHERE account = ? AND uid = ?`, account, c.UID).
		Scan(&id, &href, &etag, &js, &kind, &name, &deleted)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cards(id, account, book_id, uid, kind, name, jscontact, vcard,
			   href, etag, created_modseq, updated_modseq)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.UID, account, bookID, c.UID, nullStr(c.Kind), nullStr(c.Name),
			c.JSContact, c.VCard, c.Href, nullStr(c.ETag), seq, seq); err != nil {
			return 0, fmt.Errorf("store: create card %s: %w", c.UID, err)
		}
		if err := storeCardPhoto(ctx, tx, blobs, account, c); err != nil {
			return 0, err
		}
		return 1, nil
	case err != nil:
		return 0, fmt.Errorf("store: read card %s: %w", c.UID, err)
	}

	if deleted.Valid {
		// The server restored (or never lost) a card we had tombstoned:
		// resurrection counts as a create for /changes.
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET deleted = NULL, created_modseq = ?, updated_modseq = ?,
			   book_id = ?, href = ?, etag = ?, kind = ?, name = ?, jscontact = ?, vcard = ?
			 WHERE id = ? AND account = ?`,
			seq, seq, bookID, c.Href, nullStr(c.ETag), nullStr(c.Kind), nullStr(c.Name),
			c.JSContact, c.VCard, id, account); err != nil {
			return 0, fmt.Errorf("store: resurrect card %s: %w", c.UID, err)
		}
		if err := storeCardPhoto(ctx, tx, blobs, account, c); err != nil {
			return 0, err
		}
		return 1, nil
	}

	// Unchanged: same server address, same etag (or the server withholds
	// etags and the bytes are equal), same book.
	if href == c.Href && etag.String == c.ETag && kind.String == c.Kind &&
		name.String == c.Name && js.String == c.JSContact {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cards SET book_id = ?, href = ?, etag = ?, kind = ?, name = ?,
		   jscontact = ?, vcard = ?, updated_modseq = ?
		 WHERE id = ? AND account = ?`,
		bookID, c.Href, nullStr(c.ETag), nullStr(c.Kind), nullStr(c.Name),
		c.JSContact, c.VCard, seq, id, account); err != nil {
		return 0, fmt.Errorf("store: update card %s: %w", c.UID, err)
	}
	if err := storeCardPhoto(ctx, tx, blobs, account, c); err != nil {
		return 0, err
	}
	return 1, nil
}

// storeCardPhoto files inline photo bytes handed over by the engine. The
// JSContact already names the blob id — it was minted before conversion —
// so the row and the blob land atomically (PLAN §10).
// storeCardPhoto files inline photo bytes the engine handed over. The
// JSContact already names the blob id — it was minted before conversion —
// so the blob row lands atomically with the card (PLAN §10). The media
// type was checked against the image allow-list upstream (FR-P.11).
func storeCardPhoto(ctx context.Context, tx *sql.Tx, blobs *BlobStore, account string, c StoreCard) error {
	if c.PhotoBlobID == "" || c.PhotoBytes == nil {
		return nil
	}
	return blobs.put(ctx, tx, account, c.PhotoBlobID, c.PhotoMediaType, c.PhotoBytes)
}

// CommitCardWrite records a ContactCard/set the DAV server already
// accepted (D-14 applied to CardDAV): the create or update is stored
// post-write with the etag the server returned; destroy tombstones after
// the DELETE landed.
func (s *Store) CommitCardWrite(ctx context.Context, account, bookID string, c StoreCard) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := putCard(ctx, tx, s.blobs, account, bookID, c, seq); err != nil {
			return err
		}
		return bumpContactCardState(ctx, tx, account, seq)
	})
}

// CommitCardDestroy tombstones a card the server just deleted, or whose
// deletion the server confirmed as already-gone (FR-P.10 — both end the
// same way in the cache).
func (s *Store) CommitCardDestroy(ctx context.Context, account, id string) error {
	return s.tx(ctx, account, true, func(tx *sql.Tx) error {
		var cur int64
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(updated_modseq, 0) FROM cards WHERE account = ? AND id = ?`,
			account, id).Scan(&cur)
		if err == sql.ErrNoRows {
			return nil // never had it: nothing to tombstone
		}
		if err != nil {
			return fmt.Errorf("store: read card for destroy: %w", err)
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET deleted = ?, updated_modseq = ? WHERE account = ? AND id = ?`,
			seq, seq, account, id); err != nil {
			return fmt.Errorf("store: tombstone card: %w", err)
		}
		return bumpContactCardState(ctx, tx, account, seq)
	})
}

// TombstoneCardsByHrefs marks the cards at the given server hrefs gone
// (sync-detected deletes, FR-P.2).
func (s *Store) TombstoneCardsByHrefs(ctx context.Context, account string, hrefs []string) (int, error) {
	if len(hrefs) == 0 {
		return 0, nil
	}
	touched := 0
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		ph, args := idPlaceholders(hrefs)
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM cards WHERE account = ? AND href IN (`+ph+`) AND deleted IS NULL`,
			append([]any{account}, args...)...)
		if err != nil {
			return fmt.Errorf("store: find cards by href: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		phIDs, argsIDs := idPlaceholders(ids)
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET deleted = ?, updated_modseq = ? WHERE account = ? AND id IN (`+phIDs+`)`,
			append([]any{seq, seq, account}, argsIDs...)...); err != nil {
			return fmt.Errorf("store: tombstone cards: %w", err)
		}
		touched = len(ids)
		return bumpContactCardState(ctx, tx, account, seq)
	})
	if err != nil {
		return 0, err
	}
	return touched, nil
}

// PruneCardsInBook tombstones live cards of one book whose hrefs are not
// in the fresh listing — the full-diff side of the getctag fallback
// (FR-P.2: "deletes, moves and ETag changes are all detected").
func (s *Store) PruneCardsInBook(ctx context.Context, account, bookID string, liveHrefs []string) (int, error) {
	touched := 0
	err := s.tx(ctx, account, true, func(tx *sql.Tx) error {
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		var rows *sql.Rows
		if len(liveHrefs) == 0 {
			rows, err = tx.QueryContext(ctx,
				`SELECT id FROM cards WHERE account = ? AND book_id = ? AND deleted IS NULL`,
				account, bookID)
		} else {
			ph, args := idPlaceholders(liveHrefs)
			rows, err = tx.QueryContext(ctx,
				`SELECT id FROM cards WHERE account = ? AND book_id = ? AND deleted IS NULL
				 AND href NOT IN (`+ph+`)`,
				append([]any{account, bookID}, args...)...)
		}
		if err != nil {
			return fmt.Errorf("store: prune scan: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		phIDs, argsIDs := idPlaceholders(ids)
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET deleted = ?, updated_modseq = ? WHERE account = ? AND id IN (`+phIDs+`)`,
			append([]any{seq, seq, account}, argsIDs...)...); err != nil {
			return fmt.Errorf("store: prune tombstone: %w", err)
		}
		touched = len(ids)
		return bumpContactCardState(ctx, tx, account, seq)
	})
	if err != nil {
		return 0, err
	}
	return touched, nil
}

// CardRefs lists the book's live cards as the store addresses them — the
// listing-diff input for the getctag fallback and the place the engine
// finds a card's stored etag for If-Match.
type CardRef struct {
	ID   string
	Href string
	ETag string
	UID  string
}

func (s *Store) CardRefs(ctx context.Context, account, bookID string) ([]CardRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, href, COALESCE(etag, ''), uid FROM cards
		 WHERE account = ? AND book_id = ? AND deleted IS NULL`, account, bookID)
	if err != nil {
		return nil, fmt.Errorf("store: card refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CardRef
	for rows.Next() {
		var r CardRef
		if err := rows.Scan(&r.ID, &r.Href, &r.ETag, &r.UID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BookAddress returns one book's href and write/ctag/sync-token state
// for the engine's write path (PUT targets, If-Match retry).
type BookAddress struct {
	ID        string
	Href      string
	CTag      string
	SyncToken string
	MayWrite  bool
}

func (s *Store) BookAddress(ctx context.Context, account, id string) (*BookAddress, error) {
	var b BookAddress
	var syncToken, ctag sql.NullString
	var deleted sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, href, sync_token, ctag, may_write, deleted FROM addressbooks
		 WHERE account = ? AND id = ?`, account, id).
		Scan(&b.ID, &b.Href, &syncToken, &ctag, &b.MayWrite, &deleted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: book address: %w", err)
	}
	b.SyncToken, b.CTag = syncToken.String, ctag.String
	return &b, nil
}

// BookAddressByHref is the reverse lookup the sync loop uses to map a
// discovery href onto the cache id.
func (s *Store) BookAddressByHref(ctx context.Context, account, href string) (*BookAddress, error) {
	var b BookAddress
	var syncToken, ctag sql.NullString
	var deleted sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, href, sync_token, ctag, may_write, deleted FROM addressbooks
		 WHERE account = ? AND href = ?`, account, href).
		Scan(&b.ID, &b.Href, &syncToken, &ctag, &b.MayWrite, &deleted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: book address by href: %w", err)
	}
	b.SyncToken, b.CTag = syncToken.String, ctag.String
	return &b, nil
}

// CardByUID looks a card up by its vCard UID — the existence test a
// create runs before touching the server (FR-P.8: uid collision →
// exists).
func (s *Store) CardByUID(ctx context.Context, account, uid string) (jmapapi.ContactCard, bool, error) {
	var id, js string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, jscontact FROM cards WHERE account = ? AND uid = ? AND deleted IS NULL`,
		account, uid).Scan(&id, &js)
	switch {
	case err == sql.ErrNoRows:
		return jmapapi.ContactCard{}, false, nil
	case err != nil:
		return jmapapi.ContactCard{}, false, fmt.Errorf("store: card by uid: %w", err)
	}
	books, err := cardBookIDs(ctx, s.db, account, id)
	if err != nil {
		return jmapapi.ContactCard{}, false, err
	}
	return jmapapi.ContactCard{ID: id, AddressBookIDs: books, Content: []byte(js)}, true, nil
}

// AddressBooksByID implements [jmapapi.Store] (FR-P.4).
func (s *Store) AddressBooksByID(ctx context.Context, account string, ids []string) ([]*jmapapi.AddressBook, string, []string, error) {
	state, err := s.AddressBookStateString(ctx, account)
	if err != nil {
		return nil, "", nil, err
	}
	query := `SELECT id, name, description, sort_order, may_read, may_write, may_share, may_delete
	          FROM addressbooks WHERE account = ? AND deleted IS NULL`
	args := []any{account}
	if ids != nil {
		ph, idArgs := idPlaceholders(ids)
		query += ` AND id IN (` + ph + `)`
		args = append(args, idArgs...)
	}
	query += ` ORDER BY sort_order, name, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", nil, fmt.Errorf("store: books by id: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*jmapapi.AddressBook
	got := map[string]bool{}
	for rows.Next() {
		var b jmapapi.AddressBook
		var name, desc sql.NullString
		var sortOrder sql.NullInt64
		var read, write, share, del int
		if err := rows.Scan(&b.ID, &name, &desc, &sortOrder, &read, &write, &share, &del); err != nil {
			return nil, "", nil, err
		}
		b.Name, b.Description = name.String, desc.String
		b.SortOrder = int(sortOrder.Int64)
		b.MayRead, b.MayWrite, b.MayShare, b.MayDelete = read == 1, write == 1, share == 1, del == 1
		out = append(out, &b)
		got[b.ID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, "", nil, err
	}
	var notFound []string
	for _, id := range ids {
		if !got[id] {
			notFound = append(notFound, id)
		}
	}
	return out, state, notFound, nil
}

// CardsByID implements [jmapapi.Store] (FR-P.6).
func (s *Store) CardsByID(ctx context.Context, account string, ids []string) ([]*jmapapi.ContactCard, string, []string, error) {
	state, err := s.ContactCardStateString(ctx, account)
	if err != nil {
		return nil, "", nil, err
	}
	query := `SELECT id, jscontact FROM cards WHERE account = ? AND deleted IS NULL`
	args := []any{account}
	if ids != nil {
		ph, idArgs := idPlaceholders(ids)
		query += ` AND id IN (` + ph + `)`
		args = append(args, idArgs...)
	}
	query += ` ORDER BY name, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", nil, fmt.Errorf("store: cards by id: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*jmapapi.ContactCard
	got := map[string]bool{}
	for rows.Next() {
		var c jmapapi.ContactCard
		var js string
		if err := rows.Scan(&c.ID, &js); err != nil {
			return nil, "", nil, err
		}
		c.Content = []byte(js)
		out = append(out, &c)
		got[c.ID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, "", nil, err
	}
	// Membership in one follow-up query: the rows cursor above holds the
	// single WAL connection, so per-card lookups inside the loop would
	// deadlock the pool (same reason emails.go batch-fetches mailbox
	// ids after closing rows).
	if len(out) > 0 {
		ids := make([]string, 0, len(out))
		for _, c := range out {
			ids = append(ids, c.ID)
		}
		ph, args := idPlaceholders(ids)
		mrows, err := s.db.QueryContext(ctx,
			`SELECT id, book_id FROM cards WHERE account = ? AND id IN (`+ph+`)`,
			append([]any{account}, args...)...)
		if err != nil {
			return nil, "", nil, fmt.Errorf("store: card books: %w", err)
		}
		byID := map[string]string{}
		for mrows.Next() {
			var id string
			var book sql.NullString
			if err := mrows.Scan(&id, &book); err != nil {
				_ = mrows.Close()
				return nil, "", nil, err
			}
			if book.String != "" {
				byID[id] = book.String
			}
		}
		if err := mrows.Err(); err != nil {
			_ = mrows.Close()
			return nil, "", nil, err
		}
		_ = mrows.Close()
		for _, c := range out {
			if b, ok := byID[c.ID]; ok {
				c.AddressBookIDs = []string{b}
			}
		}
	}
	var notFound []string
	for _, id := range ids {
		if !got[id] {
			notFound = append(notFound, id)
		}
	}
	return out, state, notFound, nil
}

func cardBookIDs(ctx context.Context, q queryer, account, cardID string) ([]string, error) {
	var bookID sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT book_id FROM cards WHERE account = ? AND id = ?`, account, cardID).Scan(&bookID)
	if err != nil {
		return nil, fmt.Errorf("store: card book: %w", err)
	}
	if bookID.String == "" {
		return nil, nil
	}
	return []string{bookID.String}, nil
}

// CardLocation is where the store addresses one card on the server:
// its book, href and last-known etag (the write path's If-Match input).
type CardLocation struct {
	ID     string
	BookID string
	Href   string
	ETag   string
	UID    string
}

func (s *Store) CardLocation(ctx context.Context, account, id string) (*CardLocation, error) {
	var l CardLocation
	var etag sql.NullString
	var deleted sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, book_id, href, etag, uid, deleted FROM cards WHERE account = ? AND id = ?`,
		account, id).Scan(&l.ID, &l.BookID, &l.Href, &etag, &l.UID, &deleted)
	if err == sql.ErrNoRows || deleted.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: card location: %w", err)
	}
	l.ETag = etag.String
	return &l, nil
}

// SaveBookSync stores a book's sync-token/ctag bookkeeping without
// touching any JMAP-visible state or modseq (PLAN §8: sync-token churn
// is not a client-visible change).
func (s *Store) SaveBookSync(ctx context.Context, account, id, syncToken, ctag string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE addressbooks SET sync_token = ?, ctag = ? WHERE account = ? AND id = ?`,
		nullStr(syncToken), nullStr(ctag), account, id)
	if err != nil {
		return fmt.Errorf("store: save book sync state: %w", err)
	}
	return nil
}

// AddressBookStateString is the AddressBook type state (the monotonic
// modseq floor behind it; PLAN §8).
func (s *Store) AddressBookStateString(ctx context.Context, account string) (string, error) {
	m, err := loadMeta(ctx, s.db, account)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(m.AddressBookState, 10), nil
}

// ContactCardStateString is the ContactCard type state.
func (s *Store) ContactCardStateString(ctx context.Context, account string) (string, error) {
	m, err := loadMeta(ctx, s.db, account)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(m.ContactCardState, 10), nil
}

func intVal(n sql.NullInt64) int { return int(n.Int64) }

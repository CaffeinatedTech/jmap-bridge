package sync

// The CardDAV half of the engine (PLAN §8, FR-P.1–.3): the contacts
// loop (open, sync, poll/kick, exponential backoff) and the DAV-first
// write path behind the jmapapi.Backend contacts seam. The same golden
// rules as mail: the server is touched before the cache (D-14), the
// capability appears only after the first full sync succeeded (FR-P.3),
// and state strings move only with server-confirmed content changes
// (golden rule 5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/dav"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/google/uuid"
)

// contactsLoop drives CardDAV sync until ctx ends: open the session,
// run full and incremental passes, and back off with the same ladder as
// the IMAP loop (FR-S.4's shape, reused honestly for a second protocol).
func (e *Engine) contactsLoop(ctx context.Context) {
	e.contactsKick <- struct{}{} // first pass now
	failures := 0
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			e.closeDAV()
			return
		}
		sess, err := e.davSession(ctx)
		if err != nil {
			e.log.Warn("sync: carddav open failed", "account", e.cfg.Account, "err", err)
			failures++
			if !e.sleepOrContactsKick(ctx, backoff(failures)) {
				return
			}
			continue
		}
		select {
		case <-e.contactsKick:
		case <-ticker.C:
		case <-ctx.Done():
			e.closeDAV()
			return
		}
		if err := e.syncContacts(ctx, sess); err != nil {
			e.log.Warn("sync: contacts pass failed", "account", e.cfg.Account, "err", err)
			failures++
			if !e.sleepOrContactsKick(ctx, backoff(failures)) {
				return
			}
			continue
		}
		failures = 0
		if !e.contactsReady.Load() {
			e.contactsReady.Store(true)
			e.log.Info("sync: contacts ready", "account", e.cfg.Account)
			// The session's capability set just changed (FR-P.3). The
			// pass itself moved the AddressBook/ContactCard state when
			// it ingested anything, and the SSE signal follows that
			// commit; a client that learns of the capability next
			// refetches the session when the response sessionState
			// diverges (RFC 8620 §3.4).
		}
	}
}

// sleepOrContactsKick waits d, waking early on kick or ctx end.
func (e *Engine) sleepOrContactsKick(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-e.contactsKick:
		return true
	}
}

// davSession returns the live session, opening one if needed.
func (e *Engine) davSession(ctx context.Context) (*dav.Session, error) {
	e.davMu.Lock()
	defer e.davMu.Unlock()
	if e.davSess != nil {
		return e.davSess, nil
	}
	sess, err := dav.Open(ctx, *e.cfg.CardDAV)
	if err != nil {
		return nil, err
	}
	e.davSess = sess
	e.log.Info("sync: carddav session open", "account", e.cfg.Account)
	return sess, nil
}

func (e *Engine) closeDAV() {
	e.davMu.Lock()
	defer e.davMu.Unlock()
	e.davSess = nil
}

// ContactsReady reports whether the contacts capability may be
// advertised (FR-P.3: CardDAV configured and the first sync succeeded).
func (e *Engine) ContactsReady() bool { return e.contactsReady.Load() }

// SyncContactsOnce runs one complete contacts pass (discovery +
// incremental sync per book) and marks the capability ready on the
// first success. The loop calls it; the live gate calls it directly,
// which keeps the production path — not a test re-implementation —
// what gets demonstrated.
func (e *Engine) SyncContactsOnce(ctx context.Context) error {
	sess, err := e.davSession(ctx)
	if err != nil {
		return err
	}
	if err := e.syncContacts(ctx, sess); err != nil {
		return err
	}
	if !e.contactsReady.Load() {
		e.contactsReady.Store(true)
		e.log.Info("sync: contacts ready", "account", e.cfg.Account)
	}
	return nil
}

// syncContacts runs one full pass over the account's books (FR-P.1,
// FR-P.2): discovery, then sync-collection (or the getctag/listing
// fallback) per book.
func (e *Engine) syncContacts(ctx context.Context, sess *dav.Session) error {
	discovered, err := sess.Books(ctx)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	recs := make([]store.BookRec, 0, len(discovered))
	for _, b := range discovered {
		recs = append(recs, store.BookRec{
			Href:        b.Href,
			Name:        b.Name,
			Description: b.Description,
			SortOrder:   b.SortOrder,
			MayWrite:    b.MayWrite,
			MayDelete:   b.MayDelete,
		})
	}
	if err := e.st.SyncBooks(ctx, e.cfg.Account, recs); err != nil {
		return fmt.Errorf("store books: %w", err)
	}
	for _, b := range discovered {
		if err := e.syncBook(ctx, sess, b); err != nil {
			// One book's failure is logged and retried next pass, like
			// a folder pass; only discovery failures unwind the loop.
			e.log.Warn("sync: contacts book failed", "book", b.Href, "err", err)
		}
	}
	return nil
}

func (e *Engine) syncBook(ctx context.Context, sess *dav.Session, b dav.Book) error {
	addr, err := e.st.BookAddressByHref(ctx, e.cfg.Account, b.Href)
	if err != nil {
		return err
	}
	if addr == nil {
		return fmt.Errorf("book %q vanished from the store", b.Href)
	}
	storedToken := addr.SyncToken
	outcome, err := sess.SyncBook(ctx, b.Href, storedToken)
	if err != nil {
		return err
	}
	if outcome.Supported {
		e.log.Debug("sync: book sync outcome", "href", b.Href, "changed", len(outcome.Changed), "deleted", len(outcome.Deleted), "token", outcome.SyncToken)
		if err := e.fetchCards(ctx, sess, addr.ID, outcome.Changed); err != nil {
			return err
		}
		if len(outcome.Deleted) > 0 {
			if _, err := e.st.TombstoneCardsByHrefs(ctx, e.cfg.Account, outcome.Deleted); err != nil {
				return err
			}
		}
		if outcome.SyncToken != "" && outcome.SyncToken != storedToken {
			return e.st.SaveBookSync(ctx, e.cfg.Account, addr.ID, outcome.SyncToken, addr.CTag)
		}
		return nil
	}
	return e.syncBookByListing(ctx, sess, addr, outcome.CTag)
}

// syncBookByListing is the getctag fallback (FR-P.2): when the ctag is
// unchanged since the stored one and the book has cards, nothing to do;
// otherwise a full listing diff by etag.
func (e *Engine) syncBookByListing(ctx context.Context, sess *dav.Session, addr *store.BookAddress, ctag string) error {
	refs, err := e.st.CardRefs(ctx, e.cfg.Account, addr.ID)
	if err != nil {
		return err
	}
	// Empty book with an empty stored ctag: still needs the first
	// listing to seed, so only skip when cards exist and the ctag held.
	if ctag != "" && addr.CTag == ctag && len(refs) > 0 {
		return nil
	}
	live, newCTag, err := sess.ListAll(ctx, addr.Href)
	if err != nil {
		return err
	}
	liveByHref := map[string]dav.CardMeta{}
	for _, m := range live {
		liveByHref[m.Href] = m
	}
	storedHref := map[string]string{} // href -> etag
	for _, r := range refs {
		storedHref[r.Href] = r.ETag
	}
	var changed []dav.CardMeta
	for _, m := range live {
		if etag, ok := storedHref[m.Href]; !ok || etag != m.ETag {
			changed = append(changed, m)
		}
	}
	// Cards the cache has but the listing does not are deletes or moves
	// into another book; this book's pass tombstones them and a later
	// (or already-done) pass of the target book re-creates them where
	// they live now.
	var gone []string
	for href := range storedHref {
		if _, ok := liveByHref[href]; !ok {
			gone = append(gone, href)
		}
	}
	if len(gone) > 0 {
		if _, err := e.st.TombstoneCardsByHrefs(ctx, e.cfg.Account, gone); err != nil {
			return err
		}
	}
	if err := e.fetchCards(ctx, sess, addr.ID, changed); err != nil {
		return err
	}
	if newCTag != addr.CTag {
		return e.st.SaveBookSync(ctx, e.cfg.Account, addr.ID, addr.SyncToken, newCTag)
	}
	return nil
}

// fetchCards pulls the changed bodies via one multiget per batch, then
// converts and stores (PLAN §8). Cards whose body is gone (deleted mid
// flight) are tombstoned.
func (e *Engine) fetchCards(ctx context.Context, sess *dav.Session, bookID string, changed []dav.CardMeta) error {
	if len(changed) == 0 {
		return nil
	}
	hrefs := make([]string, 0, len(changed))
	for _, c := range changed {
		hrefs = append(hrefs, c.Href)
	}
	cards, err := sess.MultiGet(ctx, bookHrefOf(changed), hrefs)
	if errors.Is(err, dav.ErrUnsupported) {
		cards, err = e.fetchOneByOne(ctx, sess, hrefs)
	}
	if err != nil {
		return err
	}
	found := map[string]dav.Card{}
	for _, c := range cards {
		found[c.Href] = c
	}
	var store_ []store.StoreCard
	var gone []string
	for _, m := range changed {
		if c, ok := found[m.Href]; ok {
			sc, err := e.buildStoreCard(ctx, c)
			if err != nil {
				e.log.Warn("sync: dropping malformed card", "href", c.Href, "err", err)
				continue
			}
			store_ = append(store_, sc)
			continue
		}
		// Not in the multiget: it was deleted between report and fetch.
		gone = append(gone, m.Href)
	}
	if len(gone) > 0 {
		if _, err := e.st.TombstoneCardsByHrefs(ctx, e.cfg.Account, gone); err != nil {
			return err
		}
	}
	if len(store_) == 0 {
		return nil
	}
	_, err = e.st.PutCards(ctx, e.cfg.Account, bookID, store_)
	return err
}

// fetchOneByOne is the no-multiget fallback: direct GETs (FR-P.2's
// "then GET for new hrefs" applied to a server without multiget).
func (e *Engine) fetchOneByOne(ctx context.Context, sess *dav.Session, hrefs []string) ([]dav.Card, error) {
	var out []dav.Card
	for _, h := range hrefs {
		c, err := sess.Get(ctx, h)
		if errors.Is(err, dav.ErrGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// buildStoreCard converts one wire card into the store row: canonical
// JSContact (with the photo moved to a blob reference), denormalised
// columns, and the last-known vCard bytes.
func (e *Engine) buildStoreCard(ctx context.Context, c dav.Card) (store.StoreCard, error) {
	contact, err := convert.VCFToContact(c.Raw)
	if err != nil {
		return store.StoreCard{}, err
	}
	uid := contact.UID
	if uid == "" {
		// A card without a UID gets one minted here and stays stable
		// through the "@vcard-uid-generated" marker (RFC 9610 §3 makes
		// the id the uid; degenerate providers still need an id).
		uid = uuid.NewString()
		contact.UID = uid
	}
	rec := store.StoreCard{
		UID:   uid,
		Kind:  contact.Kind,
		Name:  displayName(contact),
		Href:  c.Href,
		ETag:  c.ETag,
		VCard: string(c.Raw),
	}
	if photo := contact.InlinePhoto(); len(photo) > 0 {
		blobID, err := e.st.MintID(ctx)
		if err != nil {
			return store.StoreCard{}, err
		}
		media := contact.Photo.Type
		if media == "" {
			media = "image/jpeg"
		}
		contact.Photo.BlobID = blobID
		contact.Photo.Type = media
		contact.Photo.Size = int64(len(photo))
		contact.ClearInlinePhoto()
		rec.PhotoBlobID = blobID
		rec.PhotoBytes = photo
		rec.PhotoMediaType = media
	}
	js, err := json.Marshal(contact)
	if err != nil {
		return store.StoreCard{}, fmt.Errorf("encode JSContact: %w", err)
	}
	rec.JSContact = string(js)
	return rec, nil
}

// displayName denormalises the FN/name for listings and sort.
func displayName(c *convert.Contact) string {
	if c.Name != nil && c.Name.Full != "" {
		return c.Name.Full
	}
	if len(c.Members) > 0 && c.Kind == "group" {
		return "Group"
	}
	return ""
}

// bookHrefOf is a courtesy: multiget is scoped per book, and the
// changed list comes from one book's pass. The href's parent is that
// book. (Single-book callers keep this honest by passing one book's
// metas.)
func bookHrefOf(changed []dav.CardMeta) string {
	if len(changed) == 0 {
		return ""
	}
	h := changed[0].Href
	if i := strings.LastIndexByte(h, '/'); i > 0 {
		return h[:i+1]
	}
	return h
}

// --- Backend write path (D-14 for contacts: DAV first, cache second) ---

var errNoDAVSession = errors.New("sync: CardDAV session is not open")

// CreateContact builds the vCard, PUTs it with If-None-Match: * (uid
// collision → jmapapi.ErrContactExists, FR-P.8), then stores the row
// (store second, only after the server said created).
func (e *Engine) CreateContact(ctx context.Context, account string, spec jmapapi.ContactSpec) (string, error) {
	sess, err := e.davSession(ctx)
	if err != nil {
		return "", errNoDAVSession
	}
	book, err := e.st.BookAddress(ctx, account, spec.BookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", fmt.Errorf("%w: unknown book", jmapapi.ErrObjectNotFound)
	}
	if !book.MayWrite {
		return "", jmapapi.ErrNotWritable
	}
	contact, uid, err := e.prepareContact(ctx, account, &spec, "")
	if err != nil {
		return "", err
	}
	if _, exists, err := e.st.CardByUID(ctx, account, uid); err != nil {
		return "", err
	} else if exists {
		return "", jmapapi.ErrContactExists
	}
	vcf, err := convert.ContactToVCF(contact, spec.Photo)
	if err != nil {
		return "", err
	}
	href := dav.BookCardHref(book.Href, cardName(uid))
	etag, err := sess.Put(ctx, href, vcf, "*", "")
	if err != nil {
		if errors.Is(err, dav.ErrPrecondition) || errors.Is(err, dav.ErrConflict) {
			return "", jmapapi.ErrContactExists
		}
		return "", err
	}
	rec := store.StoreCard{
		UID: uid, Kind: contact.Kind, Name: displayName(contact),
		JSContact: string(spec.Content), VCard: string(vcf),
		Href: href, ETag: etag,
		PhotoBlobID:    spec.PhotoBlobID,
		PhotoBytes:     spec.Photo,
		PhotoMediaType: spec.PhotoMedia,
	}
	if err := e.st.CommitCardWrite(ctx, account, book.ID, rec); err != nil {
		return "", err
	}
	return uid, nil
}

// UpdateContact rebuilds the card, PUTs it under If-Match, and moves it
// when the book changed (FR-P.9, FR-P.13). The 412 path refetches and
// retries once, then reports jmapapi.ErrOverwritten.
func (e *Engine) UpdateContact(ctx context.Context, account, id string, spec jmapapi.ContactSpec) error {
	sess, err := e.davSession(ctx)
	if err != nil {
		return errNoDAVSession
	}
	loc, err := e.st.CardLocation(ctx, account, id)
	if err != nil {
		return err
	}
	if loc == nil {
		return jmapapi.ErrObjectNotFound
	}
	book, err := e.st.BookAddress(ctx, account, spec.BookID)
	if err != nil {
		return err
	}
	if book == nil {
		return fmt.Errorf("%w: unknown book", jmapapi.ErrObjectNotFound)
	}
	if !book.MayWrite {
		return jmapapi.ErrNotWritable
	}
	contact, uid, err := e.prepareContact(ctx, account, &spec, loc.UID)
	if err != nil {
		return err
	}
	vcf, err := convert.ContactToVCF(contact, spec.Photo)
	if err != nil {
		return err
	}

	moved := book.Href != "" && (loc.BookID != book.ID)
	target := loc.Href
	if moved {
		target = dav.BookCardHref(book.Href, cardName(uid))
	}

	etag, err := sess.Put(ctx, target, vcf, ifNoneMatch(moved), loc.ETag)
	if errors.Is(err, dav.ErrPrecondition) && moved {
		// The href may already hold this uid's stale copy from an
		// interrupted move: read it, replace it.
		c, gerr := sess.Get(ctx, target)
		if gerr == nil {
			etag, err = sess.Put(ctx, target, vcf, "", c.ETag)
		} else if errors.Is(gerr, dav.ErrGone) {
			etag, err = sess.Put(ctx, target, vcf, "", "")
		} else {
			err = gerr
		}
	}
	if errors.Is(err, dav.ErrPrecondition) && !moved {
		// Stale etag: one refetch-and-retry, then overwritten (FR-P.9).
		c, gerr := sess.Get(ctx, target)
		if gerr != nil {
			if errors.Is(gerr, dav.ErrGone) {
				return jmapapi.ErrObjectNotFound
			}
			return gerr
		}
		etag, err = sess.Put(ctx, target, vcf, "", c.ETag)
		if errors.Is(err, dav.ErrPrecondition) {
			return jmapapi.ErrOverwritten
		}
	}
	if err != nil {
		return err
	}

	if moved {
		// The new copy exists on the server; removing the old is the
		// second DAV step. An already-gone old copy is success.
		if derr := sess.Delete(ctx, loc.Href, loc.ETag); derr != nil && !errors.Is(derr, dav.ErrGone) {
			if errors.Is(derr, dav.ErrPrecondition) {
				// The old href changed under us; it is somebody else's
				// copy now (a genuine uid collision mid-move). Leave it
				// — the sync pass will resolve it — and keep the move.
				e.log.Warn("sync: move left stale copy (precondition)", "href", loc.Href)
			} else {
				return derr
			}
		}
	}
	rec := store.StoreCard{
		UID: uid, Kind: contact.Kind, Name: displayName(contact),
		JSContact: string(spec.Content), VCard: string(vcf),
		Href: target, ETag: etag,
		PhotoBlobID:    spec.PhotoBlobID,
		PhotoBytes:     spec.Photo,
		PhotoMediaType: spec.PhotoMedia,
	}
	return e.st.CommitCardWrite(ctx, account, book.ID, rec)
}

// DestroyContact DELETEs the card (If-Match when the store holds an
// etag), treating already-gone as success (FR-P.10), then tombstones.
func (e *Engine) DestroyContact(ctx context.Context, account, id string) error {
	sess, err := e.davSession(ctx)
	if err != nil {
		return errNoDAVSession
	}
	loc, err := e.st.CardLocation(ctx, account, id)
	if err != nil {
		return err
	}
	if loc == nil {
		return jmapapi.ErrObjectNotFound
	}
	if err := sess.Delete(ctx, loc.Href, loc.ETag); err != nil {
		if !errors.Is(err, dav.ErrGone) {
			if errors.Is(err, dav.ErrPrecondition) {
				c, gerr := sess.Get(ctx, loc.Href)
				if gerr != nil {
					if errors.Is(gerr, dav.ErrGone) {
						// It vanished between the etag we hold and the
						// DELETE: already-gone is success (FR-P.10).
						return e.st.CommitCardDestroy(ctx, account, id)
					}
					return gerr
				}
				if err := sess.Delete(ctx, loc.Href, c.ETag); err != nil && !errors.Is(err, dav.ErrGone) {
					return err
				}
			} else {
				return err
			}
		}
	}
	return e.st.CommitCardDestroy(ctx, account, id)
}

// prepareContact validates the content JSON enough to build the vCard
// and canonicalizes it: the uid is ensured (client-supplied, else the
// row's existing uid, else a fresh uuid — the row's id is the uid, so
// an update MUST NOT mint a new one, or the server sees the card as a
// foreign UID collision), written back into spec.Content, and the photo
// blob reference is resolved for the builder.
func (e *Engine) prepareContact(ctx context.Context, account string, spec *jmapapi.ContactSpec, uidFallback string) (*convert.Contact, string, error) {
	// Unmarshal through the convert model so the photo's blob id survives
	// the round-trip without the store needing to know the JSContact shape.
	content := spec.Content
	// The handler keeps content["photo"] holding {"blobId":...} (the
	// client's reference). Replace it with the canonical full reference
	// (blobId+type+size) so a later get answers it and a rebuild finds
	// the bytes.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(content, &obj); err != nil {
		return nil, "", fmt.Errorf("sync: contact content is not JSON: %w", err)
	}
	// Ensure the uid BEFORE any conversion: client uid, else the row's
	// (update), else fresh (create). It is written back into the content
	// so the stored JSContact, the built vCard and the row id always
	// agree (RFC 9610 §3's id = uid).
	uid := rawJSONString(obj["uid"])
	if uid == "" {
		uid = strings.TrimSpace(uidFallback)
	}
	if uid == "" {
		uid = uuid.NewString()
	}
	obj["uid"] = mustJSON(uid)
	if spec.PhotoRemoved {
		delete(obj, "photo")
	}
	if spec.PhotoBlobID != "" && spec.Photo != nil {
		obj["photo"] = mustJSON(map[string]any{
			"blobId": spec.PhotoBlobID,
			"type":   spec.PhotoMedia,
			"size":   len(spec.Photo),
		})
	} else if raw, ok := obj["photo"]; ok && string(raw) != "null" {
		// Update without a new upload: keep the stored reference but
		// load its bytes for the rebuild.
		var ref struct {
			BlobID string `json:"blobId"`
		}
		if json.Unmarshal(raw, &ref) == nil && ref.BlobID != "" && spec.Photo == nil {
			data, media, err := e.st.ReadBlob(ctx, account, ref.BlobID)
			if err != nil {
				return nil, "", fmt.Errorf("sync: photo blob unreadable: %w", err)
			}
			spec.Photo = data
			spec.PhotoMedia = media
			spec.PhotoBlobID = ref.BlobID
			obj["photo"] = mustJSON(map[string]any{
				"blobId": ref.BlobID, "type": media, "size": len(data),
			})
		}
	}
	content = mustJSON(obj)
	// The canonical content is what the store keeps and what the next
	// rebuild reads — set it once, after every mutation above.
	spec.Content = content
	contact, err := convert.UnmarshalContact(content)
	if err != nil {
		return nil, "", err
	}
	if contact.Kind == "" {
		contact.Kind = "individual"
	}
	return contact, uid, nil
}

// rawJSONString decodes a JSON string member, "" for absent/null/wrong.
func rawJSONString(raw []byte) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func ifNoneMatch(moved bool) string {
	if moved {
		return "*"
	}
	return ""
}

// cardName derives the server href name from a uid. UIDs travel in the
// urn:uuid form for bridge-minted ids; the name keeps ".vcf" (the
// convention every CardDAV client uses) and only sanitises characters
// the path cannot hold (RFC 6352 §4.2).
func cardName(uid string) string {
	name := strings.ReplaceAll(uid, "urn:uuid:", "")
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "card"
	}
	if !strings.HasSuffix(strings.ToLower(out), ".vcf") {
		out += ".vcf"
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

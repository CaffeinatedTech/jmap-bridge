package jmapapi

// Contacts methods: RFC 9610 AddressBook/ContactCard reads, /changes and
// the ContactCard/set write surface (FR-P.4–.13), plus the Backend seam
// the sync engine implements for DAV-first mutations (D-14's contacts
// half). Patch semantics follow RFC 8620 §5.3 with the whole-property
// form, JSON-pointer keys and null removals.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// ContactURN is the RFC 9610 contacts capability.
const ContactURN = "urn:ietf:params:jmap:contacts"

// Backend errors for the contacts seam.
var (
	// ErrContactExists: the create's uid collides with an existing card
	// (FR-P.8) → SetError "exists".
	ErrContactExists = errors.New("jmapapi: contact already exists")
	// ErrOverwritten: a stale-etag update could not be saved after one
	// refetch-retry (FR-P.9) → SetError "overwritten".
	ErrOverwritten = errors.New("jmapapi: the card was changed concurrently")
	// ErrNotWritable: the target book grants no write privileges →
	// SetError "invalidProperties".
	ErrNotWritable = errors.New("jmapapi: address book is not writable")
)

// ContactSpec is one validated ContactCard/set mutation (FR-P.8–.11).
// Content is the full JSContact content object (without id or
// addressBookIds — those are the store's row facts). Photo carries the
// bytes the handler resolved from a blobId reference; BookID is the
// single target book (membership moves re-target it).
type ContactSpec struct {
	BookID  string
	Content json.RawMessage

	Photo        []byte
	PhotoMedia   string
	PhotoBlobID  string
	PhotoRemoved bool
}

// --- AddressBook/get (FR-P.4) ---

func (h *Handler) addressBookGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args getArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	var ids []string
	if args.IDs != nil {
		ids = *args.IDs
		if len(ids) > maxObjectsInGet {
			return nil, methodErrorf("invalidArguments", "ids exceeds maxObjectsInGet (%d)", maxObjectsInGet)
		}
	}
	books, state, notFound, err := acct.Store.AddressBooksByID(ctx, acct.ID, ids)
	if err != nil {
		return nil, serverFail(err)
	}
	def := defaultBookID(books)
	list := make([]map[string]any, 0, len(books))
	for _, b := range books {
		list = append(list, filterProps(addressBookObject(b, b.ID == def), args.Properties))
	}
	resp := map[string]any{"accountId": acct.ID, "state": state, "list": list, "notFound": notFoundList(notFound)}
	return resp, nil
}

// defaultBookID names the account's primary address book: the first in
// the display order the client sorts with (sortOrder, name, id), which
// is what jmap-tui's engine asks for when it creates a card without
// naming a book.
func defaultBookID(books []*AddressBook) string {
	first := ""
	var best *AddressBook
	for _, b := range books {
		if best == nil || b.SortOrder < best.SortOrder ||
			(b.SortOrder == best.SortOrder && b.Name < best.Name) ||
			(b.SortOrder == best.SortOrder && b.Name == best.Name && b.ID < best.ID) {
			best = b
		}
	}
	if best != nil {
		first = best.ID
	}
	return first
}

func addressBookObject(b *AddressBook, isDefault bool) map[string]any {
	return map[string]any{
		"id":           b.ID,
		"name":         b.Name,
		"description":  b.Description,
		"sortOrder":    b.SortOrder,
		"isDefault":    isDefault,
		"isSubscribed": true,
		"mayRead":      b.MayRead,
		"mayWrite":     b.MayWrite,
		"mayShare":     b.MayShare,
		"mayDelete":    b.MayDelete,
		"myRights": map[string]bool{
			"mayRead":   b.MayRead,
			"mayWrite":  b.MayWrite,
			"mayShare":  b.MayShare,
			"mayDelete": b.MayDelete,
		},
	}
}

// --- ContactCard/get (FR-P.6) ---

func (h *Handler) contactCardGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args getArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	var ids []string
	if args.IDs != nil {
		ids = *args.IDs
		if len(ids) > maxObjectsInGet {
			return nil, methodErrorf("invalidArguments", "ids exceeds maxObjectsInGet (%d)", maxObjectsInGet)
		}
	}
	cards, state, notFound, err := acct.Store.CardsByID(ctx, acct.ID, ids)
	if err != nil {
		return nil, serverFail(err)
	}
	list := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		obj, merr := contactCardObject(c)
		if merr != nil {
			// A corrupt stored card must not poison the get; report it
			// as absent rather than answering broken JSON.
			notFound = append(notFound, c.ID)
			continue
		}
		list = append(list, filterProps(obj, args.Properties))
	}
	resp := map[string]any{"accountId": acct.ID, "state": state, "list": list, "notFound": notFoundList(notFound)}
	return resp, nil
}

// contactCardObject merges the row facts onto the canonical content JSON.
func contactCardObject(c *ContactCard) (map[string]any, *methodErr) {
	var content map[string]any
	if err := json.Unmarshal(c.Content, &content); err != nil {
		return nil, methodErrorf("serverFail", "stored card is not valid JSON")
	}
	content["id"] = c.ID
	books := map[string]bool{}
	for _, id := range c.AddressBookIDs {
		books[id] = true
	}
	content["addressBookIds"] = books
	if kind, ok := content["kind"].(string); !ok || kind == "" {
		content["kind"] = "individual"
	}
	if t, ok := content["@type"]; !ok || t == nil {
		content["@type"] = "Card"
	}
	if v, ok := content["version"]; !ok || v == nil {
		content["version"] = "1.0"
	}
	return content, nil
}

// --- ContactCard/set (FR-P.8–.13) ---

type contactSetArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   *[]string                  `json:"destroy"`
}

func (h *Handler) contactCardSet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args contactSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	n := len(args.Create) + len(args.Update)
	if args.Destroy != nil {
		n += len(*args.Destroy)
	}
	if n > maxObjectsInSet {
		return nil, methodErrorf("requestTooLarge",
			"create+update+destroy is %d, maxObjectsInSet is %d", n, maxObjectsInSet)
	}
	if acct.Backend == nil {
		return nil, serverFail(ErrNoWriteBackend)
	}
	states, err := acct.Store.States(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}
	oldState := states["ContactCard"]
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, methodErrorf("stateMismatch",
			"ifInState %s does not match the current ContactCard state", *args.IfInState)
	}

	resp := &SetResponse{AccountID: acct.ID, OldState: &oldState, NewState: oldState}

	for handle, obj := range args.Create {
		spec, merr := h.parseContactCreate(ctx, acct, obj)
		if merr != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = merr.set()
			continue
		}
		id, err := acct.Backend.CreateContact(ctx, acct.ID, *spec)
		if err != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = contactSetErr(err, "addressBookIds")
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[handle] = map[string]any{"id": id}
	}
	for id, obj := range args.Update {
		spec, merr := h.parseContactUpdate(ctx, acct, id, obj)
		if merr != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = merr.set()
			continue
		}
		if spec == nil {
			resp.markUpdated(id) // nothing to do — patch asked for nothing
			continue
		}
		if err := acct.Backend.UpdateContact(ctx, acct.ID, id, *spec); err != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = contactSetErr(err, "addressBookIds")
			continue
		}
		resp.markUpdated(id)
	}
	if args.Destroy != nil {
		for _, id := range *args.Destroy {
			if err := acct.Backend.DestroyContact(ctx, acct.ID, id); err != nil {
				if resp.NotDestroyed == nil {
					resp.NotDestroyed = map[string]SetError{}
				}
				resp.NotDestroyed[id] = contactSetErr(err, "")
				continue
			}
			resp.Destroyed = append(resp.Destroyed, id)
		}
	}

	states, err = acct.Store.States(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}
	resp.NewState = states["ContactCard"]
	return resp, nil
}

// checkBooksExist validates referenced address book ids against the
// account (unknown ids → invalidProperties naming addressBookIds,
// FR-P.4's id set being the only writable target).
func (h *Handler) checkBooksExist(ctx context.Context, acct *Account, ids []string) *methodErr {
	if len(ids) == 0 {
		return nil
	}
	_, _, notFound, err := acct.Store.AddressBooksByID(ctx, acct.ID, ids)
	if err != nil {
		return serverFail(err)
	}
	if len(notFound) > 0 {
		return &methodErr{
			Type:        "invalidProperties",
			Description: "unknown address book id " + strings.Join(notFound, ", "),
			Properties:  []string{"addressBookIds"},
		}
	}
	return nil
}

// contactSetErr maps a contacts backend failure onto the RFC 9610 /
// RFC 8620 §5.4 SetError codes.
func contactSetErr(err error, property string) SetError {
	switch {
	case errors.Is(err, ErrContactExists):
		return NewSetError("exists", nil, err.Error())
	case errors.Is(err, ErrOverwritten):
		return NewSetError("overwritten", nil, err.Error())
	case errors.Is(err, ErrNotWritable):
		return NewSetError("invalidProperties", []string{propertyOr(property, "addressBookIds")}, err.Error())
	default:
		return setErrFor(err, property)
	}
}

// parseContactCreate validates one create member (FR-P.8, FR-P.11,
// FR-P.13).
func (h *Handler) parseContactCreate(ctx context.Context, acct *Account, raw json.RawMessage) (*ContactSpec, *methodErr) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, methodErrorf("invalidArguments", "create must be a JSON object: %s", err)
	}
	for _, forbidden := range []string{"id"} {
		if v, ok := obj[forbidden]; ok && string(v) != "null" {
			return nil, &methodErr{
				Type:        "invalidProperties",
				Description: forbidden + " is server-set",
				Properties:  []string{forbidden},
			}
		}
	}
	if err := checkCardTypeFields(obj); err != nil {
		return nil, err
	}
	books, merr := contactBookTargets(obj)
	if merr != nil {
		return nil, merr
	}
	if len(books.trueIDs) > 1 {
		return nil, &methodErr{
			Type: "invalidArguments",
			Description: "a v0.1 card belongs to exactly one address book (CardDAV cannot express " +
				"multi-book membership): drop every id but " + books.first,
			Properties: []string{"addressBookIds"},
		}
	}
	if len(books.trueIDs) == 0 {
		return nil, &methodErr{
			Type:        "invalidProperties",
			Description: "addressBookIds must name at least one book",
			Properties:  []string{"addressBookIds"},
		}
	}
	content := stripWireOnly(obj, "id", "addressBookIds")
	spec := ContactSpec{BookID: books.first, Content: content}
	if err := h.checkBooksExist(ctx, acct, books.trueIDs); err != nil {
		return nil, err
	}
	if merr := h.resolvePhoto(ctx, acct, obj, &spec); merr != nil {
		return nil, merr
	}
	return &spec, nil
}

// parseContactUpdate merges one update patch onto the stored card
// (FR-P.9). A nil spec means the patch changed nothing observable.
func (h *Handler) parseContactUpdate(ctx context.Context, acct *Account, id string, raw json.RawMessage) (*ContactSpec, *methodErr) {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, methodErrorf("invalidPatch", "update must be a JSON object: %s", err)
	}
	cards, _, notFound, err := acct.Store.CardsByID(ctx, acct.ID, []string{id})
	if err != nil {
		return nil, serverFail(err)
	}
	if len(notFound) > 0 || len(cards) == 0 {
		return nil, &methodErr{Type: "notFound", Description: "unknown card id"}
	}
	cur := cards[0]
	var content map[string]any
	if err := json.Unmarshal(cur.Content, &content); err != nil {
		return nil, methodErrorf("serverFail", "stored card is not valid JSON")
	}
	delete(content, "id")
	delete(content, "addressBookIds")

	spec := &ContactSpec{BookID: firstOf(cur.AddressBookIDs)}
	moved := false
	removePhoto := false
	for key, val := range patch {
		base := key
		if i := strings.IndexByte(key, '/'); i >= 0 {
			base = key[:i]
		}
		switch base {
		case "id":
			return nil, immutableProp(key)
		case "uid", "@type", "version":
			return nil, immutableProp(key)
		case "addressBookIds":
			ids, merr := patchBookIDs(content, key, val, cur.AddressBookIDs)
			if merr != nil {
				return nil, merr
			}
			if len(ids.trueIDs) > 1 {
				return nil, &methodErr{
					Type: "invalidArguments",
					Description: "a v0.1 card belongs to exactly one address book (CardDAV cannot express " +
						"multi-book membership): drop every id but " + ids.first,
					Properties: []string{"addressBookIds"},
				}
			}
			if len(ids.trueIDs) == 0 {
				return nil, &methodErr{
					Type:        "invalidProperties",
					Description: "a card must belong to exactly one address book",
					Properties:  []string{"addressBookIds"},
				}
			}
			if err := h.checkBooksExist(ctx, acct, ids.trueIDs); err != nil {
				return nil, err
			}
			if ids.first != spec.BookID {
				moved = true
				spec.BookID = ids.first
			}
		case "photo":
			if string(val) == "null" {
				delete(content, "photo")
				removePhoto = true
				continue
			}
			content["photo"] = rawValue(val)
		default:
			applyPatchKey(content, key, val)
		}
	}
	// A photo replacement or removal travels to the engine as an
	// explicit instruction: content["photo"] either names a new blob or
	// is gone.
	photoJSON, _ := json.Marshal(content["photo"])
	var ref struct {
		BlobID string `json:"blobId"`
	}
	if len(photoJSON) > 0 && string(photoJSON) != "null" {
		_ = json.Unmarshal(photoJSON, &ref)
		if ref.BlobID != "" {
			if merr := h.resolvePhoto(ctx, acct,
				map[string]json.RawMessage{"photo": photoJSON}, spec); merr != nil {
				return nil, merr
			}
		}
	} else if removePhoto {
		spec.PhotoRemoved = true
	}

	out, err := json.Marshal(content)
	if err != nil {
		return nil, methodErrorf("serverFail", "could not encode updated card")
	}
	spec.Content = out
	if !moved && string(out) == string(cur.Content) && spec.Photo == nil && !spec.PhotoRemoved {
		return nil, nil
	}
	return spec, nil
}

// resolvePhoto checks the photo blob exists in this account and is an
// allowed image type within the size cap (FR-P.11), and loads its bytes
// for the engine.
func (h *Handler) resolvePhoto(ctx context.Context, acct *Account, obj map[string]json.RawMessage, spec *ContactSpec) *methodErr {
	rawPhoto, ok := obj["photo"]
	if !ok || string(rawPhoto) == "null" {
		return nil
	}
	var ref struct {
		BlobID string `json:"blobId"`
		Type   string `json:"type"`
	}
	if err := json.Unmarshal(rawPhoto, &ref); err != nil {
		return &methodErr{
			Type:        "invalidProperties",
			Description: "photo must be an object with a blobId",
			Properties:  []string{"photo"},
		}
	}
	if ref.BlobID == "" {
		return &methodErr{
			Type:        "invalidProperties",
			Description: "photo.blobId is required: contact photos are uploaded blobs (FR-P.11)",
			Properties:  []string{"photo"},
		}
	}
	data, media, err := acct.Store.ReadBlob(ctx, acct.ID, ref.BlobID)
	if err != nil {
		return &methodErr{Type: "blobNotFound", Description: err.Error()}
	}
	if !imageMediaType(media) {
		return &methodErr{
			Type:        "invalidProperties",
			Description: "photo must be a recognised image type (jpeg, png, webp, gif)",
			Properties:  []string{"photo"},
		}
	}
	if int64(len(data)) > maxContactPhotoBytes {
		return &methodErr{
			Type:        "invalidProperties",
			Description: "photo exceeds the size cap",
			Properties:  []string{"photo"},
		}
	}
	spec.Photo = data
	spec.PhotoMedia = media
	spec.PhotoBlobID = ref.BlobID
	return nil
}

// maxContactPhotoBytes is the photo size cap (PLAN §8 "photos capped to
// recognised image types with a size limit"; NFR-5's upload cap is the
// outer bound). 10 MiB is generous for an avatar, small enough that a
// book of hundreds stays a single-report fetch.
const maxContactPhotoBytes = 10 << 20

func imageMediaType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

// contactBooks reads the addressBookIds member of a create: the ids set
// true (sorted, first = the deterministic v0.1 target) and the account's
// known books for validation.
type contactBooks struct {
	trueIDs []string
	first   string
}

func contactBookTargets(obj map[string]json.RawMessage) (contactBooks, *methodErr) {
	var out contactBooks
	raw, ok := obj["addressBookIds"]
	if !ok || string(raw) == "null" {
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out, &methodErr{
			Type:        "invalidProperties",
			Description: "addressBookIds must be an object of id → true",
			Properties:  []string{"addressBookIds"},
		}
	}
	for id, v := range m {
		if on, _ := v.(bool); on {
			out.trueIDs = append(out.trueIDs, id)
		}
	}
	sort.Strings(out.trueIDs)
	if len(out.trueIDs) > 0 {
		out.first = out.trueIDs[0]
	}
	return out, nil
}

// patchBookIDs applies an addressBookIds patch (whole replacement or a
// single "addressBookIds/<id>" pointer) to the stored membership,
// returning the resulting true ids.
func patchBookIDs(content map[string]any, key string, val json.RawMessage, current []string) (contactBooks, *methodErr) {
	set := map[string]bool{}
	for _, id := range current {
		set[id] = true
	}
	if key == "addressBookIds" {
		var m map[string]any
		if string(val) == "null" {
			return contactBooks{}, &methodErr{
				Type:        "invalidProperties",
				Description: "addressBookIds must name exactly one book",
				Properties:  []string{"addressBookIds"},
			}
		}
		if err := json.Unmarshal(val, &m); err != nil {
			return contactBooks{}, &methodErr{
				Type:        "invalidProperties",
				Description: "addressBookIds must be an object of id → true",
				Properties:  []string{"addressBookIds"},
			}
		}
		set = map[string]bool{}
		for id, v := range m {
			if on, _ := v.(bool); on {
				set[id] = true
			}
		}
	} else {
		id := strings.TrimPrefix(key, "addressBookIds/")
		if id == "" || strings.Contains(id, "/") {
			return contactBooks{}, methodErrorf("invalidPatch", "bad pointer %q", key)
		}
		on, merr := patchBoolPointer(val)
		if merr != nil {
			return contactBooks{}, merr
		}
		if on {
			set[id] = true
		} else {
			delete(set, id)
		}
	}
	var out contactBooks
	for id := range set {
		out.trueIDs = append(out.trueIDs, id)
	}
	sort.Strings(out.trueIDs)
	if len(out.trueIDs) > 0 {
		out.first = out.trueIDs[0]
	}
	// Validate ids against the account's books; the handler needs the
	// store, which the caller threads through.
	return out, nil
}

func patchBoolPointer(val json.RawMessage) (bool, *methodErr) {
	if string(val) == "null" {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(val, &b); err != nil {
		return false, methodErrorf("invalidPatch", "addressBookIds values must be Booleans")
	}
	return b, nil
}

// applyPatchKey writes one RFC 8620 §5.3 patch key (property or JSON
// pointer) into the content map. null removes; pointers create the
// objects they traverse.
func applyPatchKey(content map[string]any, key string, val json.RawMessage) {
	segs := splitPointerPath(key)
	if string(val) == "null" {
		if len(segs) == 1 {
			delete(content, segs[0])
			return
		}
		parent := descend(content, segs[:len(segs)-1])
		if parent != nil {
			delete(parent, segs[len(segs)-1])
		}
		return
	}
	v := rawValue(val)
	if len(segs) == 1 {
		content[segs[0]] = v
		return
	}
	parent := descend(content, segs[:len(segs)-1])
	if parent == nil {
		return
	}
	parent[segs[len(segs)-1]] = v
}

func splitPointerPath(key string) []string {
	parts := strings.Split(key, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		out = append(out, p)
	}
	return out
}

func descend(m map[string]any, path []string) map[string]any {
	cur := m
	for _, seg := range path {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			if cur[seg] == nil {
				next = map[string]any{}
				cur[seg] = next
				continue
			}
			return nil
		}
		cur = next
	}
	return cur
}

func rawValue(val json.RawMessage) any {
	var v any
	if err := json.Unmarshal(val, &v); err != nil {
		return nil
	}
	return v
}

func checkCardTypeFields(obj map[string]json.RawMessage) *methodErr {
	if t, ok := obj["@type"]; ok && string(t) != `"Card"` && string(t) != "null" {
		return &methodErr{
			Type:        "invalidProperties",
			Description: `@type must be "Card"`,
			Properties:  []string{"@type"},
		}
	}
	if v, ok := obj["version"]; ok && string(v) != `"1.0"` && string(v) != "null" {
		return &methodErr{
			Type:        "invalidProperties",
			Description: `version must be "1.0"`,
			Properties:  []string{"version"},
		}
	}
	return nil
}

func immutableProp(key string) *methodErr {
	return &methodErr{
		Type:        "invalidProperties",
		Description: key + " is immutable or server-set",
		Properties:  []string{key},
	}
}

// stripWireOnly removes the id/addressBookIds wire members from a create
// object, leaving the content JSON the store keeps.
func stripWireOnly(obj map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, k := range keys {
		delete(obj, k)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

func firstOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

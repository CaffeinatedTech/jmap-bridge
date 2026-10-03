package jmapapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// maxObjectsInSet mirrors the value the session advertises (RFC 8620
// §2); exceeding it answers requestTooLarge instead of doing the work.
const maxObjectsInSet = 512

// --- Email/set (RFC 8621 §4.6, FR-M.9–.11) ---

type emailSetArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Create    map[string]json.RawMessage `json:"create"`
	Update    map[string]json.RawMessage `json:"update"`
	Destroy   *[]string                  `json:"destroy"`
}

func (h *Handler) emailSet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args emailSetArgs
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
	oldState := states["Email"]
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, methodErrorf("stateMismatch",
			"ifInState %s does not match the current Email state", *args.IfInState)
	}

	resp := &SetResponse{AccountID: acct.ID, OldState: &oldState, NewState: oldState}

	for handle, obj := range args.Create {
		created, merr := h.createDraft(ctx, acct, obj)
		if merr != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = merr.set()
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[handle] = created
	}
	for id, obj := range args.Update {
		patch, merr := parseEmailPatch(obj)
		if merr != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = merr.set()
			continue
		}
		if patch.Empty() {
			// A patch that asks for nothing still counts as updated
			// (RFC 8620 §5.3) — it just moves no state.
			resp.markUpdated(id)
			continue
		}
		if err := acct.Backend.ApplyEmailPatch(ctx, acct.ID, id, patch); err != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = setErrFor(err, patchErrorProperty(patch))
			continue
		}
		resp.markUpdated(id)
	}
	if args.Destroy != nil {
		for _, id := range *args.Destroy {
			if err := acct.Backend.DestroyEmails(ctx, acct.ID, id); err != nil {
				if resp.NotDestroyed == nil {
					resp.NotDestroyed = map[string]SetError{}
				}
				resp.NotDestroyed[id] = setErrFor(err, "")
				continue
			}
			resp.Destroyed = append(resp.Destroyed, id)
		}
	}

	// Read-your-writes: the state this response ends with must include
	// the mutations it just reported (FR-M.13).
	states, err = acct.Store.States(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}
	resp.NewState = states["Email"]
	return resp, nil
}

// patchErrorProperty names the property a membership-shaped failure
// blames (RFC 8620 §5.3 wants `properties` on invalidProperties).
func patchErrorProperty(p EmailPatch) string {
	if len(p.MailboxAdd) > 0 || len(p.MailboxRemove) > 0 || p.ReplaceMailboxes {
		return "mailboxIds"
	}
	if len(p.KeywordAdd) > 0 || len(p.KeywordRemove) > 0 || p.ReplaceKeywords {
		return "keywords"
	}
	return ""
}

// setErr maps a backend failure onto the SetError the RFCs define for
// it; anything unrecognised is serverFail carrying the server's own
// words (FR-M.13).
func setErrFor(err error, property string) SetError {
	var kwErr *KeywordError
	switch {
	case errors.As(err, &kwErr):
		props := make([]string, 0, len(kwErr.Keywords))
		for _, k := range kwErr.Keywords {
			props = append(props, "keywords/"+k)
		}
		return NewSetError("invalidArguments", props, kwErr.Error())
	case errors.Is(err, ErrObjectNotFound):
		return NewSetError("notFound", nil, "")
	case errors.Is(err, ErrMailboxExists):
		return NewSetError("alreadyExists", nil, "")
	case errors.Is(err, ErrMailboxHasChild):
		return NewSetError("mailboxHasChild", nil, "")
	case errors.Is(err, ErrMailboxHasEmail):
		return NewSetError("mailboxHasEmail", nil, "")
	case errors.Is(err, ErrOnDestroyRemoveEmails):
		return NewSetError("invalidProperties", []string{"onDestroyRemoveEmails"},
			"this account's provider cannot delete a mailbox's messages with it; delete the messages explicitly first")
	case errors.Is(err, ErrUnknownMailbox):
		return NewSetError("invalidProperties", []string{propertyOr(property, "mailboxIds")},
			"the mailbox does not exist in this account")
	case errors.Is(err, ErrWouldLeaveEmpty):
		return NewSetError("invalidProperties", []string{"mailboxIds"},
			"an Email must belong to at least one Mailbox at all times (RFC 8621 §4.1)")
	case errors.Is(err, ErrInvalidMailboxName):
		return NewSetError("invalidProperties", []string{"name"}, err.Error())
	case errors.Is(err, ErrBlobNotFound):
		se := NewSetError("blobNotFound", nil, err.Error())
		if blobID := blobIDOf(err.Error()); blobID != "" {
			se.NotFound = []string{blobID}
		}
		return se
	case errors.Is(err, ErrNoWriteBackend):
		return NewSetError("serverFail", nil, ErrNoWriteBackend.Error())
	default:
		return NewSetError("serverFail", nil, err.Error())
	}
}

// blobIDOf recovers the offending blob id from the wrapped message the
// backend builds ("<sentinel>: attachment blob <id>").
func blobIDOf(msg string) string {
	const marker = "attachment blob "
	if i := strings.LastIndex(msg, marker); i >= 0 {
		return strings.TrimSpace(msg[i+len(marker):])
	}
	return ""
}

func propertyOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// parseEmailPatch decodes one update member (RFC 8620 §5.3). Only the
// two set-shaped properties of an Email are patchable (RFC 8621 §4.6);
// everything else is an immutable or server-set property and fails
// with invalidProperties rather than being silently ignored.
func parseEmailPatch(raw json.RawMessage) (EmailPatch, *methodErr) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return EmailPatch{}, methodErrorf("invalidPatch", "update must be a JSON object: %s", err)
	}
	// "There MUST NOT be two patches where the pointer of one is the
	// prefix of the pointer of the other" (RFC 8620 §5.3).
	for key := range obj {
		if strings.Contains(key, "/") {
			base := key[:strings.IndexByte(key, '/')]
			if _, dup := obj[base]; dup {
				return EmailPatch{}, methodErrorf("invalidPatch",
					"patches %q and %q conflict (one is a prefix of the other)", base, key)
			}
		}
	}

	var p EmailPatch
	for key, val := range obj {
		switch {
		case key == "keywords":
			members, merr := decodeSetObject(key, val)
			if merr != nil {
				return EmailPatch{}, merr
			}
			p.ReplaceKeywords = true
			p.KeywordAdd = members
		case key == "mailboxIds":
			members, merr := decodeSetObject(key, val)
			if merr != nil {
				return EmailPatch{}, merr
			}
			p.ReplaceMailboxes = true
			p.MailboxAdd = members
		case strings.HasPrefix(key, "keywords/"):
			kw := strings.TrimPrefix(key, "keywords/")
			if kw == "" || strings.Contains(kw, "/") {
				return EmailPatch{}, methodErrorf("invalidPatch", "bad pointer %q", key)
			}
			on, merr := patchBool(val, key)
			if merr != nil {
				return EmailPatch{}, merr
			}
			if on {
				p.KeywordAdd = append(p.KeywordAdd, kw)
			} else {
				p.KeywordRemove = append(p.KeywordRemove, kw)
			}
		case strings.HasPrefix(key, "mailboxIds/"):
			id := strings.TrimPrefix(key, "mailboxIds/")
			if id == "" || strings.Contains(id, "/") {
				return EmailPatch{}, methodErrorf("invalidPatch", "bad pointer %q", key)
			}
			on, merr := patchBool(val, key)
			if merr != nil {
				return EmailPatch{}, merr
			}
			if on {
				p.MailboxAdd = append(p.MailboxAdd, id)
			} else {
				p.MailboxRemove = append(p.MailboxRemove, id)
			}
		case strings.Contains(key, "/"):
			// A pointer into a property that does not exist on Email.
			return EmailPatch{}, methodErrorf("invalidPatch",
				"Email has no property %q", key[:strings.IndexByte(key, '/')])
		default:
			return EmailPatch{}, &methodErr{
				Type:        "invalidProperties",
				Description: "Email/" + key + " is immutable or server-set; only keywords and mailboxIds may be patched",
				Properties:  []string{key},
			}
		}
	}
	return p, nil
}

// patchBool classifies a patch value: true adds, null removes (RFC
// 8620 §5.3), false also removes — Stalwart accepts the Boolean
// spelling and jmap-tui's own notes record Fastmail rejecting it, so
// accepting both never breaks a client while the canonical form still
// works everywhere (FR-M.9).
func patchBool(raw json.RawMessage, key string) (bool, *methodErr) {
	if string(raw) == "null" {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, &methodErr{
			Type:        "invalidProperties",
			Description: key + " must be a Boolean or null",
			Properties:  []string{key},
		}
	}
	return b, nil
}

// decodeSetObject reads the whole-property form (an entire keywords or
// mailboxIds object). Members are the keys whose value is true; keys
// with false or null are simply not members (RFC 8621 §4.1: values in
// these objects are true, so anything else means "absent").
func decodeSetObject(key string, raw json.RawMessage) ([]string, *methodErr) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &methodErr{
			Type:        "invalidProperties",
			Description: key + " must be an object",
			Properties:  []string{key},
		}
	}
	out := make([]string, 0, len(obj))
	for member, val := range obj {
		if string(val) == "null" {
			continue
		}
		var b bool
		if err := json.Unmarshal(val, &b); err != nil {
			return nil, &methodErr{
				Type:        "invalidProperties",
				Description: key + "/" + member + " must be a Boolean",
				Properties:  []string{key + "/" + member},
			}
		}
		if b {
			out = append(out, member)
		}
	}
	return out, nil
}

// --- Mailbox/set (RFC 8621 §2.5, FR-M.12) ---

type mailboxSetArgs struct {
	AccountID             string                     `json:"accountId"`
	IfInState             *string                    `json:"ifInState"`
	Create                map[string]json.RawMessage `json:"create"`
	Update                map[string]json.RawMessage `json:"update"`
	Destroy               *[]string                  `json:"destroy"`
	OnDestroyRemoveEmails bool                       `json:"onDestroyRemoveEmails"`
}

// mailboxCreate is the subset of Mailbox the bridge accepts on create.
// name and parentId are always honored. sortOrder (RFC 8621 §2,
// UnsignedInt) is honored on create: the bridge stores it and serves
// it until a later discovery pass re-derives it. role and isSubscribed
// are deliberately absent: role is derived from the server's
// SPECIAL-USE flags and isSubscribed is unmodelled — accepting a value
// we cannot keep would mislead the client (FR-M.12).
type mailboxCreate struct {
	Name      string  `json:"name"`
	ParentID  *string `json:"parentId"`
	SortOrder *int    `json:"sortOrder"`
}

func (h *Handler) mailboxSet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args mailboxSetArgs
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
	oldState := states["Mailbox"]
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, methodErrorf("stateMismatch",
			"ifInState %s does not match the current Mailbox state", *args.IfInState)
	}
	resp := &SetResponse{AccountID: acct.ID, OldState: &oldState, NewState: oldState}

	for handle, obj := range args.Create {
		id, merr := h.createMailbox(ctx, acct, obj)
		if merr != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = merr.set()
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[handle] = h.mailboxCreated(ctx, acct, id)
	}
	for id, obj := range args.Update {
		name, parentID, merr := h.parseMailboxUpdate(ctx, acct, id, obj)
		if merr != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = merr.set()
			continue
		}
		if err := acct.Backend.RenameMailbox(ctx, acct.ID, id, name, parentID); err != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[id] = setErrFor(err, "")
			continue
		}
		resp.markUpdated(id)
	}
	if args.Destroy != nil {
		for _, id := range *args.Destroy {
			if err := acct.Backend.DestroyMailbox(ctx, acct.ID, id, args.OnDestroyRemoveEmails); err != nil {
				if resp.NotDestroyed == nil {
					resp.NotDestroyed = map[string]SetError{}
				}
				resp.NotDestroyed[id] = setErrFor(err, "")
				continue
			}
			resp.Destroyed = append(resp.Destroyed, id)
		}
	}

	states, err = acct.Store.States(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}
	resp.NewState = states["Mailbox"]
	return resp, nil
}

func (h *Handler) createMailbox(ctx context.Context, acct *Account, raw json.RawMessage) (string, *methodErr) {
	var c mailboxCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", methodErrorf("invalidArguments", "%s", err)
	}
	if merr := rejectMailboxProps(raw, true); merr != nil {
		return "", merr
	}
	if strings.TrimSpace(c.Name) == "" {
		return "", &methodErr{
			Type: "invalidProperties", Description: "name is required",
			Properties: []string{"name"},
		}
	}
	parent := ""
	if c.ParentID != nil {
		parent = *c.ParentID
	}
	sortOrder := 0
	if c.SortOrder != nil {
		if *c.SortOrder < 0 {
			return "", &methodErr{
				Type:        "invalidProperties",
				Description: "sortOrder must not be negative",
				Properties:  []string{"sortOrder"},
			}
		}
		sortOrder = *c.SortOrder
	}
	id, err := acct.Backend.CreateMailbox(ctx, acct.ID, c.Name, parent, sortOrder)
	if err != nil {
		return "", backendMethodErr(err, "name")
	}
	return id, nil
}

// rejectMailboxProps refuses the Mailbox properties the bridge derives
// from the server instead of accepting a value it cannot keep (see
// mailboxCreate). isCreate loosens the guard for sortOrder only: a
// client-requested order is stored at create time and honored until a
// later discovery pass re-derives it; on update it is still refused.
func rejectMailboxProps(raw json.RawMessage, isCreate bool) *methodErr {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return methodErrorf("invalidArguments", "%s", err)
	}
	var bad []string
	for key := range obj {
		switch key {
		case "name", "parentId":
		case "sortOrder":
			if !isCreate {
				bad = append(bad, key)
			}
		default:
			bad = append(bad, key)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return &methodErr{
		Type:        "invalidProperties",
		Description: "the bridge derives these from the server: " + strings.Join(bad, ", "),
		Properties:  bad,
	}
}

// parseMailboxUpdate merges a patch into the mailbox's current name and
// parent: a patch may carry either or both (RFC 8620 §5.3).
func (h *Handler) parseMailboxUpdate(ctx context.Context, acct *Account, id string, raw json.RawMessage) (name, parentID string, merr *methodErr) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", "", methodErrorf("invalidArguments", "%s", err)
	}
	if bad := rejectMailboxProps(raw, false); bad != nil {
		return "", "", bad
	}
	mbs, _, notFound, err := acct.Store.MailboxesByID(ctx, acct.ID, []string{id})
	if err != nil {
		return "", "", serverFail(err)
	}
	if len(notFound) > 0 || len(mbs) == 0 {
		return "", "", &methodErr{Type: "notFound", Description: "unknown mailbox id"}
	}
	name, parentID = mbs[0].Name, mbs[0].ParentID

	for key, val := range obj {
		switch key {
		case "name":
			var n string
			if err := json.Unmarshal(val, &n); err != nil {
				return "", "", &methodErr{
					Type: "invalidProperties", Description: "name must be a String",
					Properties: []string{"name"},
				}
			}
			if strings.TrimSpace(n) == "" {
				return "", "", &methodErr{
					Type: "invalidProperties", Description: "name must not be empty",
					Properties: []string{"name"},
				}
			}
			name = n
		case "parentId":
			// null removes the parent (top level); a string reparents.
			if string(val) == "null" {
				parentID = ""
				continue
			}
			var p string
			if err := json.Unmarshal(val, &p); err != nil {
				return "", "", &methodErr{
					Type: "invalidProperties", Description: "parentId must be an Id or null",
					Properties: []string{"parentId"},
				}
			}
			parentID = p
		}
	}
	return name, parentID, nil
}

// mailboxCreated answers with the created object, which RFC 8620 §5.3
// wants to carry the server-set properties (at minimum `id`).
func (h *Handler) mailboxCreated(ctx context.Context, acct *Account, id string) map[string]any {
	mbs, _, _, err := acct.Store.MailboxesByID(ctx, acct.ID, []string{id})
	if err != nil || len(mbs) == 0 {
		return map[string]any{"id": id}
	}
	return mailboxObject(mbs[0], accountOffers(acct, SubmissionURN))
}

// resolveMailboxRefs resolves each "#handle" mailbox id against an
// earlier creation in the same request (RFC 8620 §5.3): a draft created
// after its target mailbox may name it by creation id. A plain id, or a
// reference the batch cannot resolve, travels unchanged for the backend
// to validate.
func resolveMailboxRefs(ctx context.Context, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if resolved, ok := resolveCreationRef(ctx, id); ok {
			out = append(out, resolved)
			continue
		}
		out = append(out, id)
	}
	return out
}

// backendMethodErr converts a create/update failure into a methodErr
// (create paths want a *methodErr, not a SetError, so they can carry
// properties).
func backendMethodErr(err error, property string) *methodErr {
	se := setErrFor(err, property)
	out := &methodErr{Type: se.Type, Description: derefStr(se.Description)}
	if len(se.Properties) > 0 {
		out.Properties = se.Properties
	}
	return out
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- shared /set plumbing ---

// draftObject is the Email/set create shape the bridge accepts, decoded
// leniently so §4.6's constraints can be checked with the whole object
// in view.
type draftObject struct {
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`

	From, To, Cc, Bcc, ReplyTo []Address
	Subject                    string
	InReplyTo                  []string `json:"inReplyTo"`
	References                 []string `json:"references"`
	ReceivedAt                 *time.Time

	TextBody      []partRef          `json:"textBody"`
	HTMLBody      []partRef          `json:"htmlBody"`
	Attachments   []attachmentRef    `json:"attachments"`
	BodyStructure json.RawMessage    `json:"bodyStructure"`
	BodyValues    map[string]bodyVal `json:"bodyValues"`

	// Forbidden or server-set on create (RFC 8621 §4.6, RFC 8620 §5.3).
	Headers  json.RawMessage `json:"headers"`
	ID       *string         `json:"id"`
	ThreadID *string         `json:"threadId"`
}

type partRef struct {
	PartID  string `json:"partId"`
	BlobID  string `json:"blobId"`
	Type    string `json:"type"`
	Charset string `json:"charset"`
	Size    *int64 `json:"size"`
}

type attachmentRef struct {
	PartID      string `json:"partId"`
	BlobID      string `json:"blobId"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Disposition string `json:"disposition"`
	Size        *int64 `json:"size"`
}

type bodyVal struct {
	Value           string `json:"value"`
	IsTruncated     *bool  `json:"isTruncated"`
	EncodingProblem *bool  `json:"isEncodingProblem"`
}

// createDraft validates one create member and hands it to the backend.
func (h *Handler) createDraft(ctx context.Context, acct *Account, raw json.RawMessage) (*CreatedEmail, *methodErr) {
	var d draftObject
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := validateDraft(&d); merr != nil {
		return nil, merr
	}
	spec := DraftSpec{
		MailboxIDs: resolveMailboxRefs(ctx, trueKeys(d.MailboxIDs)),
		From:       d.From, To: d.To, Cc: d.Cc, Bcc: d.Bcc, ReplyTo: d.ReplyTo,
		Subject:    d.Subject,
		Keywords:   d.Keywords,
		InReplyTo:  d.InReplyTo,
		References: d.References,
	}
	if d.ReceivedAt != nil {
		spec.ReceivedAt = *d.ReceivedAt
	}
	parts, merr := draftParts(&d)
	if merr != nil {
		return nil, merr
	}
	spec.Parts = parts

	created, err := acct.Backend.CreateDraft(ctx, acct.ID, spec)
	if err != nil {
		return nil, backendMethodErr(err, "mailboxIds")
	}
	return created, nil
}

// validateDraft enforces the RFC 8621 §4.6 create constraints the
// bridge cannot satisfy by interpretation; violations are
// invalidProperties (the RFC says SHOULD — the alternative, silently
// reshaping the client's message, would make what we store a guess).
func validateDraft(d *draftObject) *methodErr {
	if len(d.Headers) > 0 {
		return &methodErr{
			Type: "invalidProperties",
			Description: "headers must not be given on create; set each header field " +
				"as an individual property (RFC 8621 §4.6)",
			Properties: []string{"headers"},
		}
	}
	if d.ID != nil {
		return &methodErr{
			Type: "invalidProperties", Description: "id is server-set",
			Properties: []string{"id"},
		}
	}
	if d.ThreadID != nil {
		return &methodErr{
			Type: "invalidProperties", Description: "threadId is server-set",
			Properties: []string{"threadId"},
		}
	}
	hasBodyProps := len(d.TextBody) > 0 || len(d.HTMLBody) > 0 || len(d.Attachments) > 0
	if len(d.BodyStructure) > 0 && hasBodyProps {
		return &methodErr{
			Type: "invalidProperties",
			Description: "bodyStructure must not be combined with textBody, htmlBody " +
				"or attachments (RFC 8621 §4.6)",
			Properties: []string{"bodyStructure"},
		}
	}
	if len(d.TextBody) > 1 {
		return invalidProps("textBody", "textBody must contain exactly one part")
	}
	if len(d.HTMLBody) > 1 {
		return invalidProps("htmlBody", "htmlBody must contain exactly one part")
	}
	if len(d.TextBody) == 1 {
		p := d.TextBody[0]
		if p.PartID == "" || p.BlobID != "" {
			return invalidProps("textBody", "the text part must carry a partId and no blobId")
		}
		if t := strings.ToLower(p.Type); t != "" && t != "text/plain" {
			return invalidProps("textBody", "the text part must be text/plain")
		}
		if _, ok := d.BodyValues[p.PartID]; !ok {
			return invalidProps("bodyValues", "missing body value for partId "+p.PartID)
		}
	}
	if len(d.HTMLBody) == 1 {
		p := d.HTMLBody[0]
		if p.PartID == "" || p.BlobID != "" {
			return invalidProps("htmlBody", "the html part must carry a partId and no blobId")
		}
		if t := strings.ToLower(p.Type); t != "" && t != "text/html" {
			return invalidProps("htmlBody", "the html part must be text/html")
		}
		if _, ok := d.BodyValues[p.PartID]; !ok {
			return invalidProps("bodyValues", "missing body value for partId "+p.PartID)
		}
	}
	for _, a := range d.Attachments {
		if a.BlobID == "" {
			return invalidProps("attachments", "attachment parts must reference a blobId")
		}
		if a.PartID != "" {
			return invalidProps("attachments", "a part may carry a partId or a blobId, not both")
		}
	}
	for _, v := range d.BodyValues {
		if v.IsTruncated != nil && *v.IsTruncated {
			return invalidProps("bodyValues", "isTruncated must be false or omitted on create")
		}
		if v.EncodingProblem != nil && *v.EncodingProblem {
			return invalidProps("bodyValues", "isEncodingProblem must be false or omitted on create")
		}
	}
	return nil
}

func invalidProps(prop, desc string) *methodErr {
	return &methodErr{Type: "invalidProperties", Description: desc, Properties: []string{prop}}
}

// draftParts flattens the create's body into the ordered part list the
// builder materialises: textBody/htmlBody plus attachments when given,
// otherwise the client's bodyStructure tree walked in document order.
func draftParts(d *draftObject) ([]DraftPart, *methodErr) {
	if len(d.BodyStructure) > 0 {
		return structureParts(d)
	}
	var out []DraftPart
	for _, p := range d.TextBody {
		out = append(out, DraftPart{
			Type: "text/plain", Charset: p.Charset, Text: d.BodyValues[p.PartID].Value,
		})
	}
	for _, p := range d.HTMLBody {
		out = append(out, DraftPart{
			Type: "text/html", Charset: p.Charset, Text: d.BodyValues[p.PartID].Value,
		})
	}
	for _, a := range d.Attachments {
		out = append(out, DraftPart{
			Type:        firstNonEmpty(a.Type, "application/octet-stream"),
			Disposition: firstNonEmpty(a.Disposition, "attachment"),
			Name:        a.Name,
			BlobID:      a.BlobID,
		})
	}
	return out, nil
}

// structureParts walks a client-supplied bodyStructure (RFC 8621 §4.6
// allows it instead of textBody/htmlBody/attachments). The container
// nesting is normalised by the builder; only the leaves travel.
func structureParts(d *draftObject) ([]DraftPart, *methodErr) {
	var root any
	if err := json.Unmarshal(d.BodyStructure, &root); err != nil {
		return nil, invalidProps("bodyStructure", "bodyStructure must be a JSON object: "+err.Error())
	}
	var out []DraftPart
	var walk func(node any) *methodErr
	walk = func(node any) *methodErr {
		obj, ok := node.(map[string]any)
		if !ok {
			return invalidProps("bodyStructure", "every part must be an object")
		}
		if subs, has := obj["subParts"]; has {
			list, ok := subs.([]any)
			if !ok {
				return invalidProps("bodyStructure", "subParts must be an array")
			}
			for _, s := range list {
				if merr := walk(s); merr != nil {
					return merr
				}
			}
			return nil
		}
		part := DraftPart{
			Type:        strOf(obj["type"]),
			Disposition: strOf(obj["disposition"]),
			Name:        strOf(obj["name"]),
			Charset:     strOf(obj["charset"]),
		}
		partID, hasPartID := obj["partId"].(string)
		blobID, hasBlobID := obj["blobId"].(string)
		switch {
		case hasPartID && partID != "" && hasBlobID && blobID != "":
			return invalidProps("bodyStructure", "a part may carry a partId or a blobId, not both")
		case hasPartID && partID != "":
			val, ok := d.BodyValues[partID]
			if !ok {
				return invalidProps("bodyValues", "missing body value for partId "+partID)
			}
			part.Text = val.Value
		case hasBlobID && blobID != "":
			part.BlobID = blobID
		default:
			return invalidProps("bodyStructure", "every leaf part needs a partId or a blobId")
		}
		out = append(out, part)
		return nil
	}
	if merr := walk(root); merr != nil {
		return nil, merr
	}
	return out, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func trueKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	return out
}

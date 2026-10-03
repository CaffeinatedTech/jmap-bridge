package jmapapi

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
	"unicode/utf8"
)

// method looks up a dispatchable method; nil means unknownMethod
// (FR-J.2).
func (h *Handler) method(name string) methodFunc {
	switch name {
	case "Core/echo":
		return coreEcho
	case "Mailbox/get":
		return h.mailboxGet
	case "Mailbox/query":
		return h.mailboxQuery
	case "Mailbox/changes":
		return changesMethod("Mailbox")
	case "Mailbox/set":
		return h.mailboxSet
	case "Email/get":
		return h.emailGet
	case "Email/query":
		return h.emailQuery
	case "Email/changes":
		return changesMethod("Email")
	case "Email/set":
		return h.emailSet
	case "Email/import":
		return h.emailImport
	case "Thread/get":
		return h.threadGet
	case "Identity/get":
		return h.identityGet
	case "EmailSubmission/set":
		return h.emailSubmissionSet
	case "AddressBook/get":
		return h.addressBookGet
	case "AddressBook/changes":
		return changesMethod("AddressBook")
	case "ContactCard/get":
		return h.contactCardGet
	case "ContactCard/changes":
		return changesMethod("ContactCard")
	case "ContactCard/set":
		return h.contactCardSet
	default:
		return nil
	}
}

// coreEcho implements Core/echo (RFC 8620 §3.1): the method echoes its
// arguments object back unchanged, and an absent object echoes as `{}`.
// It is mandatory for every JMAP server and needs no account.
func coreEcho(_ context.Context, _ *Account, raw json.RawMessage) (any, *methodErr) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "arguments must be a JSON object")
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// checkAccount validates the accountId argument: required, and equal to
// the account the request was routed for. An absent accountId is
// invalidArguments; a present one that is not this account is
// accountNotFound — identical whether the account exists or not, so a
// caller cannot probe other accounts through method errors (FR-A.11,
// RFC 8620 §3.6.2).
func checkAccount(acct *Account, id string) *methodErr {
	if id == "" {
		return methodErrorf("invalidArguments", "accountId is required")
	}
	if id != acct.ID {
		return methodErrorf("accountNotFound", "unknown accountId")
	}
	return nil
}

// --- Mailbox/get (FR-M.1) ---

type getArgs struct {
	AccountID string `json:"accountId"`

	IDs *[]string `json:"ids"`

	Properties     *[]string `json:"properties"`
	BodyProperties *[]string `json:"bodyProperties"`

	FetchTextBodyValues bool `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool `json:"fetchAllBodyValues"`

	// MaxBodyValueBytes, when set, caps each returned body value at that
	// many octets and marks it truncated (RFC 8621 §4.1.4).
	MaxBodyValueBytes int `json:"maxBodyValueBytes"`
}

func (h *Handler) mailboxGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
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
	mbs, state, notFound, err := acct.Store.MailboxesByID(ctx, acct.ID, ids)
	if err != nil {
		return nil, serverFail(err)
	}
	list := make([]map[string]any, 0, len(mbs))
	for _, mb := range mbs {
		list = append(list, filterProps(mailboxObject(mb, accountOffers(acct, SubmissionURN)), args.Properties))
	}
	resp := map[string]any{"accountId": acct.ID, "state": state, "list": list, "notFound": notFoundList(notFound)}
	return resp, nil
}

func mailboxObject(mb *Mailbox, maySubmit bool) map[string]any {
	obj := map[string]any{
		"id":            mb.ID,
		"name":          mb.Name,
		"sortOrder":     mb.SortOrder,
		"totalEmails":   mb.TotalEmails,
		"unreadEmails":  mb.UnreadEmails,
		"totalThreads":  mb.TotalThreads,
		"unreadThreads": mb.UnreadThreads,
		"isSubscribed":  true,
		"myRights": map[string]bool{
			"mayReadItems":   mb.MayRead,
			"mayAddItems":    mb.MayAddItems,
			"mayRemoveItems": mb.MayRemoveItems,
			"maySetSeen":     mb.MayRead,
			"maySetKeywords": mb.MayAddItems,
			"mayCreateChild": mb.MayCreateChild,
			"mayRename":      mb.MayRename,
			"mayDelete":      mb.MayDelete,
			"maySubmit":      maySubmit,
		},
	}
	if mb.ParentID != "" {
		obj["parentId"] = mb.ParentID
	} else {
		obj["parentId"] = nil
	}
	if mb.Role != "" {
		obj["role"] = mb.Role
	}
	return obj
}

// --- Mailbox/query (FR-M.2) ---

// nullableString distinguishes JSON null from an absent key, which
// matters for Mailbox/query's parentId/role filters (null filters for
// "no value", absent applies no filter).
type nullableString struct {
	set bool
	val string
}

func (n *nullableString) UnmarshalJSON(b []byte) error {
	n.set = true
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &n.val)
}

type mailboxFilter struct {
	ParentID nullableString `json:"parentId"`
	Role     nullableString `json:"role"`
}

type sortArg struct {
	Property     string `json:"property"`
	IsAscending  bool   `json:"isAscending"`
	IsDescending bool   `json:"isDescending"`
}

type mailboxQueryArgs struct {
	AccountID string        `json:"accountId"`
	Filter    mailboxFilter `json:"filter"`
	Sort      []sortArg     `json:"sort"`
	pageArgs
}

// pageArgs is the paging shape shared by Mailbox/query and Email/query.
type pageArgs struct {
	Position       int    `json:"position"`
	Anchor         string `json:"anchor"`
	AnchorOffset   int    `json:"anchorOffset"`
	Limit          int    `json:"limit"`
	CalculateTotal bool   `json:"calculateTotal"`
	// CollapseThreads is Email/query-only; accepted in the shared shape
	// so one decoder serves both.
	CollapseThreads bool `json:"collapseThreads"`
}

func (h *Handler) mailboxQuery(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args mailboxQueryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	mbs, state, err := acct.Store.Mailboxes(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}

	filtered := make([]*Mailbox, 0, len(mbs))
	for _, mb := range mbs {
		if args.Filter.ParentID.set {
			if args.Filter.ParentID.val == "" {
				if mb.ParentID != "" {
					continue
				}
			} else if mb.ParentID != args.Filter.ParentID.val {
				continue
			}
		}
		if args.Filter.Role.set && mb.Role != args.Filter.Role.val {
			continue
		}
		filtered = append(filtered, mb)
	}

	less, merr := mailboxSortLess(args.Sort)
	if merr != nil {
		return nil, merr
	}
	sort.SliceStable(filtered, func(i, j int) bool { return less(filtered[i], filtered[j]) })

	ids := make([]string, 0, len(filtered))
	for _, mb := range filtered {
		ids = append(ids, mb.ID)
	}
	position, window := paginate(ids, args.Anchor, args.AnchorOffset, args.Position, capQueryLimit(args.Limit))

	resp := map[string]any{
		"accountId":           acct.ID,
		"queryState":          queryState(state, raw),
		"canCalculateChanges": false,
		"position":            position,
		"ids":                 window,
	}
	if args.CalculateTotal {
		resp["total"] = len(ids)
	}
	return resp, nil
}

// mailboxSortLess builds the comparator for Mailbox/query. Supported
// properties: sortOrder (default), name, id (FR-M.2).
func mailboxSortLess(sortArgs []sortArg) (func(a, b *Mailbox) bool, *methodErr) {
	prop := "sortOrder"
	ascending := true
	if len(sortArgs) > 0 {
		prop = sortArgs[0].Property
		ascending = sortArgs[0].IsAscending && !sortArgs[0].IsDescending
	}
	switch prop {
	case "sortOrder":
		// fall through to the shared body with name as tiebreak
	case "name":
		return func(a, b *Mailbox) bool {
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.ID < b.ID
		}, nil
	case "id":
		return func(a, b *Mailbox) bool { return a.ID < b.ID }, nil
	default:
		return nil, methodErrorf("invalidArguments", "unsupported sort property %q for Mailbox", prop)
	}
	return func(a, b *Mailbox) bool {
		if a.SortOrder != b.SortOrder {
			if ascending {
				return a.SortOrder < b.SortOrder
			}
			return a.SortOrder > b.SortOrder
		}
		if a.Name != b.Name {
			if ascending {
				return a.Name < b.Name
			}
			return a.Name > b.Name
		}
		return a.ID < b.ID
	}, nil
}

// paginate applies anchor/anchorOffset (which replace position) and
// limit, returning the starting position and the window. A negative
// position counts from the end of the list (RFC 8620 §5.5). An anchor
// id that is no longer in the result set clamps to the end of the list
// — the tolerant behaviour jmap-tui's mock server implements and its
// window repair relies on.
func paginate(ids []string, anchor string, anchorOffset, position, limit int) (int, []string) {
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

// --- Email/query (FR-M.5) ---

type emailFilterArg struct {
	InMailbox     string     `json:"inMailbox"`
	Text          string     `json:"text"`
	From          string     `json:"from"`
	To            string     `json:"to"`
	Subject       string     `json:"subject"`
	After         *time.Time `json:"after"`
	Before        *time.Time `json:"before"`
	HasKeyword    string     `json:"hasKeyword"`
	HasAttachment *bool      `json:"hasAttachment"`
}

type emailQueryArgs struct {
	AccountID string         `json:"accountId"`
	Filter    emailFilterArg `json:"filter"`
	Sort      []sortArg      `json:"sort"`
	pageArgs
}

func (h *Handler) emailQuery(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args emailQueryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}

	q := EmailQuery{
		Filter: EmailFilter{
			InMailbox:     args.Filter.InMailbox,
			Text:          args.Filter.Text,
			From:          args.Filter.From,
			To:            args.Filter.To,
			Subject:       args.Filter.Subject,
			After:         args.Filter.After,
			Before:        args.Filter.Before,
			HasKeyword:    args.Filter.HasKeyword,
			HasAttachment: args.Filter.HasAttachment,
		},
		Position:        args.Position,
		Limit:           capQueryLimit(args.Limit),
		Anchor:          args.Anchor,
		AnchorOffset:    args.AnchorOffset,
		CollapseThreads: args.CollapseThreads,
	}
	// RFC 8620 §4.4: the sort list applies in order — the store renders
	// every comparator, deterministically tied off by id (FR-X.3).
	for _, s := range args.Sort {
		if !supportedEmailSort(s.Property) {
			return nil, methodErrorf("invalidArguments", "unsupported sort property %q for Email", s.Property)
		}
		q.Sort = append(q.Sort, EmailSort{Property: s.Property, Ascending: s.IsAscending && !s.IsDescending})
	}

	ids, position, total, counter, err := acct.Store.QueryEmails(ctx, acct.ID, q)
	if errors.Is(err, ErrAnchorNotFound) {
		return nil, methodErrorf("anchorNotFound", "anchor %q is not in the query results", args.Anchor)
	}
	if err != nil {
		return nil, serverFail(err)
	}
	resp := map[string]any{
		"accountId":           acct.ID,
		"queryState":          queryState(counter, raw),
		"canCalculateChanges": false,
		"position":            position,
		"ids":                 ids,
	}
	if args.CalculateTotal {
		resp["total"] = total
	}
	return resp, nil
}

// maxQueryResults bounds one /query's returned window. RFC 8620 §4.4
// lets the server cap results; without this a single authenticated
// request could materialise an entire 100k mailbox in memory (NFR-5).
const maxQueryResults = 1000

// capQueryLimit turns a client limit into a bounded server limit: 0
// (unlimited) and oversized values both become maxQueryResults.
func capQueryLimit(limit int) int {
	if limit <= 0 || limit > maxQueryResults {
		return maxQueryResults
	}
	return limit
}

func supportedEmailSort(prop string) bool {
	switch prop {
	case "receivedAt", "subject", "from", "size", "hasAttachment":
		return true
	default:
		return false
	}
}

// --- Email/get (FR-M.4) ---

func (h *Handler) emailGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
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
	wantBodies := args.FetchAllBodyValues || args.FetchTextBodyValues || args.FetchHTMLBodyValues
	emails, state, notFound, err := acct.Store.EmailsByID(ctx, acct.ID, ids, wantBodies)
	if err != nil {
		return nil, serverFail(err)
	}
	list := make([]map[string]any, 0, len(emails))
	for _, e := range emails {
		list = append(list, filterProps(emailObject(e, &args), args.Properties))
	}
	resp := map[string]any{"accountId": acct.ID, "state": state, "list": list, "notFound": notFoundList(notFound)}
	return resp, nil
}

// emailObject builds the full wire object; callers filter with
// filterProps afterwards. Two shapes exist: the store path (Structure
// set — real parsed MIME) and the M0 fixture path (TextBody/HTMLBody
// strings), which synthesises its parts exactly as jmap-tui's mockjmap
// does (PLAN §2.1).
func emailObject(e *Email, args *getArgs) map[string]any {
	obj := map[string]any{
		"id":            e.ID,
		"threadId":      e.ThreadID,
		"mailboxIds":    idSet(e.MailboxIDs),
		"subject":       e.Subject,
		"receivedAt":    e.ReceivedAt.UTC().Format(time.RFC3339),
		"size":          e.Size,
		"hasAttachment": e.HasAttachment,
		"preview":       e.Preview,
	}
	// blobId travels once the bridge holds the raw bytes: it is the id
	// of a copy we cached (a draft we built, a message we fetched to
	// send), never one we would have to download to answer a read
	// (D-2, RFC 8621 §4.1.1).
	if e.BlobID != "" {
		obj["blobId"] = e.BlobID
	}
	if len(e.Keywords) > 0 {
		obj["keywords"] = map[string]bool(e.Keywords)
	}
	if len(e.From) > 0 {
		obj["from"] = addressList(e.From)
	}
	if len(e.To) > 0 {
		obj["to"] = addressList(e.To)
	}
	if len(e.Cc) > 0 {
		obj["cc"] = addressList(e.Cc)
	}
	if len(e.Bcc) > 0 {
		obj["bcc"] = addressList(e.Bcc)
	}
	if len(e.ReplyTo) > 0 {
		obj["replyTo"] = addressList(e.ReplyTo)
	}
	if len(e.MessageID) > 0 {
		obj["messageId"] = e.MessageID
	}
	if len(e.References) > 0 {
		obj["references"] = e.References
	}
	if len(e.InReplyTo) > 0 {
		obj["inReplyTo"] = e.InReplyTo
	}

	if e.Structure != nil {
		structureInto(obj, e, args)
	} else {
		fixtureBodies(obj, e, args)
	}
	return obj
}

// structureInto emits bodyStructure, textBody/htmlBody, attachments and
// bodyValues from the parsed MIME tree (FR-M.4).
func structureInto(obj map[string]any, e *Email, args *getArgs) {
	var tree map[string]any
	if err := json.Unmarshal(e.Structure, &tree); err != nil {
		return
	}
	byPart := map[string]map[string]any{}
	collectParts(tree, byPart)

	obj["bodyStructure"] = filterPartTree(tree, args.BodyProperties)

	if len(e.TextParts) > 0 {
		obj["textBody"] = partsFor(byPart, e.TextParts, args.BodyProperties)
	}
	if len(e.HTMLParts) > 0 {
		obj["htmlBody"] = partsFor(byPart, e.HTMLParts, args.BodyProperties)
	}
	if len(e.Attachments) > 0 {
		ids := make([]string, 0, len(e.Attachments))
		for _, a := range e.Attachments {
			ids = append(ids, a.PartID)
		}
		obj["attachments"] = partsFor(byPart, ids, args.BodyProperties)
	}

	wantValues := args.Properties == nil || containsString(*args.Properties, "bodyValues")
	fetch := args.FetchAllBodyValues || args.FetchTextBodyValues || args.FetchHTMLBodyValues
	if !wantValues || !fetch || len(e.BodyValues) == 0 {
		return
	}
	values := map[string]map[string]any{}
	add := func(ids []string) {
		for _, id := range ids {
			if v, ok := e.BodyValues[id]; ok {
				val, truncated := truncateBodyValue(v, args.MaxBodyValueBytes)
				values[id] = map[string]any{
					"value":             val,
					"isEncodingProblem": false,
					"isTruncated":       truncated,
				}
			}
		}
	}
	switch {
	case args.FetchAllBodyValues:
		add(e.TextParts)
		add(e.HTMLParts)
	case args.FetchTextBodyValues:
		add(e.TextParts)
	case args.FetchHTMLBodyValues:
		add(e.HTMLParts)
	}
	if len(values) > 0 {
		obj["bodyValues"] = values
	}
}

// fixtureBodies is the M0 path: synthesised "1"/"2" parts from plain
// string bodies, byte-compatible with mockjmap's answers.
func fixtureBodies(obj map[string]any, e *Email, args *getArgs) {
	var textPart, htmlPart map[string]any
	if e.TextBody != "" {
		textPart = map[string]any{"partId": "1", "type": "text/plain", "charset": "utf-8", "size": len(e.TextBody)}
		obj["textBody"] = []map[string]any{filterPart(textPart, args.BodyProperties)}
	}
	if e.HTMLBody != "" {
		htmlPart = map[string]any{"partId": "2", "type": "text/html", "charset": "utf-8", "size": len(e.HTMLBody)}
		obj["htmlBody"] = []map[string]any{filterPart(htmlPart, args.BodyProperties)}
	}

	// bodyValues rides only when the client both lists the property and
	// asks for a fetch mode (RFC 8621 §4.1).
	wantValues := args.Properties == nil || containsString(*args.Properties, "bodyValues")
	fetch := args.FetchAllBodyValues || args.FetchTextBodyValues || args.FetchHTMLBodyValues
	if wantValues && fetch {
		values := map[string]map[string]any{}
		if textPart != nil && (args.FetchAllBodyValues || args.FetchTextBodyValues) {
			val, truncated := truncateBodyValue(e.TextBody, args.MaxBodyValueBytes)
			values["1"] = map[string]any{
				"value": val, "isEncodingProblem": false, "isTruncated": truncated,
			}
		}
		if htmlPart != nil && (args.FetchAllBodyValues || args.FetchHTMLBodyValues) {
			val, truncated := truncateBodyValue(e.HTMLBody, args.MaxBodyValueBytes)
			values["2"] = map[string]any{
				"value": val, "isEncodingProblem": false, "isTruncated": truncated,
			}
		}
		if len(values) > 0 {
			obj["bodyValues"] = values
		}
	}
}

// truncateBodyValue caps a body value at maxBytes octets (when positive)
// without splitting a UTF-8 code point, reporting whether it cut
// anything (RFC 8621 §4.1.4).
func truncateBodyValue(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut], true
}

// notFoundList normalises a notFound result to a JSON array: the /get
// response object REQUIRES the property (RFC 8620 §5.1), and an empty
// list must serialise as `[]`, never be omitted.
func notFoundList(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// collectParts indexes every part of the tree by partId.
func collectParts(node map[string]any, byPart map[string]map[string]any) {
	if pid, ok := node["partId"].(string); ok && pid != "" {
		byPart[pid] = node
	}
	if subs, ok := node["subParts"].([]any); ok {
		for _, s := range subs {
			if child, ok := s.(map[string]any); ok {
				collectParts(child, byPart)
			}
		}
	}
}

// partsFor renders the requested part objects for a selection list,
// applying bodyProperties (RFC 8621 §4.1).
func partsFor(byPart map[string]map[string]any, ids []string, props *[]string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if p, ok := byPart[id]; ok {
			out = append(out, filterPart(p, props))
		}
	}
	return out
}

// filterPartTree applies bodyProperties recursively to the structure
// tree; partId always travels so bodyValues stays addressable.
func filterPartTree(node map[string]any, props *[]string) map[string]any {
	if props == nil {
		return node
	}
	out := make(map[string]any, len(*props)+1)
	for _, p := range *props {
		if v, ok := node[p]; ok {
			if p == "subParts" {
				if subs, ok := v.([]any); ok {
					filtered := make([]any, 0, len(subs))
					for _, s := range subs {
						if child, ok := s.(map[string]any); ok {
							filtered = append(filtered, filterPartTree(child, props))
						}
					}
					out["subParts"] = filtered
				}
				continue
			}
			out[p] = v
		}
	}
	if _, ok := node["partId"]; ok {
		out["partId"] = node["partId"]
	}
	return out
}

// --- Thread/get (FR-M.7) ---

func (h *Handler) threadGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	if len(args.IDs) > maxObjectsInGet {
		return nil, methodErrorf("invalidArguments", "ids exceeds maxObjectsInGet (%d)", maxObjectsInGet)
	}
	threads, state, notFound, err := acct.Store.ThreadsByID(ctx, acct.ID, args.IDs)
	if err != nil {
		return nil, serverFail(err)
	}
	list := make([]map[string]any, 0, len(threads))
	for _, t := range threads {
		list = append(list, map[string]any{"id": t.ID, "emailIds": t.EmailIDs})
	}
	resp := map[string]any{"accountId": acct.ID, "state": state, "list": list, "notFound": notFoundList(notFound)}
	return resp, nil
}

// --- Mailbox/changes and Email/changes (FR-M.3, FR-M.6) ---

func changesMethod(kind string) methodFunc {
	return func(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
		var args struct {
			AccountID  string `json:"accountId"`
			SinceState string `json:"sinceState"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, methodErrorf("invalidArguments", "%s", err)
		}
		if merr := checkAccount(acct, args.AccountID); merr != nil {
			return nil, merr
		}
		cs, err := acct.Store.Changes(ctx, acct.ID, kind, args.SinceState)
		if errors.Is(err, ErrCannotCalculateChanges) {
			return nil, &methodErr{
				Type:        "cannotCalculateChanges",
				Description: "state " + args.SinceState + " is not known; refetch with /get",
			}
		}
		if err != nil {
			return nil, serverFail(err)
		}
		created := cs.Created
		if created == nil {
			created = []string{}
		}
		updated := cs.Updated
		if updated == nil {
			updated = []string{}
		}
		destroyed := cs.Destroyed
		if destroyed == nil {
			destroyed = []string{}
		}
		return map[string]any{
			"accountId":      acct.ID,
			"oldState":       args.SinceState,
			"newState":       cs.NewState,
			"hasMoreChanges": cs.HasMore,
			"created":        created,
			"updated":        updated,
			"destroyed":      destroyed,
		}, nil
	}
}

func serverFail(err error) *methodErr {
	return &methodErr{Type: "serverFail", Description: err.Error()}
}

// --- shared helpers ---

// filterProps keeps only the requested top-level properties; nil means
// "all properties the object carries". The id property always travels
// even when the client did not ask for it (RFC 8620 §5.1) — a client
// that keys its cache by the ids it just fetched cannot function without
// it, and jmap-tui's list windows are built from exactly this shape.
//
// Property names the object does not carry are ignored rather than
// rejected: the RFC asks for invalidArguments there, but the bridge
// models a subset of each type's properties (a part of Email that has
// no value yet — blobId until the raw copy is cached —, or a property
// of a type it does not model at all) and refusing a request for one
// would fail a read that could have been answered.
func filterProps(obj map[string]any, props *[]string) map[string]any {
	if props == nil {
		return obj
	}
	out := make(map[string]any, len(*props)+1)
	if v, ok := obj["id"]; ok {
		out["id"] = v
	}
	for _, p := range *props {
		if v, ok := obj[p]; ok {
			out[p] = v
		}
	}
	return out
}

// filterPart narrows a body part to bodyProperties when given.
func filterPart(part map[string]any, props *[]string) map[string]any {
	if props == nil {
		return part
	}
	out := make(map[string]any, len(*props))
	for _, p := range *props {
		if v, ok := part[p]; ok {
			out[p] = v
		}
	}
	// partId always travels so clients can address bodyValues.
	if _, ok := out["partId"]; !ok {
		out["partId"] = part["partId"]
	}
	return out
}

func idSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func addressList(addrs []Address) []map[string]string {
	out := make([]map[string]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, map[string]string{"name": a.Name, "email": a.Email})
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

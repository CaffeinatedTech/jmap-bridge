package jmapapi

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// --- Email/import (RFC 8621 §4.8, FR-M.11) ---

// importArgs is the Email/import argument set. emails is an object keyed
// by client creation id: at least one import is required, and a missing
// or empty map is invalidArguments.
type importArgs struct {
	AccountID string                     `json:"accountId"`
	IfInState *string                    `json:"ifInState"`
	Emails    map[string]json.RawMessage `json:"emails"`
}

// emailImportObj is one Email/import value. The map values keep the raw
// JSON so a member that is not the Boolean true can be rejected rather
// than silently defaulted (RFC 8621 §4.8: these maps hold only true).
type emailImportObj struct {
	BlobID     string                     `json:"blobId"`
	MailboxIDs map[string]json.RawMessage `json:"mailboxIds"`
	Keywords   map[string]json.RawMessage `json:"keywords"`
	ReceivedAt *time.Time                 `json:"receivedAt"`
}

// importResponse is the Email/import answer (RFC 8621 §4.8): a /set-style
// response carrying only created/notCreated. Nil collections marshal as
// JSON null, the same convention SetResponse uses (FR-J.4).
type importResponse struct {
	AccountID  string              `json:"accountId"`
	OldState   *string             `json:"oldState"`
	NewState   string              `json:"newState"`
	Created    map[string]any      `json:"created"`
	NotCreated map[string]SetError `json:"notCreated"`
}

func (h *Handler) emailImport(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args importArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	if len(args.Emails) == 0 {
		return nil, methodErrorf("invalidArguments", "emails is required and must not be empty")
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

	// Validate every member before the first APPEND: a malformed import
	// must not leave an earlier one on the server while the method
	// answers invalidArguments (golden rule 1).
	specs := make(map[string]ImportSpec, len(args.Emails))
	for handle, obj := range args.Emails {
		spec, merr := parseImport(obj)
		if merr != nil {
			return nil, merr
		}
		specs[handle] = spec
	}

	resp := &importResponse{AccountID: acct.ID, OldState: &oldState, NewState: oldState}
	for handle, spec := range specs {
		created, err := acct.Backend.ImportEmail(ctx, acct.ID, spec)
		if err != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = importSetErr(err)
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[handle] = created
	}

	// Read-your-writes: the state this response ends with must include
	// the imports it just reported (FR-M.13).
	states, err = acct.Store.States(ctx, acct.ID)
	if err != nil {
		return nil, serverFail(err)
	}
	resp.NewState = states["Email"]
	return resp, nil
}

// parseImport validates one Email/import value (RFC 8621 §4.8).
func parseImport(raw json.RawMessage) (ImportSpec, *methodErr) {
	var obj emailImportObj
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ImportSpec{}, methodErrorf("invalidArguments", "%s", err)
	}
	if obj.BlobID == "" {
		return ImportSpec{}, methodErrorf("invalidArguments", "emails[].blobId is required")
	}
	mailboxSet, merr := importTrueProperties("mailboxIds", obj.MailboxIDs, true)
	if merr != nil {
		return ImportSpec{}, merr
	}
	keywords, merr := importTrueProperties("keywords", obj.Keywords, false)
	if merr != nil {
		return ImportSpec{}, merr
	}
	// The append target is whichever mailbox comes first; sort so the
	// choice is deterministic across the map's random iteration order.
	mailboxIDs := make([]string, 0, len(mailboxSet))
	for id := range mailboxSet {
		mailboxIDs = append(mailboxIDs, id)
	}
	sort.Strings(mailboxIDs)

	received := time.Now()
	if obj.ReceivedAt != nil {
		received = *obj.ReceivedAt
	}
	return ImportSpec{
		BlobID:     obj.BlobID,
		MailboxIDs: mailboxIDs,
		Keywords:   keywords,
		ReceivedAt: received,
	}, nil
}

// importTrueProperties reads an RFC 8621 §4.8 true-valued object
// (mailboxIds, keywords). Every member must be the Boolean true; a
// non-true value, or a required-but-absent or empty object, is
// invalidArguments rather than a silently reshaped import.
func importTrueProperties(key string, obj map[string]json.RawMessage, required bool) (map[string]bool, *methodErr) {
	if obj == nil {
		if required {
			return nil, methodErrorf("invalidArguments", "%s is required", key)
		}
		return nil, nil
	}
	if required && len(obj) == 0 {
		return nil, methodErrorf("invalidArguments", "%s must not be empty", key)
	}
	out := make(map[string]bool, len(obj))
	for member, val := range obj {
		var b bool
		if err := json.Unmarshal(val, &b); err != nil || !b {
			return nil, methodErrorf("invalidArguments", "%s/%s must be true", key, member)
		}
		out[member] = true
	}
	return out, nil
}

// importSetErr maps a backend import failure onto the SetError RFC 8621
// §4.8 puts in notCreated. A blob or mailbox the account does not hold is
// the import's own property problem; anything else is serverFail
// carrying the backend's words (FR-M.13).
func importSetErr(err error) SetError {
	switch {
	case errors.Is(err, ErrBlobNotFound):
		return NewSetError("invalidProperties", []string{"blobId"}, err.Error())
	case errors.Is(err, ErrUnknownMailbox):
		return NewSetError("invalidProperties", []string{"mailboxIds"}, err.Error())
	default:
		return NewSetError("serverFail", nil, err.Error())
	}
}

package jmapapi

// SetError is the per-record error object of a /set response (RFC 8620
// §5.3): a registry error code in Type, optional offending Properties
// for invalidProperties, and a developer-facing Description.
type SetError struct {
	Type        string   `json:"type"`
	Properties  []string `json:"properties,omitempty"`
	Description *string  `json:"description,omitempty"`
	// NotFound carries the ids an error refers to — RFC 8621 §4.6
	// requires it on blobNotFound.
	NotFound []string `json:"notFound,omitempty"`
	// InvalidRecipients lists the addresses RFC 8621 §7.5 requires on
	// an invalidRecipients SetError.
	InvalidRecipients []string `json:"invalidRecipients,omitempty"`
}

// NewSetError builds a SetError. properties may be nil; description may
// be empty (omitted from the wire).
func NewSetError(typ string, properties []string, description string) SetError {
	e := SetError{Type: typ, Properties: properties}
	if description != "" {
		e.Description = &description
	}
	return e
}

// SetResponse is the RFC 8620 §5.3 /set response shape (FR-J.4): absent
// collections are JSON null — never [] or {} — `created` is an
// Id[Foo] map, `updated` an Id[Foo|null] map and `destroyed` an Id[]
// list, which is what the RFC defines and what Fastmail and Stalwart
// were measured answering (2026-09-29). It ships with M0 because the
// shape is a protocol contract of its own; M2's Email/set, M2's
// Mailbox/set and M3's implicit submission Email/set fill it in.
type SetResponse struct {
	AccountID string `json:"accountId"`
	// OldState is null unless the caller knows the pre-mutation state
	// (RFC 8620 §5.3: String|null).
	OldState *string `json:"oldState"`
	NewState string  `json:"newState"`

	Created      map[string]any           `json:"created"`
	Updated      map[string]*UpdateDetail `json:"updated"`
	Destroyed    []string                 `json:"destroyed"`
	NotCreated   map[string]SetError      `json:"notCreated"`
	NotUpdated   map[string]SetError      `json:"notUpdated"`
	NotDestroyed map[string]SetError      `json:"notDestroyed"`
}

// UpdateDetail is the value side of a /set response's `updated` map
// (RFC 8620 §5.3 Id[Foo|null]): an object of changes the client did not
// ask for, or null when there are none. The bridge always has none — it
// applies exactly the patch it was given — so every entry is null, the
// same answer Fastmail and Stalwart give.
type UpdateDetail struct{}

// markUpdated records one id as changed with no unrequested changes. A
// nil Updated marshals as JSON null, which is RFC 8620 §5.3's "nothing
// was updated" (FR-J.4: null, never [] or {}).
func (r *SetResponse) markUpdated(id string) {
	if r.Updated == nil {
		r.Updated = map[string]*UpdateDetail{}
	}
	r.Updated[id] = nil
}

// set converts a methodErr into the SetError a /set member failure
// carries (RFC 8620 §5.3). The two shapes share a type on purpose: the
// same code, description and properties travel through both.
func (e *methodErr) set() SetError {
	return SetError{
		Type:              e.Type,
		Properties:        e.Properties,
		Description:       optStr(e.Description),
		InvalidRecipients: e.InvalidRecipients,
	}
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// NewSetResponse starts an empty set response for an account: every
// collection is null, newState carries the post-mutation type state.
func NewSetResponse(accountID, newState string) *SetResponse {
	return &SetResponse{AccountID: accountID, NewState: newState}
}

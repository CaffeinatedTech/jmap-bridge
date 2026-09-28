package jmapapi

// SetError is the per-record error object of a /set response (RFC 8620
// §5.3): a registry error code in Type, optional offending Properties
// for invalidProperties, and a developer-facing Description.
type SetError struct {
	Type        string   `json:"type"`
	Properties  []string `json:"properties,omitempty"`
	Description *string  `json:"description,omitempty"`
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
// collections are JSON null — never [] or {} — and Updated is an array
// of ids, the form Fastmail, Stalwart and jmap-tui's flexible decoder
// accept (PLAN §2.1). It ships with M0 because the shape is a protocol
// contract of its own; M2's Email/set and M2's Mailbox/set fill it in.
type SetResponse struct {
	AccountID string `json:"accountId"`
	// OldState is null unless the caller knows the pre-mutation state
	// (RFC 8620 §5.3: String|null).
	OldState *string `json:"oldState"`
	NewState string  `json:"newState"`

	Created      map[string]any      `json:"created"`
	Updated      []string            `json:"updated"`
	Destroyed    []string            `json:"destroyed"`
	NotCreated   map[string]SetError `json:"notCreated"`
	NotUpdated   map[string]SetError `json:"notUpdated"`
	NotDestroyed map[string]SetError `json:"notDestroyed"`
}

// NewSetResponse starts an empty set response for an account: every
// collection is null, newState carries the post-mutation type state.
func NewSetResponse(accountID, newState string) *SetResponse {
	return &SetResponse{AccountID: accountID, NewState: newState}
}

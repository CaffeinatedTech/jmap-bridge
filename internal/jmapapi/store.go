// Package jmapapi implements the JMAP method surface: batch dispatch,
// result references (RFC 8620 §3.7), state strings, and the read methods
// backed by a Store. The Store seam (PLAN §3) is what M1 swaps for the
// SQLite-backed internal/store without touching dispatch.
package jmapapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrCannotCalculateChanges makes a Store decline /changes replay when it
// cannot produce a change set for the requested sinceState (RFC 8620
// §5.2); dispatch turns it into a cannotCalculateChanges method error.
var ErrCannotCalculateChanges = errors.New("cannot calculate changes")

// Mailbox is the store-level view of a JMAP Mailbox (FR-M.1).
type Mailbox struct {
	ID        string
	ParentID  string // "" for a top-level mailbox
	Name      string
	Role      string // "" when the mailbox has no SPECIAL-USE role
	SortOrder int

	TotalEmails   int
	UnreadEmails  int
	TotalThreads  int
	UnreadThreads int

	MayRead        bool
	MayAddItems    bool
	MayRemoveItems bool
	MayCreateChild bool
	MayRename      bool
	MayDelete      bool
}

// Address is an RFC 5322 mailbox as JMAP models it. The tags are the
// JMAP wire spelling, so the store's canonical headers JSON and the API
// response agree (convert writes them, emailObject echoes them).
type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Email is the store-level view of a JMAP Email (FR-M.4). Summary
// fields are always populated; body data arrives through Structure /
// BodyValues once the store has parsed it.
type Email struct {
	ID       string
	ThreadID string

	// BlobID is the id of the raw RFC 5322 bytes when the bridge holds
	// them (RFC 8621 §4.1.1); "" while they have not been cached yet —
	// bodies are lazy (D-2), and the raw copy arrives with them.
	BlobID string

	MailboxIDs []string
	Keywords   map[string]bool

	MessageID  []string
	References []string
	InReplyTo  []string

	From, To, Cc, Bcc, ReplyTo []Address

	Subject       string
	ReceivedAt    time.Time
	Size          int64
	HasAttachment bool
	Preview       string

	TextBody string // fixture path: plain text body, "" when absent
	HTMLBody string // fixture path: html body, "" when absent

	// Structure is the JMAP EmailBodyPart tree as raw JSON (FR-M.4
	// bodyStructure); nil for the M0 fixture path, which synthesises
	// its parts from TextBody/HTMLBody instead.
	Structure json.RawMessage
	// BodyValues maps partId → decoded body text, populated once the
	// body has been hydrated (FR-S.8).
	BodyValues map[string]string
	// TextParts / HTMLParts are the textBody/htmlBody selections
	// derived from Structure (FR-M.4).
	TextParts, HTMLParts []string
	// Attachments are the attachment entries of Structure, with
	// BlobID set once the part has been written to the blob store.
	Attachments []Attachment
}

// Attachment is one attachment body part (RFC 8621 §4.1.3).
type Attachment struct {
	PartID      string `json:"partId"`
	BlobID      string `json:"blobId,omitempty"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Name        string `json:"name,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

// Thread is a JMAP Thread: ids of its emails, oldest first (RFC 8621
// §3.1).
type Thread struct {
	ID       string
	EmailIDs []string
}

// EmailFilter is the parsed subset of Email/query FilterCondition the
// bridge honours in v0.1 (FR-M.5).
type EmailFilter struct {
	InMailbox     string
	Text          string
	From          string
	To            string
	Subject       string
	After         *time.Time
	Before        *time.Time
	HasKeyword    string
	HasAttachment *bool
}

// EmailSort is one sort comparator (RFC 8621 §4.4.1).
type EmailSort struct {
	Property  string
	Ascending bool
}

// EmailQuery is a fully parsed Email/query argument set handed to the
// Store; dispatch owns the wire parsing, the Store owns evaluation.
type EmailQuery struct {
	Filter          EmailFilter
	Sort            []EmailSort
	Position        int
	Limit           int // 0 = no limit
	Anchor          string
	AnchorOffset    int
	CollapseThreads bool
}

// ChangeSet is a replayed /changes result (RFC 8620 §5.2). Created,
// Updated and Destroyed are id lists; NewState is the state after the
// changes.
type ChangeSet struct {
	Created   []string
	Updated   []string
	Destroyed []string
	NewState  string
	HasMore   bool
}

// Store is the data seam: M0's internal/fixture implements it with
// in-memory sample mail, M1's internal/store implements it over SQLite
// (PLAN §3, §4). Every method is account-scoped — the bridge never serves
// one account's data for another (FR-A.11).
type Store interface {
	// Mailboxes returns every mailbox, plus the Mailbox type state.
	Mailboxes(ctx context.Context, account string) ([]*Mailbox, string, error)

	// MailboxesByID returns the named mailboxes; ids that do not exist
	// for this account land in notFound. A nil ids slice means "all".
	MailboxesByID(ctx context.Context, account string, ids []string) ([]*Mailbox, string, []string, error)

	// QueryEmails evaluates q and returns the window of matching ids,
	// the position the window starts at (anchor-adjusted), the total
	// match count (regardless of q.Limit), and the query counter that
	// dispatch folds into queryState (PLAN §4.1).
	QueryEmails(ctx context.Context, account string, q EmailQuery) (ids []string, position, total int, counter string, err error)

	// EmailsByID returns the named emails; unknown ids land in notFound.
	// A nil ids slice means "all". wantBodies asks the store to make
	// bodyValues available first (hydration on demand, FR-M.4/FR-S.8);
	// with it false the store never touches bodies (golden rule 2).
	EmailsByID(ctx context.Context, account string, ids []string, wantBodies bool) ([]*Email, string, []string, error)

	// ThreadsByID returns the named threads; unknown ids land in
	// notFound.
	ThreadsByID(ctx context.Context, account string, ids []string) ([]*Thread, string, []string, error)

	// States returns the current state string for each pushable type
	// (Mailbox, Email, Thread; contacts join in M6) — the payload of a
	// StateChange push (FR-J.8).
	States(ctx context.Context, account string) (map[string]string, error)

	// Changes replays changes of kind ("Mailbox" or "Email") since
	// sinceState, or returns ErrCannotCalculateChanges when it cannot.
	Changes(ctx context.Context, account, kind, sinceState string) (ChangeSet, error)

	// PutBlob stores an immutable blob for the account and returns its
	// id (FR-M.16, RFC 8620 §6.1). The account it is filed under is the
	// one that may read it back (FR-M.17, FR-A.11).
	PutBlob(ctx context.Context, account, mediaType string, data []byte) (string, error)

	// ReadBlob returns a blob's bytes and media type. An id that does
	// not exist in this account fails with ErrBlobNotFound, which the
	// download endpoint answers as a 404 (FR-M.17).
	ReadBlob(ctx context.Context, account, id string) ([]byte, string, error)
}

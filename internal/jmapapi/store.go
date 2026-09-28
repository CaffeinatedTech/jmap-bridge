// Package jmapapi implements the JMAP method surface: batch dispatch,
// result references (RFC 8620 §3.7), state strings, and the read methods
// backed by a Store. The Store seam (PLAN §3) is what M1 swaps for the
// SQLite-backed internal/store without touching dispatch.
package jmapapi

import (
	"context"
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

// Address is an RFC 5322 mailbox as JMAP models it.
type Address struct {
	Name  string
	Email string
}

// Email is the store-level view of a JMAP Email (FR-M.4). Bodies are
// plain text strings until M1's blob store materialises MIME structure.
type Email struct {
	ID       string
	ThreadID string

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

	TextBody string // "" when the message has no text/plain part
	HTMLBody string // "" when the message has no text/html part
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
	// A nil ids slice means "all".
	EmailsByID(ctx context.Context, account string, ids []string) ([]*Email, string, []string, error)

	// ThreadsByID returns the named threads; unknown ids land in
	// notFound.
	ThreadsByID(ctx context.Context, account string, ids []string) ([]*Thread, string, []string, error)

	// Changes replays changes of kind ("Mailbox" or "Email") since
	// sinceState, or returns ErrCannotCalculateChanges when it cannot.
	Changes(ctx context.Context, account, kind, sinceState string) (ChangeSet, error)
}

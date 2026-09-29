package jmapapi

import (
	"context"
	"errors"
	"time"
)

// The Backend is the mutation seam (PLAN §7.1, D-14): dispatch owns
// JMAP semantics — patch shapes, SetError mapping, state strings — and
// the backend owns everything that touches the server and the cache
// behind it: resolve ids to folder copies, run the IMAP command, and
// commit locally only after the server said OK. One method handles one
// object, because RFC 8620 §5.3 makes each create/update/destroy
// member all-or-nothing (FR-M.13).
//
// The implementation is the per-account sync engine (internal/sync):
// it already owns the account's connections, its store and its
// discovery pass, and the package doc has always said M2 writes land
// there.
type Backend interface {
	// ApplyEmailPatch runs one Email/set update (FR-M.9): keyword and
	// membership deltas, server-first, local commit second.
	ApplyEmailPatch(ctx context.Context, account, emailID string, p EmailPatch) error

	// DestroyEmails expunges every copy of one email and tombstones it
	// (FR-M.10). Destroying an id the cache already considers gone is a
	// no-op success.
	DestroyEmails(ctx context.Context, account, emailID string) error

	// CreateDraft builds an RFC 5322 message from spec, APPENDs it, and
	// records it (FR-M.11).
	CreateDraft(ctx context.Context, account string, spec DraftSpec) (*CreatedEmail, error)

	// CreateMailbox runs CREATE, refreshes role detection and returns
	// the new mailbox id (FR-M.12).
	CreateMailbox(ctx context.Context, account, name, parentID string) (string, error)

	// RenameMailbox runs RENAME (or a reparent) keeping the id stable.
	RenameMailbox(ctx context.Context, account, id, name, parentID string) error

	// DestroyMailbox runs DELETE. removeEmails reports the caller's
	// onDestroyRemoveEmails argument (RFC 8621 §2.5).
	DestroyMailbox(ctx context.Context, account, id string, removeEmails bool) error
}

// Mutation errors the Backend reports; dispatch maps them to the
// SetError types RFC 8621 defines for these conditions. Any other
// error becomes serverFail carrying its text (FR-M.13).
var (
	// ErrObjectNotFound is an id the cache has never seen (or has
	// already tombstoned): notFound.
	ErrObjectNotFound = errors.New("jmapapi: object not found")
	// ErrMailboxExists: a mailbox with the same path already exists →
	// alreadyExists (JMAP error code registry, RFC 8620 §5.4).
	ErrMailboxExists = errors.New("jmapapi: mailbox already exists")
	// ErrMailboxHasChild → RFC 8621 §2.5 mailboxHasChild.
	ErrMailboxHasChild = errors.New("jmapapi: mailbox has children")
	// ErrMailboxHasEmail → RFC 8621 §2.5 mailboxHasEmail.
	ErrMailboxHasEmail = errors.New("jmapapi: mailbox has emails")
	// ErrWouldLeaveEmpty: the patch would leave the email in no mailbox,
	// which the mail store may never do (RFC 8621 §4.1).
	ErrWouldLeaveEmpty = errors.New("jmapapi: email would belong to no mailbox")
	// ErrUnknownMailbox: a patch or create names a mailbox id the cache
	// does not hold → invalidProperties.
	ErrUnknownMailbox = errors.New("jmapapi: unknown mailbox id")
	// ErrNoWriteBackend: the account has no IMAP backend (cache-only),
	// so it can serve reads but must never claim a write succeeded.
	ErrNoWriteBackend = errors.New("jmapapi: account has no IMAP backend")
	// ErrNoLocation: the cache cannot yet address the message on the
	// server (its uid mapping arrives with the next sync pass).
	ErrNoLocation = errors.New("jmapapi: no backend location for the message")
	// ErrInvalidMailboxName: a name the server cannot represent (empty,
	// or containing the hierarchy separator).
	ErrInvalidMailboxName = errors.New("jmapapi: invalid mailbox name")
	// ErrBlobNotFound: a create referenced a blobId that does not exist
	// in this account → RFC 8621 §4.6 blobNotFound.
	ErrBlobNotFound = errors.New("jmapapi: blob not found")
)

// KeywordError reports the server refusing a custom keyword (FR-M.8):
// the write fails naming the keyword rather than dropping it silently.
type KeywordError struct {
	Keywords []string
}

func (e *KeywordError) Error() string {
	return "server refused keyword(s): " + joinComma(e.Keywords)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// EmailPatch is one decoded Email/set update (FR-M.9). The whole-set
// form (`mailboxIds: {...}`, `keywords: {...}`) is a replacement: the
// backend subtracts the current members from what the client sent to
// work out the removals (RFC 8620 §5.3: an entire object is a valid
// patch).
type EmailPatch struct {
	KeywordAdd    []string
	KeywordRemove []string
	MailboxAdd    []string
	MailboxRemove []string

	// Replace* mark the whole-property form: the given Add set becomes
	// the entire membership/keyword set.
	ReplaceKeywords  bool
	ReplaceMailboxes bool
}

// Empty reports whether the patch asks for nothing at all.
func (p EmailPatch) Empty() bool {
	return len(p.KeywordAdd) == 0 && len(p.KeywordRemove) == 0 &&
		len(p.MailboxAdd) == 0 && len(p.MailboxRemove) == 0 &&
		!p.ReplaceKeywords && !p.ReplaceMailboxes
}

// DraftPart is one body part of a create: either inline text or a
// reference to an uploaded blob (RFC 8621 §4.6 allows a partId *or* a
// blobId, never both).
type DraftPart struct {
	Type        string // "text/plain", "text/html", or an attachment media type
	Disposition string
	Charset     string
	Name        string
	Text        string // bodyValues content for an inline part
	BlobID      string // set → the backend reads the bytes from the blob store
}

// DraftSpec is a validated Email/set create (FR-M.11). The handler has
// already checked the RFC 8621 §4.6 constraints and resolved the body
// parts; the backend chooses the target mailbox, generates Message-ID
// and Date when the client omitted them, and appends.
type DraftSpec struct {
	// MailboxIDs is the create's mailboxIds; empty means "the account's
	// drafts mailbox" (FR-M.11).
	MailboxIDs []string

	From, To, Cc, Bcc, ReplyTo []Address
	Subject                    string
	Keywords                   map[string]bool
	Parts                      []DraftPart
	InReplyTo, References      []string
	ReceivedAt                 time.Time
}

// CreatedEmail is the created object RFC 8621 §4.6 requires in
// `created`: id, blobId, threadId and size.
type CreatedEmail struct {
	ID       string `json:"id"`
	BlobID   string `json:"blobId,omitempty"`
	ThreadID string `json:"threadId"`
	Size     int64  `json:"size"`
}

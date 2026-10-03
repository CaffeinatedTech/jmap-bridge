// Package mailbackend is the sync engine's only view of a mail provider
// (D-API-2, GMAIL_API_PLAN §3.1). It defines provider-neutral types and
// the Backend interface; internal/imapdrv adapts CaffeinatedTech/go-imap
// to it today, and a future internal/gmailapi adapts the Gmail REST API
// without internal/sync learning either provider's nouns.
//
// The interface is deliberately provider-shaped rather than
// protocol-shaped: a Backend owns its own change strategy (IMAP tiers or
// Gmail history), its own container naming (IMAP paths or label ids) and
// its own membership mechanism (folder copies or labels). The engine
// orchestrates passes and commits what the backend reported, so a
// backend can never lie to the cache about what the server accepted
// (golden rule 1).
package mailbackend

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
)

// Kind names a backend implementation, for metrics and logs.
type Kind string

const (
	KindIMAP     Kind = "imap"
	KindGmailAPI Kind = "gmail_api"
)

// Capabilities is what a provider can do, advertised honestly (golden
// rule 4). The engine consults these instead of sniffing the provider.
type Capabilities struct {
	// Preview means summaries carry server-side preview text (RFC 8970
	// PREVIEW, or a Gmail snippet); otherwise the engine derives them.
	Preview bool
	// Labels means membership is expressed as labels on one message
	// (Gmail) rather than per-folder copies; the write path folds
	// accordingly.
	Labels bool
	// Move means a native MOVE exists; otherwise removals are
	// copy-plus-expunge.
	Move bool
	// UIDPlus means COPYUID/UIDPLUS precision is available.
	UIDPlus bool
	// CustomKeywords means arbitrary keywords are storable (IMAP
	// keywords); otherwise only the provider's known keyword set is.
	CustomKeywords bool
	// Push means a server-push watch exists; otherwise the engine polls.
	Push bool
	// ImplicitArchive means an archive container holds every message
	// implicitly and cannot express removal (Gmail All Mail, D-20).
	ImplicitArchive bool
}

// Folder is one provider container mapped into the JMAP mailbox
// vocabulary. Container is the backend handle: an IMAP folder path or a
// Gmail label id.
type Folder struct {
	Container string
	// Name is the display path (hierarchical, separator included).
	Name string
	// Delim is the hierarchy separator inside Name, 0 when flat.
	Delim rune
	// Role is the JMAP role derived by the backend ("" when unrole'd).
	Role string
	// NativeID is the backend's own container identifier when it differs
	// from the visible path (the Gmail label id); "" when the path is the
	// identifier (IMAP).
	NativeID string
	// NoSelect marks a hierarchy container with no messages.
	NoSelect bool
	// Implicit marks a container whose membership the server owns
	// (Gmail All Mail): clients may not add to or remove from it.
	Implicit bool
}

// FolderStatus is a container's current state, used for discovery
// counts and reset detection (FR-S.6).
type FolderStatus struct {
	// Version is the container's identity token (IMAP UIDVALIDITY); a
	// change means every cached reference is stale.
	Version  uint32
	Next     uint64 // next id the provider will assign (IMAP UIDNEXT)
	ModSeq   uint64 // provider change sequence (IMAP HIGHESTMODSEQ)
	Messages uint32 // message count (EXISTS / messagesTotal)
	Unseen   uint32
}

// Ref identifies one message copy in one container. It is the only
// addressing the engine handles; the backend parses/encodes its own
// native form. IMAP: Container=folder path, Version=UIDVALIDITY,
// Native=UID decimal. Gmail API: Container=label id, Version="",
// Native=message id.
type Ref struct {
	Container string
	Version   string
	Native    string
}

// NewRef builds a Ref from a container and a numeric native id
// (the IMAP shape).
func NewRef(container string, version uint32, uid uint32) Ref {
	return Ref{
		Container: container,
		Version:   strconv.FormatUint(uint64(version), 10),
		Native:    strconv.FormatUint(uint64(uid), 10),
	}
}

// VersionNum parses the numeric Version, 0 when absent/opaque.
func (r Ref) VersionNum() uint32 {
	v, _ := strconv.ParseUint(r.Version, 10, 32)
	return uint32(v)
}

// UID parses the numeric Native id and reports whether it was numeric.
func (r Ref) UID() (uint32, bool) {
	v, err := strconv.ParseUint(r.Native, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// Cursor is one container's opaque sync position. The engine persists
// it verbatim in sync_state; only the backend interprets its fields.
type Cursor struct {
	Version      uint32 // UIDVALIDITY
	Next         uint64 // UIDNEXT
	ModSeq       uint64
	Messages     uint32
	BackfillMark uint32 // progress through the initial walk
	BackfillDone bool
}

// Header is one message's header-level summary: envelope, structure,
// flags, size and date — never a body (golden rule 2). Backends map
// their own representations into this.
type Header struct {
	Ref          Ref
	Flags        []string
	ModSeq       uint64
	InternalDate time.Time
	Size         int64
	Envelope     convert.Envelope
	Structure    *convert.Part
	// ThreadHint seeds thread ids ahead of Message-ID (Gmail thrid).
	ThreadHint uint64
	// ThreadKey, when set, is a fully-formed store thread-registry key
	// ("g:<sha1(native thread id)>") that outranks header-chain
	// derivation. The Gmail API's threadId is an opaque string with no
	// numeric X-GM-THRID equivalent, so the adapter supplies the key.
	ThreadKey string
	// Preview is server-side preview text when the provider includes it
	// in the header fetch (Gmail's snippet); the engine stores it at
	// ingest instead of a second preview round trip.
	Preview string
}

// FlagChange is one message's flag state as the provider reported it.
type FlagChange struct {
	Ref    Ref
	Flags  []string
	ModSeq uint64
}

// Delta is one container's incremental sync result. The engine folds it
// into the store; the backend decides what to include.
type Delta struct {
	// Headers are new messages to ingest.
	Headers []Header
	// Flags are flag changes for known (or, when unknown, new) messages,
	// folded one uid at a time. Ignored when Bulk is set.
	Flags []FlagChange
	// Bulk marks Flags as the container's complete flag state
	// (baseline tier): the engine applies them in one bulk update
	// instead of per-uid.
	Bulk bool
	// Removed are copies the provider no longer holds (expunged). The
	// engine applies its own-write grace window before tombstoning.
	Removed []Ref
	// Reset means the container's identity changed or its anchor was
	// rejected: every cached reference is stale and the engine must
	// reset the container.
	Reset bool
}

// Message is the engine's neutral view of one JMAP Email for writes:
// its id, its RFC 5322 Message-ID (for header search), and the implicit
// archive container when one exists.
type Message struct {
	ID                string
	MessageID         string
	ImplicitContainer string
}

// Mailbox is the engine's neutral view of one JMAP Mailbox for writes.
type Mailbox struct {
	ID       string
	Path     string // backend container handle
	Role     string
	Implicit bool
}

// Copy is one live membership of a message: the JMAP mailbox id and the
// backend ref addressing it there.
type Copy struct {
	MailboxID string
	Ref       Ref
}

// Hooks are the narrow store reads a backend needs to preserve its own
// reconciliation strategy without importing the store. They are function
// values so the dependency points engine → backend, never the reverse.
// Nil hooks disable the dependent optimization.
type Hooks struct {
	// KnownUIDs returns the container's cached refs, the reconcile
	// basis for tiers that cannot name expunges.
	KnownUIDs func() ([]Ref, error)
	// KnownSharedUIDs reports which of uids the account already knows
	// through another container (shared-UID servers), enabling the
	// header-fetch skip. Link records that membership for the skipped
	// uids.
	KnownSharedUIDs func(uids []uint32) (map[uint32]bool, error)
	LinkSharedUIDs  func(uids []uint32) error
}

// Backend is one provider session. It is not safe for concurrent use;
// the engine serialises calls per session (PLAN §10).
type Backend interface {
	Kind() Kind
	Capabilities() Capabilities

	// Connect establishes the session if it is not already up.
	Connect(ctx context.Context) error
	// Close releases the session.
	Close() error
	// Ping reports whether the session is still usable, dialing nothing.
	Ping(ctx context.Context) error
	// Release drops any per-operation session state (IMAP selection);
	// a no-op for stateless backends.
	Release(ctx context.Context) error

	// Folders lists every container (FR-S.1).
	Folders(ctx context.Context) ([]Folder, error)
	// FolderStatus reads one container's state without selecting it.
	FolderStatus(ctx context.Context, container string) (FolderStatus, error)

	// Backfill walks one container's headers from cursor in one batch.
	// It returns the headers fetched and the next cursor; the caller
	// loops until the next cursor's BackfillDone is set.
	Backfill(ctx context.Context, container string, c Cursor, batch int, hooks Hooks) (headers []Header, next Cursor, err error)

	// Incremental applies one container's post-backfill sync and
	// returns the changes to fold into the store.
	Incremental(ctx context.Context, container string, c Cursor, hooks Hooks) (Delta, Cursor, error)

	// FetchHeaders fetches header records for specific refs of one
	// container (the flag-change path where a reported uid turns out to
	// be a new message).
	FetchHeaders(ctx context.Context, container string, refs []Ref) ([]Header, error)

	// Watch begins watching a container for foreign changes and returns
	// a channel that receives a coalesced signal and closes when the
	// watch ends. A nil channel (with nil error) means the backend has
	// no push and the engine polls.
	Watch(ctx context.Context, container string) (<-chan struct{}, error)

	// FetchPreviews returns preview text per ref (server PREVIEW when
	// available, otherwise a bounded partial fetch).
	FetchPreviews(ctx context.Context, refs []Ref) (map[Ref]string, error)
	// FetchRaw downloads one message's full bytes, never setting \Seen.
	FetchRaw(ctx context.Context, ref Ref) ([]byte, error)
	// FetchRawBatch downloads several full messages from one container
	// in one batch, keyed by ref.
	FetchRawBatch(ctx context.Context, container string, refs []Ref) (map[Ref][]byte, error)

	// StoreKeywords adds and removes neutral keywords on every copy.
	StoreKeywords(ctx context.Context, copies []Copy, add, remove []string) error
	// SetMembership changes which containers hold a message, returning
	// the copies it created and every copy it touched (for the grace
	// window). The engine refuses implicit removals before calling.
	SetMembership(ctx context.Context, msg Message, copies []Copy, add, remove []Mailbox) (added []Copy, touched []Ref, err error)
	// Destroy permanently removes every copy, routing through trash
	// when the provider needs it (Gmail). trash is the trash container
	// path, "" when none.
	Destroy(ctx context.Context, copies []Ref, trash string) (touched []Ref, err error)

	// Append stores raw RFC 5322 bytes in a container with the given
	// neutral keywords, returning the copy's ref and the provider flags
	// it stored.
	Append(ctx context.Context, container string, raw []byte, keywords []string, at *time.Time) (Ref, []string, error)

	CreateMailbox(ctx context.Context, path string) error
	RenameMailbox(ctx context.Context, from, to string) error
	DeleteMailbox(ctx context.Context, path string) error
	HierarchyDelim(ctx context.Context) (rune, error)
}

// ErrThrottled is the provider asking the engine to back off. The IMAP
// driver maps OK [THROTTLED] and rate-limit errors here; the Gmail API
// maps 429/403 rateLimitExceeded. It is backend-neutral so the engine's
// cooldown logic keys off one error.
var ErrThrottled = errors.New("mailbackend: provider throttled")

// ErrAuth is an authentication refusal (bad credentials, dead refresh
// token): the account needs a human (FR-D.4).
var ErrAuth = errors.New("mailbackend: authentication failed")

// RejectedError is a provider answering NO/BAD to a command it
// understood, as opposed to a transport failure. The engine keys
// "serverFail vs retryable" and custom-keyword refusals off it.
type RejectedError struct {
	Text string
	Err  error
}

func (e *RejectedError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return "mailbackend: command rejected"
}

func (e *RejectedError) Unwrap() error { return e.Err }

// IsRejected reports whether err is a provider refusal.
func IsRejected(err error) bool {
	var r *RejectedError
	return errors.As(err, &r)
}

// UnsupportedKeywordsError reports that a provider cannot store the named
// JMAP keywords (Gmail API mode has no $answered, no $deleted and no
// arbitrary keywords — D-API-5). It is distinct from a generic refusal so
// the engine can turn it into the RFC 8621 keyword SetError that names
// each offending keyword (FR-M.8).
type UnsupportedKeywordsError struct {
	Keywords []string
}

func (e *UnsupportedKeywordsError) Error() string {
	return "mailbackend: unsupported keyword(s): " + joinComma(e.Keywords)
}

func joinComma(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// RejectText returns the provider's refusal text, "" when not a refusal.
func RejectText(err error) string {
	var r *RejectedError
	if errors.As(err, &r) {
		return r.Text
	}
	return ""
}

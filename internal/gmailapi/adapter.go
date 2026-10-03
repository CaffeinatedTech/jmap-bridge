package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// apiUIDValidity is the synthetic container version every Gmail API
// container reports. The API has no UIDVALIDITY analogue (D-API-9), and
// a constant keeps the store's reset detection quiet; a history expiry is
// handled by a full resync inside the adapter instead.
const apiUIDValidity uint32 = 1

// allMailName is the synthetic archive mailbox (D-API-11): the API does
// not expose All Mail as a label, but every message belongs to it.
const allMailName = "All Mail"

// metadataHeaderNames is the header subset the summary mapper consumes.
// It matches what the IMAP path reads from ENVELOPE.
var metadataHeaderNames = []string{
	"From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Date",
	"Message-ID", "In-Reply-To", "References",
}

// NativeIndex is the store-backed native-id mapping the adapter needs
// (D-API-9). It is an interface so gmailapi never imports internal/store
// and so a future backend reuses the same seam. The UID it returns is a
// stable synthetic integer handle that lets the unmodified store/engine
// path address Gmail's opaque message ids.
type NativeIndex interface {
	NativeUID(ctx context.Context, native string) (uint32, bool, error)
	AllocNativeUIDs(ctx context.Context, natives []string) (map[string]uint32, error)
	NativeByUID(ctx context.Context, uid uint32) (string, bool, error)
	NativeByUIDs(ctx context.Context, uids []uint32) (map[uint32]string, error)
	KnownMemberUIDs(ctx context.Context, container string, uids []uint32) (map[uint32]bool, error)
}

// Config wires one Gmail API backend session.
type Config struct {
	Account string
	Client  *Client
	Native  NativeIndex
	Logger  *slog.Logger
	// BackfillQuery scopes the initial walk with Gmail search syntax;
	// BackfillLimit caps messages per container. Both bound the cold
	// start for large mailboxes (GMAIL_API_PLAN §14).
	BackfillQuery string
	BackfillLimit int
}

// labelInfo is one Gmail label mapped into the mailbox vocabulary.
type labelInfo struct {
	ID       string
	Name     string
	Role     string
	Implicit bool
	Total    int64
	Unread   int64
}

// bfState is one container's in-flight backfill walk. Page tokens are
// short-lived, so it is deliberately session-scoped: a restart re-walks
// the list and skips ids the store already holds (GMAIL_API_PLAN §6.2).
type bfState struct {
	page   string
	listed int
	done   bool
}

// Backend adapts the Gmail REST API to mailbackend.Backend (D-API-2).
// M10 is read-only: discovery, metadata backfill, history incremental and
// hydration. Writes land in M11.
type Backend struct {
	cfg Config
	log *slog.Logger

	labels     map[string]labelInfo // path → info
	labelsByID map[string]labelInfo
	bf         map[string]*bfState
}

// NewBackend returns a lazy Gmail API backend: all work happens on the
// session's shared *Client, so there is no socket to dial.
func NewBackend(cfg Config) *Backend {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Backend{
		cfg:        cfg,
		log:        log,
		labels:     map[string]labelInfo{},
		labelsByID: map[string]labelInfo{},
		bf:         map[string]*bfState{},
	}
}

// Kind reports the backend implementation.
func (b *Backend) Kind() mb.Kind { return mb.KindGmailAPI }

// Capabilities reports the honest API-mode profile (D-API-5): snippets,
// labels, an implicit archive, no push yet (M13) and no custom keywords.
func (b *Backend) Capabilities() mb.Capabilities {
	return mb.Capabilities{
		Preview:         true,
		Labels:          true,
		Move:            false,
		UIDPlus:         false,
		CustomKeywords:  false,
		Push:            false,
		ImplicitArchive: true,
	}
}

// TierName labels the metric family; API mode has one strategy (history).
func (b *Backend) TierName() string { return "history" }

// Connect is a no-op: the HTTP client is stateless and pooled.
func (b *Backend) Connect(context.Context) error { return nil }

// Close is a no-op; the shared client outlives sessions.
func (b *Backend) Close() error { return nil }

// Ping always succeeds once credentials are in place; the first real call
// classifies an auth failure (FR-D.4).
func (b *Backend) Ping(context.Context) error { return nil }

// Release is a no-op for a stateless backend.
func (b *Backend) Release(context.Context) error { return nil }

// Folders maps Gmail labels into mailboxes (GMAIL_API_PLAN §6.1) and adds
// the synthetic All Mail archive.
func (b *Backend) Folders(ctx context.Context) ([]mb.Folder, error) {
	resp, err := b.cfg.Client.listLabels(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	b.labels = make(map[string]labelInfo, len(resp.Labels)+1)
	b.labelsByID = make(map[string]labelInfo, len(resp.Labels)+1)
	out := make([]mb.Folder, 0, len(resp.Labels)+1)
	for _, l := range resp.Labels {
		name, role, ok := mapLabel(l)
		if !ok {
			continue
		}
		info := labelInfo{ID: l.Id, Name: name, Role: role, Total: l.MessagesTotal, Unread: l.MessagesUnread}
		b.labels[name] = info
		b.labelsByID[l.Id] = info
		out = append(out, mb.Folder{
			Container: name, Name: name, Delim: '/', Role: role, NativeID: l.Id,
		})
	}
	all := labelInfo{Name: allMailName, Role: "archive", Implicit: true}
	b.labels[allMailName] = all
	out = append(out, mb.Folder{
		Container: allMailName, Name: allMailName, Delim: 0,
		Role: "archive", Implicit: true,
	})
	b.log.Debug("gmailapi: labels mapped", "account", b.cfg.Account, "labels", len(out))
	return out, nil
}

// mapLabel maps one Gmail label to a JMAP mailbox name and role.
// IMPORTANT/STARRED/UNREAD are keywords, not places, and CHAT is not
// mail, so those are hidden (GMAIL_API_PLAN §6.1).
func mapLabel(l *gmail.Label) (name, role string, ok bool) {
	switch l.Id {
	case LabelInbox:
		return "INBOX", "inbox", true
	case LabelSent:
		return "Sent", "sent", true
	case LabelDraft:
		return "Drafts", "drafts", true
	case LabelTrash:
		return "Trash", "trash", true
	case LabelSpam:
		return "Spam", "junk", true
	case LabelImportant, LabelStarred, LabelUnread, LabelChat:
		return "", "", false
	}
	if l.Name == "" {
		return l.Id, "", true
	}
	return l.Name, "", true
}

// FolderStatus reports one container's counts and the account historyId
// as its modseq, which seeds the incremental cursor (GMAIL_API_PLAN §6.3).
func (b *Backend) FolderStatus(ctx context.Context, container string) (mb.FolderStatus, error) {
	profile, err := b.cfg.Client.profile(ctx)
	if err != nil {
		return mb.FolderStatus{}, mapErr(err)
	}
	st := mb.FolderStatus{Version: apiUIDValidity, ModSeq: profile.HistoryId}
	if container == allMailName {
		st.Messages = uint32(profile.MessagesTotal)
		return st, nil
	}
	info, ok := b.labels[container]
	if !ok {
		return mb.FolderStatus{}, fmt.Errorf("gmailapi: unknown container %q", container)
	}
	st.Messages = uint32(info.Total)
	st.Unseen = uint32(info.Unread)
	return st, nil
}

// Backfill walks one container's headers in resumable list batches
// (GMAIL_API_PLAN §6.2). Page tokens are session-scoped, so a restart
// re-walks and skips ids the store already holds.
func (b *Backend) Backfill(ctx context.Context, container string, c mb.Cursor, batch int, _ mb.Hooks) ([]mb.Header, mb.Cursor, error) {
	if batch <= 0 {
		batch = 500
	}
	st := b.bf[container]
	if st == nil {
		st = &bfState{}
		b.bf[container] = st
	}
	if st.done {
		c.Version = apiUIDValidity
		c.BackfillDone = true
		return nil, c, nil
	}
	max := int64(batch)
	if lim := b.cfg.BackfillLimit; lim > 0 {
		if st.listed >= lim {
			st.done = true
			c.Version = apiUIDValidity
			c.BackfillDone = true
			return nil, c, nil
		}
		if remaining := int64(lim - st.listed); remaining < max {
			max = remaining
		}
	}
	labelIDs, includeSpam := b.containerFilter(container)
	resp, err := b.cfg.Client.listMessagesPage(ctx, listOpts{
		Query: b.cfg.BackfillQuery, LabelIDs: labelIDs, Max: max,
		PageToken: st.page, IncludeSpamTrash: includeSpam,
	})
	if err != nil {
		return nil, c, mapErr(err)
	}
	ids := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		if m.Id != "" {
			ids = append(ids, m.Id)
		}
	}
	st.listed += len(ids)
	headers, err := b.headersFor(ctx, container, ids)
	if err != nil {
		return nil, c, err
	}
	st.page = resp.NextPageToken
	if resp.NextPageToken == "" || (b.cfg.BackfillLimit > 0 && st.listed >= b.cfg.BackfillLimit) {
		st.done = true
	}
	c.Version = apiUIDValidity
	c.BackfillMark = uint32(st.listed)
	c.BackfillDone = st.done
	b.log.Debug("gmailapi: backfill batch", "container", container, "listed", st.listed, "new", len(headers))
	return headers, c, nil
}

// headersFor allocates UIDs for ids and returns header records for those
// not already present in the container, fetching metadata in one batch.
func (b *Backend) headersFor(ctx context.Context, container string, ids []string) ([]mb.Header, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	uidMap, err := b.cfg.Native.AllocNativeUIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	uids := make([]uint32, 0, len(ids))
	for _, id := range ids {
		uids = append(uids, uidMap[id])
	}
	members, err := b.cfg.Native.KnownMemberUIDs(ctx, container, uids)
	if err != nil {
		return nil, err
	}
	var want []string
	var wantUID []uint32
	for _, id := range ids {
		uid := uidMap[id]
		if members[uid] {
			continue
		}
		want = append(want, id)
		wantUID = append(wantUID, uid)
	}
	return b.fetchMetadata(ctx, container, want, wantUID)
}

// fetchMetadata batch-fetches headers for ids and builds Header records
// under the given container.
func (b *Backend) fetchMetadata(ctx context.Context, container string, ids []string, uids []uint32) ([]mb.Header, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatMetadata, metadataHeaderNames)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]mb.Header, 0, len(ids))
	for i, id := range ids {
		m := msgs[id]
		if m == nil {
			continue
		}
		out = append(out, b.headerFrom(container, uids[i], m))
	}
	return out, nil
}

// headerFrom maps one Gmail message resource into the neutral Header.
func (b *Backend) headerFrom(container string, uid uint32, m *gmail.Message) mb.Header {
	return mb.Header{
		Ref:          mb.NewRef(container, apiUIDValidity, uid),
		Flags:        keyword.ToIMAP(KeywordsFromLabels(m.LabelIds)),
		InternalDate: time.UnixMilli(m.InternalDate),
		Size:         m.SizeEstimate,
		Envelope:     envelopeFrom(m),
		ThreadKey:    ThreadKey(m.ThreadId),
		Preview:      DecodeSnippet(m.Snippet),
	}
}

// envelopeFrom builds the neutral envelope from Gmail's metadata headers.
func envelopeFrom(m *gmail.Message) convert.Envelope {
	get := func(name string) string {
		if m.Payload == nil {
			return ""
		}
		for _, h := range m.Payload.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value
			}
		}
		return ""
	}
	return convert.Envelope{
		Date:       get("Date"),
		Subject:    get("Subject"),
		From:       parseAddresses(get("From")),
		Sender:     parseAddresses(get("Sender")),
		ReplyTo:    parseAddresses(get("Reply-To")),
		To:         parseAddresses(get("To")),
		Cc:         parseAddresses(get("Cc")),
		Bcc:        parseAddresses(get("Bcc")),
		MessageID:  get("Message-ID"),
		InReplyTo:  get("In-Reply-To"),
		References: get("References"),
	}
}

// parseAddresses parses an address header, tolerating an unparseable one
// (the summary simply carries no addresses rather than failing ingest).
func parseAddresses(v string) []convert.Address {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return nil
	}
	out := make([]convert.Address, 0, len(list))
	for _, a := range list {
		mailbox, host := "", a.Address
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			mailbox, host = a.Address[:i], a.Address[i+1:]
		}
		out = append(out, convert.Address{Name: a.Name, Mailbox: mailbox, Host: host})
	}
	return out
}

// Incremental applies one container's history delta (GMAIL_API_PLAN §6.3).
// Gmail history is account-global; each container walks it since its own
// saved historyId and filters to its label, so a message's membership and
// keyword changes land in every container that holds it exactly once.
func (b *Backend) Incremental(ctx context.Context, container string, c mb.Cursor, hooks mb.Hooks) (mb.Delta, mb.Cursor, error) {
	next := c
	next.Version = apiUIDValidity
	if c.ModSeq == 0 {
		// No saved cursor: prime it from the current historyId without
		// replaying (a fresh backfill already holds the state).
		profile, err := b.cfg.Client.profile(ctx)
		if err != nil {
			return mb.Delta{}, c, mapErr(err)
		}
		next.ModSeq = profile.HistoryId
		return mb.Delta{}, next, nil
	}
	touched := map[string]bool{}
	deleted := map[string]bool{}
	var histID uint64
	page := ""
	for {
		resp, err := b.cfg.Client.listHistory(ctx, c.ModSeq, page)
		if err != nil {
			if IsNotFound(err) {
				return b.fullResync(ctx, container, next, hooks)
			}
			return mb.Delta{}, c, mapErr(err)
		}
		if resp.HistoryId > 0 {
			histID = resp.HistoryId
		}
		for _, h := range resp.History {
			for _, ma := range h.MessagesAdded {
				if id := messageID(ma.Message); id != "" {
					touched[id] = true
				}
			}
			for _, la := range h.LabelsAdded {
				if id := messageID(la.Message); id != "" {
					touched[id] = true
				}
			}
			for _, lr := range h.LabelsRemoved {
				if id := messageID(lr.Message); id != "" {
					touched[id] = true
				}
			}
			for _, md := range h.MessagesDeleted {
				if id := messageID(md.Message); id != "" {
					deleted[id] = true
				}
			}
		}
		page = resp.NextPageToken
		if page == "" {
			break
		}
	}
	if histID > 0 {
		next.ModSeq = histID
	}
	delta, err := b.applyChanges(ctx, container, touched, deleted, hooks)
	return delta, next, err
}

// applyChanges resolves touched/deleted native ids against the container
// and emits headers, flag changes and tombstones.
func (b *Backend) applyChanges(ctx context.Context, container string, touched, deleted map[string]bool, hooks mb.Hooks) (mb.Delta, error) {
	known, err := knownUIDs(hooks)
	if err != nil {
		return mb.Delta{}, err
	}
	var delta mb.Delta
	for id := range deleted {
		uid, ok, err := b.cfg.Native.NativeUID(ctx, id)
		if err != nil {
			return mb.Delta{}, err
		}
		if ok && known[uid] {
			delta.Removed = append(delta.Removed, mb.NewRef(container, apiUIDValidity, uid))
			delete(known, uid)
		}
	}
	if len(touched) == 0 {
		return delta, nil
	}
	ids := make([]string, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	uidMap, err := b.cfg.Native.AllocNativeUIDs(ctx, ids)
	if err != nil {
		return mb.Delta{}, err
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatMetadata, metadataHeaderNames)
	if err != nil {
		return mb.Delta{}, mapErr(err)
	}
	labelID := b.labelID(container)
	for _, id := range ids {
		m := msgs[id]
		if m == nil {
			continue
		}
		uid := uidMap[id]
		if !b.inContainer(container, labelID, m.LabelIds) {
			if known[uid] {
				delta.Removed = append(delta.Removed, mb.NewRef(container, apiUIDValidity, uid))
				delete(known, uid)
			}
			continue
		}
		if known[uid] {
			delta.Flags = append(delta.Flags, mb.FlagChange{
				Ref:   mb.NewRef(container, apiUIDValidity, uid),
				Flags: keyword.ToIMAP(KeywordsFromLabels(m.LabelIds)),
			})
			continue
		}
		delta.Headers = append(delta.Headers, b.headerFrom(container, uid, m))
		known[uid] = true
	}
	return delta, nil
}

// fullResync is the history-expiry path (GMAIL_API_PLAN §6.3): re-list the
// container, ingest anything unseen, update flags on the known, and
// tombstone anything the container no longer holds. The caller advances
// the cursor to the fresh historyId.
func (b *Backend) fullResync(ctx context.Context, container string, c mb.Cursor, hooks mb.Hooks) (mb.Delta, mb.Cursor, error) {
	b.log.Warn("gmailapi: history expired, full resync", "container", container)
	profile, err := b.cfg.Client.profile(ctx)
	if err != nil {
		return mb.Delta{}, c, mapErr(err)
	}
	next := c
	next.Version = apiUIDValidity
	next.ModSeq = profile.HistoryId

	known, err := knownUIDs(hooks)
	if err != nil {
		return mb.Delta{}, c, err
	}
	ids, err := b.listAll(ctx, container)
	if err != nil {
		return mb.Delta{}, c, err
	}
	uidMap, err := b.cfg.Native.AllocNativeUIDs(ctx, ids)
	if err != nil {
		return mb.Delta{}, c, err
	}
	uids := make([]uint32, 0, len(ids))
	for _, id := range ids {
		uids = append(uids, uidMap[id])
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatMetadata, metadataHeaderNames)
	if err != nil {
		return mb.Delta{}, c, mapErr(err)
	}
	var delta mb.Delta
	present := make(map[uint32]bool, len(ids))
	for i, id := range ids {
		present[uids[i]] = true
		m := msgs[id]
		if m == nil {
			continue
		}
		if known[uids[i]] {
			delta.Flags = append(delta.Flags, mb.FlagChange{
				Ref:   mb.NewRef(container, apiUIDValidity, uids[i]),
				Flags: keyword.ToIMAP(KeywordsFromLabels(m.LabelIds)),
			})
			continue
		}
		delta.Headers = append(delta.Headers, b.headerFrom(container, uids[i], m))
	}
	for uid := range known {
		if !present[uid] {
			delta.Removed = append(delta.Removed, mb.NewRef(container, apiUIDValidity, uid))
		}
	}
	return delta, next, nil
}

// listAll pages every message id the container currently holds.
func (b *Backend) listAll(ctx context.Context, container string) ([]string, error) {
	labelIDs, includeSpam := b.containerFilter(container)
	var out []string
	page := ""
	for {
		resp, err := b.cfg.Client.listMessagesPage(ctx, listOpts{
			Query: b.cfg.BackfillQuery, LabelIDs: labelIDs, Max: 500,
			PageToken: page, IncludeSpamTrash: includeSpam,
		})
		if err != nil {
			return nil, mapErr(err)
		}
		for _, m := range resp.Messages {
			if m.Id != "" {
				out = append(out, m.Id)
			}
		}
		page = resp.NextPageToken
		if page == "" {
			return out, nil
		}
	}
}

// FetchHeaders fetches header records for specific refs of a container.
func (b *Backend) FetchHeaders(ctx context.Context, container string, refs []mb.Ref) ([]mb.Header, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	uids := make([]uint32, 0, len(refs))
	for _, r := range refs {
		if uid, ok := r.UID(); ok {
			uids = append(uids, uid)
		}
	}
	natives, err := b.cfg.Native.NativeByUIDs(ctx, uids)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(uids))
	for _, uid := range uids {
		if id, ok := natives[uid]; ok {
			ids = append(ids, id)
		}
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatMetadata, metadataHeaderNames)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]mb.Header, 0, len(ids))
	for _, uid := range uids {
		if m := msgs[natives[uid]]; m != nil {
			out = append(out, b.headerFrom(container, uid, m))
		}
	}
	return out, nil
}

// Watch reports no push: M10 uses the engine's poll ticker (push is M13).
func (b *Backend) Watch(context.Context, string) (<-chan struct{}, error) { return nil, nil }

// FetchPreviews returns snippet text per ref.
func (b *Backend) FetchPreviews(ctx context.Context, refs []mb.Ref) (map[mb.Ref]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	uids := make([]uint32, 0, len(refs))
	for _, r := range refs {
		if uid, ok := r.UID(); ok {
			uids = append(uids, uid)
		}
	}
	natives, err := b.cfg.Native.NativeByUIDs(ctx, uids)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(uids))
	for _, uid := range uids {
		if id, ok := natives[uid]; ok {
			ids = append(ids, id)
		}
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatMetadata, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make(map[mb.Ref]string, len(refs))
	for _, r := range refs {
		uid, _ := r.UID()
		if m := msgs[natives[uid]]; m != nil {
			out[r] = DecodeSnippet(m.Snippet)
		}
	}
	return out, nil
}

// FetchRaw downloads one message's full bytes.
func (b *Backend) FetchRaw(ctx context.Context, ref mb.Ref) ([]byte, error) {
	uid, ok := ref.UID()
	if !ok {
		return nil, fmt.Errorf("gmailapi: invalid ref %q", ref.Native)
	}
	native, ok, err := b.cfg.Native.NativeByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("gmailapi: no native message for uid %d", uid)
	}
	m, err := b.cfg.Client.getMessage(ctx, native, formatRaw)
	if err != nil {
		return nil, mapErr(err)
	}
	return decodeRaw(m.Raw)
}

// FetchRawBatch downloads several full messages in one batch call.
func (b *Backend) FetchRawBatch(ctx context.Context, container string, refs []mb.Ref) (map[mb.Ref][]byte, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	uids := make([]uint32, 0, len(refs))
	for _, r := range refs {
		if uid, ok := r.UID(); ok {
			uids = append(uids, uid)
		}
	}
	natives, err := b.cfg.Native.NativeByUIDs(ctx, uids)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(uids))
	for _, uid := range uids {
		if id, ok := natives[uid]; ok {
			ids = append(ids, id)
		}
	}
	msgs, err := b.cfg.Client.batchGetMessages(ctx, ids, formatRaw, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make(map[mb.Ref][]byte, len(refs))
	for _, r := range refs {
		uid, _ := r.UID()
		m := msgs[natives[uid]]
		if m == nil {
			continue
		}
		raw, derr := decodeRaw(m.Raw)
		if derr != nil {
			return nil, derr
		}
		out[r] = raw
	}
	_ = container
	return out, nil
}

// --- write surface (M11) ---

// errWriteUnsupported is returned by every mutation in M10; the read-only
// milestone advertises no write capability and refuses rather than
// pretending (golden rule 4).
var errWriteUnsupported = errors.New("gmail_api: write support lands with M11")

// StoreKeywords is not available in M10.
func (b *Backend) StoreKeywords(context.Context, []mb.Copy, []string, []string) error {
	return errWriteUnsupported
}

// SetMembership is not available in M10.
func (b *Backend) SetMembership(context.Context, mb.Message, []mb.Copy, []mb.Mailbox, []mb.Mailbox) ([]mb.Copy, []mb.Ref, error) {
	return nil, nil, errWriteUnsupported
}

// Destroy is not available in M10.
func (b *Backend) Destroy(context.Context, []mb.Ref, string) ([]mb.Ref, error) {
	return nil, errWriteUnsupported
}

// Append is not available in M10.
func (b *Backend) Append(context.Context, string, []byte, []string, *time.Time) (mb.Ref, []string, error) {
	return mb.Ref{}, nil, errWriteUnsupported
}

// CreateMailbox is not available in M10.
func (b *Backend) CreateMailbox(context.Context, string) error { return errWriteUnsupported }

// RenameMailbox is not available in M10.
func (b *Backend) RenameMailbox(context.Context, string, string) error { return errWriteUnsupported }

// DeleteMailbox is not available in M10.
func (b *Backend) DeleteMailbox(context.Context, string) error { return errWriteUnsupported }

// HierarchyDelim is Gmail's label path separator.
func (b *Backend) HierarchyDelim(context.Context) (rune, error) { return '/', nil }

// --- helpers ---

// containerFilter returns the label filter for a container and whether
// spam/trash must be included (All Mail and the junk/trash labels).
func (b *Backend) containerFilter(container string) (labelIDs []string, includeSpam bool) {
	if container == allMailName {
		return nil, true
	}
	info, ok := b.labels[container]
	if !ok || info.ID == "" {
		return nil, true
	}
	if info.ID == LabelSpam || info.ID == LabelTrash {
		includeSpam = true
	}
	return []string{info.ID}, includeSpam
}

// labelID returns the Gmail label id for a container path; "" for All
// Mail (which every message belongs to).
func (b *Backend) labelID(container string) string {
	if container == allMailName {
		return ""
	}
	return b.labels[container].ID
}

// inContainer reports whether a message's label set places it in the
// container; the synthetic All Mail container holds everything.
func (b *Backend) inContainer(container, labelID string, labels []string) bool {
	if container == allMailName {
		return true
	}
	if labelID == "" {
		return false
	}
	for _, id := range labels {
		if id == labelID {
			return true
		}
	}
	return false
}

// messageID extracts a message id from a possibly-partial history message.
func messageID(m *gmail.Message) string {
	if m == nil {
		return ""
	}
	return m.Id
}

// knownUIDs materialises the hook's container refs into a uid set.
func knownUIDs(hooks mb.Hooks) (map[uint32]bool, error) {
	out := map[uint32]bool{}
	if hooks.KnownUIDs == nil {
		return out, nil
	}
	refs, err := hooks.KnownUIDs()
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if uid, ok := r.UID(); ok {
			out[uid] = true
		}
	}
	return out, nil
}

// mapErr maps the client's already-classified errors onto the neutral
// mailbackend taxonomy (the client already uses ErrThrottled/ErrAuth).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	return err
}

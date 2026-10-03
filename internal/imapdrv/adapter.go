package imapdrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// Backend adapts one IMAP session to the provider-neutral
// mailbackend.Backend interface (D-API-2, GMAIL_API_PLAN §3.1). All of
// the IMAP-specific strategy the sync engine used to encode — tier
// dispatch (QRESYNC/CONDSTORE/baseline), per-folder cursors, the Gmail
// label membership model and the shared-UID reconcile — lives here now;
// internal/sync sees only neutral types.
//
// The low-level *Conn methods and their tests remain unchanged beneath
// this adapter.
type Backend struct {
	cfg  Config
	log  *slog.Logger
	conn *Conn
	caps mb.Capabilities
}

// NewBackend returns a lazy IMAP backend: the socket opens on the first
// Connect/operation.
func NewBackend(cfg Config) *Backend {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Backend{cfg: cfg, log: log}
}

// Kind reports the backend implementation.
func (b *Backend) Kind() mb.Kind { return mb.KindIMAP }

// Capabilities reports what this session can do. It is populated at
// connect; before then it is the zero value (the engine always connects
// first).
func (b *Backend) Capabilities() mb.Capabilities { return b.caps }

// Connect dials if no session is up, then records the capability profile.
func (b *Backend) Connect(ctx context.Context) error {
	if b.conn != nil {
		return nil
	}
	conn, err := Dial(ctx, b.cfg)
	if err != nil {
		return mapErr(err)
	}
	b.conn = conn
	b.caps = mb.Capabilities{
		Preview:         conn.SupportsPREVIEW(),
		Labels:          conn.GmailExt(),
		Move:            conn.SupportsMove(),
		UIDPlus:         conn.SupportsUIDPlus(),
		CustomKeywords:  true,
		Push:            true, // IMAP IDLE; polling is the fallback
		ImplicitArchive: conn.GmailExt(),
	}
	return nil
}

// Close logs out and drops the session.
func (b *Backend) Close() error {
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	b.conn = nil
	return err
}

// Ping proves the session is alive without dialing (NOOP).
func (b *Backend) Ping(ctx context.Context) error {
	if b.conn == nil {
		return fmt.Errorf("imapdrv: not connected")
	}
	return mapErr(b.conn.Ping(ctx))
}

// TierLabel reports the active sync tier as an int for the FR-D.6
// metric; the engine discovers it through an optional interface so it
// need not know IMAP's tier vocabulary.
func (b *Backend) TierLabel() int {
	if b.conn == nil {
		return -1
	}
	return int(b.conn.Tier())
}

// TierName names the active tier for the metric label.
func (b *Backend) TierName() string {
	if b.conn == nil {
		return ""
	}
	return b.conn.Tier().String()
}

// Compressed reports whether COMPRESS is active (FR-S.11 logs).
func (b *Backend) Compressed() bool { return b.conn != nil && b.conn.Compressed() }

// Release drops the connection's mailbox selection (UNSELECT).
func (b *Backend) Release(ctx context.Context) error {
	if b.conn == nil {
		return nil
	}
	return mapErr(b.conn.Unselect(ctx))
}

// connUp is the internal accessor; callers must have called Connect.
func (b *Backend) connUp() *Conn { return b.conn }

// Folders maps LIST output into neutral folders (FR-S.1).
func (b *Backend) Folders(ctx context.Context) ([]mb.Folder, error) {
	list, err := b.connUp().ListFolders(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]mb.Folder, 0, len(list))
	for _, f := range list {
		out = append(out, mb.Folder{
			Container: f.Name, Name: f.Name, Delim: f.Delim,
			Role: f.Role, NoSelect: f.NoSelect, Implicit: f.AllMail,
		})
	}
	return out, nil
}

// FolderStatus reads one container's STATUS (FR-S.3).
func (b *Backend) FolderStatus(ctx context.Context, container string) (mb.FolderStatus, error) {
	st, err := b.connUp().Status(ctx, container)
	if err != nil {
		return mb.FolderStatus{}, mapErr(err)
	}
	return mb.FolderStatus{
		Version: st.UIDValidity, Next: st.UIDNext,
		ModSeq: st.HighestModSeq, Messages: st.Messages, Unseen: st.Unseen,
	}, nil
}

// Backfill runs one batch of the folder's initial header walk (FR-S.3,
// FR-S.10): Gmail by sequence number, everyone else by uid, one
// EXAMINE reused across batches on the same session.
func (b *Backend) Backfill(ctx context.Context, container string, c mb.Cursor, batch int, _ mb.Hooks) ([]mb.Header, mb.Cursor, error) {
	conn := b.connUp()
	if conn.selected != container {
		if _, err := conn.Examine(ctx, container, nil); err != nil {
			return nil, c, mapErr(err)
		}
	}
	if batch <= 0 {
		batch = 500
	}
	if conn.GmailExt() {
		return b.backfillGmail(ctx, container, c, batch)
	}
	return b.backfillUID(ctx, container, c, batch)
}

func (b *Backend) backfillGmail(ctx context.Context, container string, c mb.Cursor, batch int) ([]mb.Header, mb.Cursor, error) {
	conn := b.connUp()
	cursor := uint64(c.BackfillMark) + 1
	if cursor > uint64(c.Messages) {
		c.BackfillDone = true
		return nil, c, nil
	}
	end := cursor + uint64(batch) - 1
	if end > uint64(c.Messages) {
		end = uint64(c.Messages)
	}
	seqs := make([]uint32, 0, end-cursor+1)
	for s := cursor; s <= end; s++ {
		seqs = append(seqs, uint32(s))
	}
	msgs, err := conn.FetchHeadersSeq(ctx, seqs)
	if err != nil {
		return nil, c, fmt.Errorf("backfill %q seq [%d..%d]: %w", container, cursor, end, err)
	}
	out := make([]mb.Header, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, headerOf(container, c.Version, m))
	}
	c.BackfillMark = uint32(end)
	if end >= uint64(c.Messages) {
		c.BackfillDone = true
	}
	b.log.Debug("imapdrv: backfill batch", "folder", container, "through", end, "of", c.Messages)
	return out, c, nil
}

func (b *Backend) backfillUID(ctx context.Context, container string, c mb.Cursor, batch int) ([]mb.Header, mb.Cursor, error) {
	conn := b.connUp()
	cursor := uint64(c.BackfillMark) + 1
	if cursor >= c.Next {
		c.BackfillDone = true
		return nil, c, nil
	}
	end := cursor + uint64(batch) - 1
	if end >= c.Next {
		end = c.Next - 1
	}
	uids := make([]uint32, 0, end-cursor+1)
	for u := cursor; u <= end; u++ {
		uids = append(uids, uint32(u))
	}
	msgs, err := conn.FetchHeaders(ctx, uids)
	if err != nil {
		return nil, c, fmt.Errorf("backfill %q [%d..%d]: %w", container, cursor, end, err)
	}
	out := make([]mb.Header, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, headerOf(container, c.Version, m))
	}
	c.BackfillMark = uint32(end)
	if uint64(c.BackfillMark)+1 >= c.Next {
		c.BackfillDone = true
	}
	b.log.Debug("imapdrv: backfill batch", "folder", container, "through", end, "of", c.Next)
	return out, c, nil
}

// Incremental is the tier ladder of FR-S.5: QRESYNC replays
// vanished+flag deltas from the select, CONDSTORE fetches CHANGEDSINCE
// and diffs uids, baseline refetches uid+flags wholesale. The reconcile
// (which uids the container no longer holds) uses the caller's cached
// listing through Hooks.KnownUIDs, since the store owns it.
func (b *Backend) Incremental(ctx context.Context, container string, c mb.Cursor, hooks mb.Hooks) (mb.Delta, mb.Cursor, error) {
	conn := b.connUp()
	var anchor *Anchor
	if conn.Tier() == TierQResync && c.Version != 0 {
		anchor = &Anchor{UIDValidity: c.Version, ModSeq: c.ModSeq}
	}
	sel, err := conn.Examine(ctx, container, anchor)
	if err != nil {
		return mb.Delta{}, c, mapErr(err)
	}
	if sel.Status.UIDValidity != c.Version || sel.ResyncRejected {
		// The anchor is stale: every cached reference may be wrong, and
		// the engine resets the container and lets the next pass
		// backfill afresh (FR-S.6).
		return mb.Delta{Reset: true}, c, nil
	}

	var delta mb.Delta
	for _, u := range sel.Vanished {
		delta.Removed = append(delta.Removed, mb.NewRef(container, sel.Status.UIDValidity, u))
	}

	// New messages: everything at or beyond the stored uidnext.
	if sel.Status.UIDNext > c.Next {
		for start := c.Next; start < sel.Status.UIDNext; {
			end := start + 200 - 1
			if end >= sel.Status.UIDNext {
				end = sel.Status.UIDNext - 1
			}
			uids := make([]uint32, 0, end-start+1)
			for u := start; u <= end; u++ {
				uids = append(uids, uint32(u))
			}
			msgs, err := conn.FetchHeaders(ctx, uids)
			if err != nil {
				return mb.Delta{}, c, fmt.Errorf("new uids %q: %w", container, err)
			}
			for _, m := range msgs {
				delta.Headers = append(delta.Headers, headerOf(container, sel.Status.UIDValidity, m))
			}
			start = end + 1
		}
	}

	switch conn.Tier() {
	case TierQResync:
		for _, fc := range sel.FlagChanges {
			delta.Flags = append(delta.Flags, flagChangeOfRef(container, sel.Status.UIDValidity, fc))
		}
	case TierCondStore:
		if c.ModSeq > 0 && !sel.NoModSeq {
			changed, vanished, err := conn.ChangesSince(ctx, c.ModSeq)
			if err != nil {
				return mb.Delta{}, c, mapErr(err)
			}
			for _, u := range vanished {
				delta.Removed = append(delta.Removed, mb.NewRef(container, sel.Status.UIDValidity, u))
			}
			for _, fc := range changed {
				delta.Flags = append(delta.Flags, flagChangeOfRef(container, sel.Status.UIDValidity, fc))
			}
		}
		if err := b.reconcileUIDs(ctx, container, sel.Status.UIDValidity, sel.Status.Messages, hooks, &delta); err != nil {
			return mb.Delta{}, c, err
		}
	default: // TierBaseline
		all, err := conn.AllFlags(ctx)
		if err != nil {
			return mb.Delta{}, c, mapErr(err)
		}
		delta.Bulk = true
		for _, fc := range all {
			delta.Flags = append(delta.Flags, flagChangeOfRef(container, sel.Status.UIDValidity, fc))
		}
		if err := b.reconcileFromFlags(ctx, container, sel.Status.UIDValidity, all, hooks, &delta); err != nil {
			return mb.Delta{}, c, err
		}
	}

	next := c
	next.Version = sel.Status.UIDValidity
	next.Next = sel.Status.UIDNext
	next.Messages = sel.Status.Messages
	if sel.Status.HighestModSeq > 0 {
		next.ModSeq = sel.Status.HighestModSeq
	}
	return delta, next, nil
}

// reconcileUIDs diffs the server's uid list against the cached one and
// reports the difference as Removed. Gated on the server's own count
// first: when EXISTS equals the cached live count, no expunge can have
// gone unnoticed, and skipping the sweep is what keeps a busy Gmail
// folder from tripping the provider's throttle (FR-S.12).
func (b *Backend) reconcileUIDs(ctx context.Context, container string, uv, exists uint32, hooks mb.Hooks, delta *mb.Delta) error {
	if hooks.KnownUIDs == nil {
		return nil
	}
	known, err := hooks.KnownUIDs()
	if err != nil {
		return err
	}
	if exists == uint32(len(known)) {
		return nil
	}
	server, err := b.connUp().UIDs(ctx)
	if err != nil {
		return mapErr(err)
	}
	have := make(map[uint32]bool, len(server))
	for _, u := range server {
		have[u] = true
	}
	for _, r := range known {
		uid, ok := r.UID()
		if ok && !have[uid] {
			delta.Removed = append(delta.Removed, mb.NewRef(container, uv, uid))
		}
	}
	return nil
}

// reconcileFromFlags is the baseline tier's reconcile, using the uid set
// that arrived attached to the full flag scan.
func (b *Backend) reconcileFromFlags(ctx context.Context, container string, uv uint32, all []FlagChange, hooks mb.Hooks, delta *mb.Delta) error {
	if hooks.KnownUIDs == nil {
		return nil
	}
	known, err := hooks.KnownUIDs()
	if err != nil {
		return err
	}
	have := make(map[uint32]bool, len(all))
	for _, fc := range all {
		have[fc.UID] = true
	}
	for _, r := range known {
		uid, ok := r.UID()
		if ok && !have[uid] {
			delta.Removed = append(delta.Removed, mb.NewRef(container, uv, uid))
		}
	}
	return nil
}

// FetchHeaders fetches header records for specific refs (the
// flag-change path where a reported uid turns out to be a new message).
func (b *Backend) FetchHeaders(ctx context.Context, container string, refs []mb.Ref) ([]mb.Header, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	conn := b.connUp()
	if conn.selected != container {
		if _, err := conn.Examine(ctx, container, nil); err != nil {
			return nil, mapErr(err)
		}
	}
	var out []mb.Header
	for start := 0; start < len(refs); start += 200 {
		end := min(start+200, len(refs))
		uids := make([]uint32, 0, end-start)
		uv := refs[start].VersionNum()
		for _, r := range refs[start:end] {
			if uid, ok := r.UID(); ok {
				uids = append(uids, uid)
			}
		}
		msgs, err := conn.FetchHeaders(ctx, uids)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, m := range msgs {
			out = append(out, headerOf(container, uv, m))
		}
	}
	return out, nil
}

// Watch enters IDLE on a container and returns a channel that fires on
// any unilateral change. IDLE does not end on a notification (RFC
// 2177); the channel closes when the session dies or ctx ends so the
// engine reconnects.
func (b *Backend) Watch(ctx context.Context, container string) (<-chan struct{}, error) {
	conn := b.connUp()
	if _, err := conn.Examine(ctx, container, nil); err != nil {
		return nil, mapErr(err)
	}
	session, err := conn.StartIdle()
	if err != nil {
		return nil, mapErr(err)
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		for {
			select {
			case <-conn.Notes():
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-session.IdleDone():
				_ = conn.EndIdle(session)
				return
			case <-ctx.Done():
				_ = conn.EndIdle(session)
				return
			}
		}
	}()
	return ch, nil
}

// FetchPreviews returns preview text per ref, grouping by container so
// one EXAMINE serves each group.
func (b *Backend) FetchPreviews(ctx context.Context, refs []mb.Ref) (map[mb.Ref]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	conn := b.connUp()
	byFolder := map[string][]mb.Ref{}
	for _, r := range refs {
		byFolder[r.Container] = append(byFolder[r.Container], r)
	}
	out := make(map[mb.Ref]string, len(refs))
	for folder, refs := range byFolder {
		if _, err := conn.Examine(ctx, folder, nil); err != nil {
			return nil, mapErr(err)
		}
		uids := make([]uint32, 0, len(refs))
		byUID := map[uint32]mb.Ref{}
		for _, r := range refs {
			if uid, ok := r.UID(); ok {
				uids = append(uids, uid)
				byUID[uid] = r
			}
		}
		previews, err := conn.FetchPreviews(ctx, uids)
		if err != nil {
			return nil, mapErr(err)
		}
		for uid, p := range previews {
			if p != "" {
				out[byUID[uid]] = p
			}
		}
	}
	_ = conn.Unselect(context.Background())
	return out, nil
}

// FetchRaw downloads one message's full bytes.
func (b *Backend) FetchRaw(ctx context.Context, ref mb.Ref) ([]byte, error) {
	conn := b.connUp()
	if _, err := conn.Examine(ctx, ref.Container, nil); err != nil {
		return nil, mapErr(err)
	}
	uid, _ := ref.UID()
	raw, err := conn.FetchBody(ctx, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	return raw, nil
}

// FetchRawBatch downloads several full messages of one container in one
// batched fetch.
func (b *Backend) FetchRawBatch(ctx context.Context, container string, refs []mb.Ref) (map[mb.Ref][]byte, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	conn := b.connUp()
	uids := make([]uint32, 0, len(refs))
	byUID := map[uint32]mb.Ref{}
	for _, r := range refs {
		if uid, ok := r.UID(); ok {
			uids = append(uids, uid)
			byUID[uid] = r
		}
	}
	bodies, err := conn.FetchBodies(ctx, container, uids)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make(map[mb.Ref][]byte, len(bodies))
	for uid, raw := range bodies {
		out[byUID[uid]] = raw
	}
	return out, nil
}

// StoreKeywords maps neutral keywords to IMAP flags and STOREs them.
// On a label server flags are per message, so one command covers every
// copy (FR-S.10).
func (b *Backend) StoreKeywords(ctx context.Context, copies []mb.Copy, add, remove []string) error {
	if len(copies) == 0 || (len(add) == 0 && len(remove) == 0) {
		return nil
	}
	conn := b.connUp()
	byFolder := map[string][]uint32{}
	for _, c := range copies {
		if uid, ok := c.Ref.UID(); ok && uid != 0 {
			byFolder[c.Ref.Container] = append(byFolder[c.Ref.Container], uid)
		}
	}
	if conn.GmailExt() && len(byFolder) > 1 {
		// Flags are per message, not per copy: one folder's uids suffice.
		for folder, uids := range byFolder {
			byFolder = map[string][]uint32{folder: uids}
			break
		}
	}
	addFlags := keyword.ToIMAP(truthMap(add))
	removeFlags := keyword.ToIMAP(truthMap(remove))
	for folder, uids := range byFolder {
		if err := conn.StoreFlags(ctx, folder, uids, addFlags, removeFlags); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// SetMembership changes message membership: Gmail labels on a label
// server (FR-S.10), COPY/MOVE/EXPUNGE elsewhere (PLAN §7.1).
func (b *Backend) SetMembership(ctx context.Context, msg mb.Message, copies []mb.Copy, add, remove []mb.Mailbox) ([]mb.Copy, []mb.Ref, error) {
	if b.connUp().GmailExt() {
		return b.setMembershipGmail(ctx, msg, copies, add, remove)
	}
	return b.setMembershipCopies(ctx, copies, add, remove)
}

// setMembershipCopies performs the server side of a membership change
// without labels: one removal plus one addition becomes a MOVE;
// anything else is copy-then-expunge.
func (b *Backend) setMembershipCopies(ctx context.Context, copies []mb.Copy, add, remove []mb.Mailbox) ([]mb.Copy, []mb.Ref, error) {
	conn := b.connUp()
	remPaths := map[string]string{} // container → mailbox id
	for _, m := range remove {
		remPaths[m.Path] = m.ID
	}
	if len(add) == 0 {
		touched, err := b.expungeCopies(ctx, copies, remove)
		return nil, touched, err
	}

	var moveSrc *mb.Copy
	for i := range copies {
		if _, ok := remPaths[copies[i].Ref.Container]; ok {
			if uid, ok := copies[i].Ref.UID(); ok && uid != 0 {
				moveSrc = &copies[i]
				break
			}
		}
	}
	if len(add) == 1 && moveSrc != nil {
		uid, _ := moveSrc.Ref.UID()
		res, err := conn.MoveUIDs(ctx, moveSrc.Ref.Container, add[0].Path, []uint32{uid})
		if err != nil {
			return nil, nil, mapErr(err)
		}
		added := []mb.Copy{{
			MailboxID: add[0].ID,
			Ref:       mb.NewRef(add[0].Path, res.DestUIDValidity, res.DestUIDs[uid]),
		}}
		// Expunge the other removals, but not the one the MOVE consumed.
		var rest []mb.Mailbox
		for _, m := range remove {
			if m.Path != moveSrc.Ref.Container {
				rest = append(rest, m)
			}
		}
		touched, err := b.expungeCopies(ctx, copies, rest)
		if err != nil {
			return nil, nil, err
		}
		return added, append(touched, moveSrc.Ref), nil
	}

	var src *mb.Copy
	for i := range copies {
		if uid, ok := copies[i].Ref.UID(); ok && uid != 0 {
			src = &copies[i]
			break
		}
	}
	if src == nil {
		return nil, nil, fmt.Errorf("imapdrv: no folder holds a known uid for this message")
	}
	srcUID, _ := src.Ref.UID()
	added := make([]mb.Copy, 0, len(add))
	for _, dst := range add {
		res, err := conn.CopyUIDs(ctx, src.Ref.Container, dst.Path, []uint32{srcUID})
		if err != nil {
			return nil, nil, mapErr(err)
		}
		added = append(added, mb.Copy{
			MailboxID: dst.ID,
			Ref:       mb.NewRef(dst.Path, res.DestUIDValidity, res.DestUIDs[srcUID]),
		})
	}
	touched, err := b.expungeCopies(ctx, copies, remove)
	if err != nil {
		return nil, nil, err
	}
	return added, touched, nil
}

// expungeCopies permanently removes the named containers' copies.
func (b *Backend) expungeCopies(ctx context.Context, copies []mb.Copy, remove []mb.Mailbox) ([]mb.Ref, error) {
	if len(remove) == 0 {
		return nil, nil
	}
	conn := b.connUp()
	remPaths := map[string]bool{}
	for _, m := range remove {
		remPaths[m.Path] = true
	}
	byFolder := map[string][]uint32{}
	var touched []mb.Ref
	for _, c := range copies {
		if !remPaths[c.Ref.Container] {
			continue
		}
		uid, ok := c.Ref.UID()
		if !ok || uid == 0 {
			return nil, fmt.Errorf("imapdrv: no uid for folder %q yet", c.Ref.Container)
		}
		byFolder[c.Ref.Container] = append(byFolder[c.Ref.Container], uid)
		touched = append(touched, c.Ref)
	}
	for folder, uids := range byFolder {
		if err := conn.ExpungeUIDs(ctx, folder, uids); err != nil {
			return nil, mapErr(err)
		}
	}
	return touched, nil
}

// gmailLabelFor maps a mailbox to the Gmail label that implements it
// (FR-S.10).
func gmailLabelFor(role, path string) string {
	switch role {
	case "inbox":
		return `\Inbox`
	case "sent":
		return `\Sent`
	case "drafts":
		return `\Draft`
	case "trash":
		return `\Trash`
	case "junk":
		return `\Spam`
	}
	switch leafName(path) {
	case "Starred":
		return `\Starred`
	case "Important":
		return `\Important`
	}
	return path
}

func leafName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '.' {
			return path[i+1:]
		}
	}
	return path
}

// setMembershipGmail is the label half of a membership change
// (FR-S.10): one pair of X-GM-LABELS STOREs replaces copy storms.
func (b *Backend) setMembershipGmail(ctx context.Context, msg mb.Message, copies []mb.Copy, add, remove []mb.Mailbox) ([]mb.Copy, []mb.Ref, error) {
	conn := b.connUp()
	var addLabels, removeLabels []string
	// Every add is a membership the server will hold — a label write
	// accepts it, or it is implicit and commits locally.
	added := make([]mb.Copy, 0, len(add))
	for _, m := range add {
		if m.Implicit {
			var uid uint32
			var uv uint32
			for _, c := range copies {
				if c.MailboxID == m.ID {
					uid, _ = c.Ref.UID()
					uv = c.Ref.VersionNum()
					break
				}
			}
			added = append(added, mb.Copy{MailboxID: m.ID, Ref: mb.NewRef(m.Path, uv, uid)})
			continue
		}
		addLabels = append(addLabels, gmailLabelFor(m.Role, m.Path))
		added = append(added, mb.Copy{MailboxID: m.ID, Ref: mb.NewRef(m.Path, 0, 0)})
	}
	for _, m := range remove {
		if m.Implicit {
			// All Mail holds everything; no label write can remove it.
			return nil, nil, &mb.RejectedError{Text: "membership in " + m.Path + " is managed by the server and cannot be removed"}
		}
		removeLabels = append(removeLabels, gmailLabelFor(m.Role, m.Path))
	}
	if len(addLabels) == 0 && len(removeLabels) == 0 {
		return added, nil, nil
	}

	src := b.gmailLabelSource(ctx, copies, remove, msg)
	if src == nil {
		return nil, nil, fmt.Errorf("imapdrv: no folder holds a known uid for this message")
	}
	uid, _ := src.Ref.UID()
	if err := conn.StoreGmLabels(ctx, src.Ref.Container, []uint32{uid}, addLabels, removeLabels); err != nil {
		return nil, nil, mapErr(err)
	}
	var touched []mb.Ref
	for _, c := range copies {
		if _, ok := c.Ref.UID(); ok {
			touched = append(touched, c.Ref)
		}
	}
	return added, touched, nil
}

// gmailLabelSource chooses the copy a label write is issued from. It
// prefers a container the patch is not removing — Gmail accepts a
// command that would remove the selected folder's own label but
// declines to carry it out. The implicit All Mail copy is best; when
// none is cached, All Mail is located by Message-ID.
func (b *Backend) gmailLabelSource(ctx context.Context, copies []mb.Copy, remove []mb.Mailbox, msg mb.Message) *mb.Copy {
	remPaths := map[string]bool{}
	for _, m := range remove {
		remPaths[m.Path] = true
	}
	var preferred, fallback *mb.Copy
	for i := range copies {
		c := &copies[i]
		if uid, ok := c.Ref.UID(); !ok || uid == 0 {
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if remPaths[c.Ref.Container] {
			continue
		}
		if msg.ImplicitContainer != "" && c.Ref.Container == msg.ImplicitContainer {
			return c
		}
		if preferred == nil {
			preferred = c
		}
	}
	if preferred != nil {
		return preferred
	}
	if msg.ImplicitContainer != "" && msg.MessageID != "" {
		if uid, err := b.connUp().FindUID(ctx, msg.ImplicitContainer, "Message-ID", msg.MessageID); err == nil && uid != 0 {
			return &mb.Copy{Ref: mb.NewRef(msg.ImplicitContainer, 0, uid)}
		}
	}
	return fallback
}

// Destroy permanently expunges every copy (FR-M.10). On Gmail it routes
// through trash first when a trash container is known, since expunge is
// permanent only there (FR-M.18); without one the generic path is the
// honest fallback.
func (b *Backend) Destroy(ctx context.Context, copies []mb.Ref, trash string) ([]mb.Ref, error) {
	conn := b.connUp()
	if trash != "" && conn.GmailExt() {
		touched, err := b.destroyGmail(ctx, copies, trash)
		if err == nil {
			return touched, nil
		}
		if !errors.Is(err, errNoTrashRouting) {
			return nil, err
		}
		b.log.Warn("imapdrv: gmail destroy without a usable trash mailbox, expunging in place")
	}
	byFolder := map[string][]uint32{}
	for _, r := range copies {
		uid, ok := r.UID()
		if !ok || uid == 0 {
			return nil, fmt.Errorf("imapdrv: no uid for folder %q yet", r.Container)
		}
		byFolder[r.Container] = append(byFolder[r.Container], uid)
	}
	for folder, uids := range byFolder {
		if err := conn.ExpungeUIDs(ctx, folder, uids); err != nil {
			return nil, mapErr(err)
		}
	}
	return copies, nil
}

var errNoTrashRouting = errors.New("imapdrv: no usable trash routing")

func (b *Backend) destroyGmail(ctx context.Context, copies []mb.Ref, trash string) ([]mb.Ref, error) {
	conn := b.connUp()
	var direct []uint32 // already in Trash: expunge in place
	var trashUIDs []uint32
	var touched []mb.Ref
	for _, r := range copies {
		uid, ok := r.UID()
		if !ok || uid == 0 {
			return nil, errNoTrashRouting
		}
		if r.Container == trash {
			direct = append(direct, uid)
			touched = append(touched, r)
			continue
		}
		res, err := conn.MoveUIDs(ctx, r.Container, trash, []uint32{uid})
		if err != nil {
			return nil, mapErr(err)
		}
		moved := res.DestUIDs[uid]
		if moved == 0 {
			moved = uid
		}
		trashUIDs = append(trashUIDs, moved)
		touched = append(touched, r, mb.NewRef(trash, res.DestUIDValidity, moved))
	}
	if len(direct) > 0 {
		if err := conn.ExpungeUIDs(ctx, trash, direct); err != nil {
			return nil, mapErr(err)
		}
	}
	if len(trashUIDs) > 0 {
		if err := conn.ExpungeUIDs(ctx, trash, trashUIDs); err != nil {
			return nil, mapErr(err)
		}
	}
	return touched, nil
}

// Append stores raw RFC 5322 bytes with the given neutral keywords,
// returning the copy's ref and the provider flags stored.
func (b *Backend) Append(ctx context.Context, container string, raw []byte, keywords []string, at *time.Time) (mb.Ref, []string, error) {
	flags := keyword.ToIMAP(truthMap(keywords))
	uid, uv, err := b.connUp().AppendMessage(ctx, container, raw, flags, at)
	if err != nil {
		return mb.Ref{}, nil, mapErr(err)
	}
	return mb.NewRef(container, uv, uid), flags, nil
}

// CreateMailbox creates a folder (FR-M.12).
func (b *Backend) CreateMailbox(ctx context.Context, path string) error {
	return mapErr(b.connUp().CreateMailbox(ctx, path))
}

// RenameMailbox renames/reparents a folder.
func (b *Backend) RenameMailbox(ctx context.Context, from, to string) error {
	return mapErr(b.connUp().RenameMailbox(ctx, from, to))
}

// DeleteMailbox deletes a folder.
func (b *Backend) DeleteMailbox(ctx context.Context, path string) error {
	return mapErr(b.connUp().DeleteMailbox(ctx, path))
}

// HierarchyDelim returns the hierarchy separator (PLAN §7.1).
func (b *Backend) HierarchyDelim(ctx context.Context) (rune, error) {
	d, err := b.connUp().HierarchyDelim(ctx)
	return d, mapErr(err)
}

// PendingNotifications drains the connection's coalesced change signal,
// used by the watch loop before re-arming.
func (b *Backend) PendingNotifications() { b.connUp().drainNotes() }

// --- helpers ---

func headerOf(container string, uv uint32, m HeaderMsg) mb.Header {
	return mb.Header{
		Ref:          mb.NewRef(container, uv, m.UID),
		Flags:        m.Flags,
		ModSeq:       m.ModSeq,
		InternalDate: m.InternalDate,
		Size:         m.Size,
		Envelope:     m.Envelope,
		Structure:    m.Structure,
		ThreadHint:   m.Thrid,
	}
}

func flagChangeOfRef(container string, uv uint32, fc FlagChange) mb.FlagChange {
	return mb.FlagChange{
		Ref:    mb.NewRef(container, uv, fc.UID),
		Flags:  fc.Flags,
		ModSeq: fc.ModSeq,
	}
}

func truthMap(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// mapErr maps driver errors onto the neutral mailbackend taxonomy.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrThrottled) {
		return fmt.Errorf("%w: %v", mb.ErrThrottled, err)
	}
	if IsAuthError(err) {
		return fmt.Errorf("%w: %v", mb.ErrAuth, err)
	}
	if ServerRejected(err) {
		return &mb.RejectedError{Text: RejectText(err), Err: err}
	}
	return err
}

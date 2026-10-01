package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file is the Gmail profile's write-path half (FR-S.10, FR-M.18):
// membership changes become label changes, All Mail membership is
// implicit, destroy routes through Trash, and the eventual-consistency
// grace window keeps the cache from tombstoning on lists that have not
// caught up with our own writes yet (FR-S.12).
//
// The read-path half (labels and thread ids in the header fetch,
// shared-UID dedupe) lives in backfill.go and imapdrv.

// gmailBatchPace is the pause between Gmail backfill batches (FR-S.12).
// The live gate calibrated it the hard way: ~300 headers/s sustained for
// twelve minutes put the account into Gmail's IMAP throttle, where
// further commands answer "OK [THROTTLED]" and writes are silently
// dropped. A batch every couple of seconds (~150 headers/s) stays under
// it; the backfill is resumable, so patience is free.
const gmailBatchPace = 5 * time.Second

// graceWindow is how long a folder's uid list may keep showing stale
// state after one of our own writes: Gmail rebuilds its per-folder
// indexes around label changes, and a message can vanish from a list it
// demonstrably still belongs to. Tombstones inside the window are
// deferred to the next pass — they are recoverable, while a wrong
// tombstone is a client-visible lie (FR-S.12).
const graceWindow = 60 * time.Second

// recordOwnWrite notes that we just changed the message identified by
// these (folder, uid) pairs, so the next pass's reconcile can spare them
// from tombstoning.
func (e *Engine) recordOwnWrite(folder string, uids []uint32) {
	if len(uids) == 0 {
		return
	}
	now := time.Now()
	e.ownMu.Lock()
	defer e.ownMu.Unlock()
	if e.ownWrites == nil {
		e.ownWrites = map[string]map[uint32]time.Time{}
	}
	byUID := e.ownWrites[folder]
	if byUID == nil {
		byUID = map[uint32]time.Time{}
		e.ownWrites[folder] = byUID
	}
	for _, uid := range uids {
		byUID[uid] = now
	}
	// Prune while we are here: anything past twice the window can no
	// longer matter.
	cutoff := now.Add(-2 * graceWindow)
	for f, m := range e.ownWrites {
		for uid, at := range m {
			if at.Before(cutoff) {
				delete(m, uid)
			}
		}
		if len(m) == 0 {
			delete(e.ownWrites, f)
		}
	}
}

// graceFilter drops uids we wrote to recently, so a stale folder list
// does not tombstone membership the server still holds (FR-S.12).
func (e *Engine) graceFilter(folder string, uids []uint32) []uint32 {
	e.ownMu.Lock()
	defer e.ownMu.Unlock()
	byUID := e.ownWrites[folder]
	if len(byUID) == 0 {
		return uids
	}
	cutoff := time.Now().Add(-graceWindow)
	out := uids[:0:0]
	for _, uid := range uids {
		if at, ours := byUID[uid]; ours && at.After(cutoff) {
			continue
		}
		out = append(out, uid)
	}
	return out
}

// gmLabelFor maps a mailbox to the Gmail label that implements it
// (FR-S.10). System folders have system labels; everything else's label
// is its mailbox path, because Gmail's user labels and IMAP folder names
// are the same strings. The implicit mailbox ([Gmail]/All Mail) is
// decided by the store's implicit flag before this is consulted — its
// membership is server-defined and no label write can express it.
func gmLabelFor(role, path string) string {
	switch role {
	case "inbox":
		return `\Inbox`
	case "sent":
		return `\Sent`
	case "drafts":
		// Gmail's X-GM-LABELS spells the draft label in flag form
		// ("\Draft"), even though SPECIAL-USE advertises "\Drafts"; a
		// write using the latter is accepted and silently ignored.
		return `\Draft`
	case "trash":
		return `\Trash`
	case "junk":
		return `\Spam`
	}
	// The two Gmail system folders SPECIAL-USE does not cover.
	switch leafName(path) {
	case "Starred":
		return `\Starred`
	case "Important":
		return `\Important`
	}
	return path
}

// leafName returns the part of an IMAP path after the last hierarchy
// separator (Gmail uses "/").
func leafName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '.' {
			return path[i+1:]
		}
	}
	return path
}

// changeMembershipGmail is the Gmail half of changeMembership (FR-S.10):
// label writes instead of copy storms. One pair of STORE commands covers
// every add and every remove, addressed through one copy whose folder and
// uid are used together. Gmail UIDs are per-folder — every label is its
// own IMAP mailbox with its own uid sequence and uidvalidity — so the uid
// is only valid in the folder it was read from. The write also prefers a
// copy the patch is not removing: Gmail answers OK but silently declines
// to remove the selected folder's own label (removing \Inbox archives
// only when issued from another copy, All Mail preferred).
func (e *Engine) changeMembershipGmail(ctx context.Context, emailID string, copies []store.Copy, addSet []string, remSet []string) ([]store.MembershipAdd, error) {
	addLabels, _, err := e.labelsFor(ctx, addSet, false)
	if err != nil {
		return nil, err
	}
	removeLabels, _, err := e.labelsFor(ctx, remSet, true)
	if err != nil {
		return nil, err
	}
	// Nothing server-side to do: every add is implicit and every remove
	// was refused above.
	if len(addLabels) == 0 && len(removeLabels) == 0 {
		return nil, nil
	}

	src := e.gmailLabelSource(ctx, emailID, copies, remSet)
	if src == nil {
		return nil, fmt.Errorf("%w: no folder holds a known uid for this message", jmapapi.ErrNoLocation)
	}

	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		e.log.Debug("sync: gmail label write",
			"email", emailID, "folder", src.Folder, "uid", src.UID,
			"add", addLabels, "remove", removeLabels)
		if err := conn.StoreGmLabels(ctx, src.Folder, []uint32{src.UID}, addLabels, removeLabels); err != nil {
			return err
		}
		e.recordOwnWrite(src.Folder, []uint32{src.UID})
		for _, c := range copies {
			if c.UID != 0 {
				e.recordOwnWrite(c.Folder, []uint32{c.UID})
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Every add is a membership the server now holds — a label write
	// accepted it, or it is implicit — so it commits locally at once.
	// That is what keeps a draft moved into Sent from being tombstoned
	// when its Drafts membership goes away. A destination already cached
	// (the implicit All Mail) supplies its own uid; otherwise the uid is
	// unknown — Gmail assigns one on the label write — so the membership
	// commits without a mapping and the next sync pass fills it in.
	adds := make([]store.MembershipAdd, 0, len(addSet))
	for _, id := range addSet {
		var uid, uv uint32
		for i := range copies {
			if copies[i].MailboxID == id {
				uid, uv = copies[i].UID, copies[i].UIDValidity
				break
			}
		}
		adds = append(adds, store.MembershipAdd{
			MailboxID: id, UID: uid, UIDValidity: uv,
		})
	}
	return adds, nil
}

// gmailLabelSource chooses the copy whose folder and uid a Gmail label
// write is issued from. It prefers a folder the patch is not removing,
// because Gmail accepts a command that would remove the selected
// folder's own label but declines to carry it out (removing \Inbox
// archives only when issued from another copy). The implicit All Mail
// copy is the best source: every message lives there and it is never in
// a removable set. When no cached copy is usable — a fresh message the
// All Mail pass has not linked yet — All Mail is located by Message-ID.
// Only a message with no other copy at all falls back to a removed one,
// the single command that can still work (filing a draft into Sent).
func (e *Engine) gmailLabelSource(ctx context.Context, emailID string, copies []store.Copy, remSet []string) *store.Copy {
	allID, err := e.st.ImplicitMailboxID(ctx, e.cfg.Account)
	if err != nil {
		allID = ""
	}
	var preferred, fallback *store.Copy
	for i := range copies {
		c := &copies[i]
		if c.UID == 0 {
			continue
		}
		if fallback == nil {
			fallback = c
		}
		if contains(remSet, c.MailboxID) {
			continue
		}
		if allID != "" && c.MailboxID == allID {
			return c
		}
		if preferred == nil {
			preferred = c
		}
	}
	if preferred != nil {
		return preferred
	}
	if allID != "" {
		if c := e.gmailAllMailCopy(ctx, emailID, allID); c != nil {
			return c
		}
	}
	return fallback
}

// gmailAllMailCopy addresses an email in All Mail by searching for its
// Message-ID. Gmail's All Mail holds every message and its per-folder uid
// differs from every cached copy's, so the search is the only way to name
// a message the cache has seen only through a folder being removed.
func (e *Engine) gmailAllMailCopy(ctx context.Context, emailID, allID string) *store.Copy {
	msgid, err := e.st.EmailMessageID(ctx, e.cfg.Account, emailID)
	if err != nil || msgid == "" {
		return nil
	}
	path, err := e.st.MailboxPath(ctx, e.cfg.Account, allID)
	if err != nil {
		return nil
	}
	var uid uint32
	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		var e2 error
		uid, e2 = conn.FindUID(ctx, path, "Message-ID", msgid)
		return e2
	}); err != nil {
		e.log.Debug("sync: all-mail uid lookup failed", "email", emailID, "err", err)
		return nil
	}
	if uid == 0 {
		return nil
	}
	return &store.Copy{MailboxID: allID, Folder: path, UID: uid}
}

// gmailDraftCopy returns the drafts-folder copy of an email, or nil. Gmail
// silently drops an SMTP submission whose Message-ID already exists in
// the mailbox, and the bridge APPENDed the message as this draft, so the
// send path removes it before submitting.
func (e *Engine) gmailDraftCopy(ctx context.Context, account, emailID string) (*store.Copy, error) {
	draftID, err := e.st.MailboxIDByRole(ctx, account, "drafts")
	if err != nil {
		return nil, err
	}
	if draftID == "" {
		return nil, nil
	}
	copies, err := e.st.EmailCopies(ctx, account, emailID)
	if err != nil {
		return nil, err
	}
	for i := range copies {
		if copies[i].MailboxID == draftID && copies[i].UID != 0 {
			return &copies[i], nil
		}
	}
	return nil, nil
}

// restoreGmailDraft re-APPENDs the draft the send path expunged when the
// send then failed, so an error never loses the client's draft. The cache
// reconciles the fresh uid on the next sync pass.
func (e *Engine) restoreGmailDraft(ctx context.Context, account, emailID string, c *store.Copy, raw []byte) {
	if c == nil {
		return
	}
	kw, err := e.st.EmailKeywords(ctx, account, emailID)
	if err != nil {
		e.log.Warn("sync: draft not restored", "email", emailID, "err", err)
		return
	}
	flags := keyword.ToIMAP(kw)
	if !contains(flags, `\Draft`) {
		flags = append(flags, `\Draft`)
	}
	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		_, _, err := conn.AppendMessage(ctx, c.Folder, raw, flags, nil)
		return err
	}); err != nil {
		e.log.Warn("sync: draft not restored",
			"email", emailID, "folder", c.Folder, "err", err)
	}
}

// labelsFor resolves mailbox ids to Gmail labels. forRemoval marks the
// implicit-mailbox check: removing from All Mail is not expressible and
// fails the patch; adding to it is a no-op the caller commits locally.
func (e *Engine) labelsFor(ctx context.Context, ids []string, forRemoval bool) (labels []string, implicit map[string]bool, err error) {
	implicit = map[string]bool{}
	if len(ids) == 0 {
		return nil, implicit, nil
	}
	mbs, _, notFound, err := e.st.MailboxesByID(ctx, e.cfg.Account, ids)
	if err != nil {
		return nil, nil, err
	}
	if len(notFound) > 0 {
		return nil, nil, jmapapi.ErrUnknownMailbox
	}
	// The implicit mailbox is the store's implicit flag (the \All
	// attribute at discovery), not the archive role: a plain folder
	// named "Archive" shares the role but is a normal label mailbox.
	allID, err := e.st.ImplicitMailboxID(ctx, e.cfg.Account)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]*jmapapi.Mailbox{}
	for _, mb := range mbs {
		byID[mb.ID] = mb
	}
	for _, id := range ids {
		mb, ok := byID[id]
		if !ok {
			return nil, nil, jmapapi.ErrUnknownMailbox
		}
		if id == allID {
			if forRemoval {
				return nil, nil, &implicitRemoveError{path: mb.Path}
			}
			implicit[id] = true
			continue
		}
		labels = append(labels, gmLabelFor(mb.Role, mb.Path))
	}
	return labels, implicit, nil
}

// implicitRemoveError is the refusal for a membership removal the Gmail
// server model cannot express: All Mail holds everything, so "remove
// from All Mail" has no label write.
type implicitRemoveError struct{ path string }

func (e *implicitRemoveError) Error() string {
	return "membership in " + e.path + " is managed by the server and cannot be removed"
}

// destroyEmailsGmail makes destroy permanent on Gmail (FR-M.18): copies
// outside Trash/Spam are moved to Trash (Gmail's MOVE drops the other
// labels), then everything is expunged inside Trash, where expunge means
// permanent. Without a trash-role mailbox the only honest fallback is
// the generic expunge-everywhere path, which the caller runs.
func (e *Engine) destroyEmailsGmail(ctx context.Context, copies []store.Copy) error {
	trashID, err := e.st.MailboxIDByRole(ctx, e.cfg.Account, "trash")
	if err != nil {
		return err
	}
	if trashID == "" {
		return errNoTrashRole
	}
	trashPath, err := e.st.MailboxPath(ctx, e.cfg.Account, trashID)
	if err != nil {
		return err
	}

	var direct []uint32 // already in Trash or Spam: expunge in place
	var movable []store.Copy
	var trashUIDs []uint32
	for _, c := range copies {
		if c.UID == 0 {
			return fmt.Errorf("%w: no folder holds a known uid for this message", jmapapi.ErrNoLocation)
		}
		if c.MailboxID == trashID {
			direct = append(direct, c.UID)
			continue
		}
		movable = append(movable, c)
	}

	if len(movable) > 0 {
		for _, c := range movable {
			var res imapdrv.CopyResult
			if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
				var err error
				res, err = conn.MoveUIDs(ctx, c.Folder, trashPath, []uint32{c.UID})
				return err
			}); err != nil {
				return err
			}
			// Gmail returns the destination uid in COPYUID; a moved
			// message gets a new uid in Trash because uids are
			// per-folder. Without a COPYUID mapping the source uid is
			// not valid in Trash, so it is only a best-effort fallback
			// for servers that omit it.
			uid := res.DestUIDs[c.UID]
			if uid == 0 {
				uid = c.UID
			}
			trashUIDs = append(trashUIDs, uid)
			e.recordOwnWrite(c.Folder, []uint32{c.UID})
			e.recordOwnWrite(trashPath, []uint32{uid})
		}
	}
	if len(direct) > 0 || len(trashUIDs) > 0 {
		if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
			if len(direct) > 0 {
				if err := conn.ExpungeUIDs(ctx, trashPath, direct); err != nil {
					return err
				}
			}
			if len(trashUIDs) > 0 {
				return conn.ExpungeUIDs(ctx, trashPath, trashUIDs)
			}
			return nil
		}); err != nil {
			return err
		}
		e.recordOwnWrite(trashPath, append(append([]uint32{}, direct...), trashUIDs...))
	}
	return nil
}

// errNoTrashRole marks the Gmail destroy fallback.
var errNoTrashRole = errors.New("gmail: account has no trash mailbox; falling back to expunge in place")

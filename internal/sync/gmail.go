package sync

import (
	"context"
	"time"

	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file keeps the backend-neutral half of the Gmail profile: the
// eventual-consistency grace window (FR-S.12) and the draft handle used
// by the send path. The provider-specific label/membership strategy
// (labels, label writes, All Mail routing, trash destroy) lives in the
// imapdrv adapter behind mailbackend.Capabilities.Labels (M8).

// gmailBatchPace is the pause between Gmail backfill batches (FR-S.12).
// The live gate calibrated it the hard way: ~300 headers/s sustained for
// twelve minutes put the account into Gmail's IMAP throttle. A batch
// every couple of seconds stays under it; the backfill is resumable, so
// patience is free.
const gmailBatchPace = 5 * time.Second

// graceWindow is how long a folder's uid list may keep showing stale
// state after one of our own writes: Gmail rebuilds its per-folder
// indexes around label changes, and a message can vanish from a list it
// demonstrably still belongs to. Tombstones inside the window are
// deferred to the next pass — they are recoverable, while a wrong
// tombstone is a client-visible lie (FR-S.12).
const graceWindow = 60 * time.Second

// recordOwnUIDs is the per-folder form of recordOwnWrite, for callers
// that hold plain uids.
func (e *Engine) recordOwnUIDs(folder string, uids []uint32) {
	if len(uids) == 0 {
		return
	}
	refs := make([]mb.Ref, 0, len(uids))
	for _, u := range uids {
		refs = append(refs, mb.NewRef(folder, 0, u))
	}
	e.recordOwnWrite(refs)
}

// recordOwnWrite notes that we just changed the message identified by
// these (container, uid) refs, so the next pass's reconcile can spare
// them from tombstoning.
func (e *Engine) recordOwnWrite(refs []mb.Ref) {
	if len(refs) == 0 {
		return
	}
	now := time.Now()
	e.ownMu.Lock()
	defer e.ownMu.Unlock()
	if e.ownWrites == nil {
		e.ownWrites = map[string]map[uint32]time.Time{}
	}
	for _, r := range refs {
		uid, ok := r.UID()
		if !ok || uid == 0 {
			continue
		}
		byUID := e.ownWrites[r.Container]
		if byUID == nil {
			byUID = map[uint32]time.Time{}
			e.ownWrites[r.Container] = byUID
		}
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

// gmailDraftCopy returns the drafts-folder copy of an email, or nil.
// Gmail silently drops an SMTP submission whose Message-ID already
// exists in the mailbox, and the bridge APPENDed the message as this
// draft, so the send path removes it before submitting.
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
	kws := boolKeys(kw)
	if !contains(kws, "$draft") {
		kws = append(kws, "$draft")
	}
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		_, _, err := b.Append(ctx, c.Folder, raw, kws, nil)
		return err
	}); err != nil {
		e.log.Warn("sync: draft not restored",
			"email", emailID, "folder", c.Folder, "err", err)
	}
}

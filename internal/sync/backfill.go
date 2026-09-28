package sync

import (
	"context"
	"fmt"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// discoverLocked lists folders with STATUS and reconciles them into the
// store (FR-S.1). It returns each folder's fresh status for the
// incremental phase and is also where UIDVALIDITY resets surface
// (FR-S.6): SyncFolders names the folders it reset and their cursors
// start over.
func (e *Engine) discoverLocked(ctx context.Context) (map[string]imapdrv.FolderStatus, error) {
	conn := e.work
	list, err := conn.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	folders := make([]store.Folder, 0, len(list))
	statuses := make(map[string]imapdrv.FolderStatus, len(list))
	idleCandidate := ""
	for _, f := range list {
		st, err := conn.Status(ctx, f.Name)
		if err != nil {
			return nil, fmt.Errorf("status %q: %w", f.Name, err)
		}
		statuses[f.Name] = imapdrv.FolderStatus{
			UIDValidity:   st.UIDValidity,
			UIDNext:       st.UIDNext,
			HighestModSeq: st.HighestModSeq,
		}
		folders = append(folders, store.Folder{
			Name: f.Name, Delim: f.Delim, Role: f.Role, NoSelect: f.NoSelect,
			UIDValidity: st.UIDValidity, UIDNext: st.UIDNext, HighestModSeq: st.HighestModSeq,
		})
		if f.Role == "inbox" && !f.NoSelect {
			idleCandidate = f.Name
		}
	}
	reset, err := e.st.SyncFolders(ctx, e.cfg.Account, folders)
	if err != nil {
		return nil, err
	}
	for _, name := range reset {
		if err := e.st.ClearFolderSync(ctx, e.cfg.Account, name); err != nil {
			return nil, err
		}
		e.log.Info("sync: uidvalidity changed, folder reset", "folder", name)
	}
	names := make([]string, 0, len(folders))
	for _, f := range folders {
		names = append(names, f.Name)
	}
	e.folders = names
	if idleCandidate != "" {
		e.setCurrentIdleFolder(idleCandidate)
	}
	return statuses, nil
}

// syncFolderLocked runs one folder's pass: backfill while the cursor
// says so, incremental afterwards.
func (e *Engine) syncFolderLocked(ctx context.Context, folder string, status imapdrv.FolderStatus) error {
	fs, err := e.st.FolderSync(ctx, e.cfg.Account, folder)
	if err != nil {
		return err
	}
	// Defence in depth: a UIDVALIDITY the store reset elsewhere (or a
	// stale cursor row) must never anchor a pass (FR-S.6).
	if fs.UIDValidity != 0 && fs.UIDValidity != status.UIDValidity {
		if err := e.st.ResetFolder(ctx, e.cfg.Account, folder, status.UIDValidity); err != nil {
			return err
		}
		fs = store.FolderSync{}
		if err := e.st.ClearFolderSync(ctx, e.cfg.Account, folder); err != nil {
			return err
		}
	}
	if !fs.BackfillDone {
		return e.backfillLocked(ctx, folder, status, fs)
	}
	return e.incrementalLocked(ctx, folder, status, fs)
}

// backfillLocked ingests the folder's headers in resumable batches
// (FR-S.3): one EXAMINE, then UID-range header fetches up to the
// snapshot's uidnext — never bodies, never more than batch_size uids
// per round trip (FR-S.12).
func (e *Engine) backfillLocked(ctx context.Context, folder string, status imapdrv.FolderStatus, fs store.FolderSync) error {
	if _, err := e.work.Examine(ctx, folder, nil); err != nil {
		return err
	}
	batch := e.cfg.BatchSize
	for cursor := fs.BackfillUID + 1; uint64(cursor) < status.UIDNext; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := uint64(cursor) + uint64(batch) - 1
		if end >= status.UIDNext {
			end = status.UIDNext - 1
		}
		uids := make([]uint32, 0, end-uint64(cursor)+1)
		for u := uint64(cursor); u <= end; u++ {
			uids = append(uids, uint32(u))
		}
		msgs, err := e.work.FetchHeaders(ctx, uids)
		if err != nil {
			return fmt.Errorf("backfill %q [%d..%d]: %w", folder, cursor, end, err)
		}
		if err := e.ingest(ctx, folder, status.UIDValidity, msgs); err != nil {
			return err
		}
		fs.UIDValidity = status.UIDValidity
		fs.UIDNext = status.UIDNext
		fs.BackfillUID = uint32(end)
		if err := e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs); err != nil {
			return err
		}
		cursor = uint32(end) + 1
		e.log.Debug("sync: backfill batch",
			"folder", folder, "through", end, "of", status.UIDNext)
	}
	fs.UIDValidity = status.UIDValidity
	fs.UIDNext = status.UIDNext
	fs.HighestModSeq = status.HighestModSeq
	fs.BackfillDone = true
	if err := e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs); err != nil {
		return err
	}
	e.log.Info("sync: backfill complete",
		"folder", folder, "messages", fs.BackfillUID, "tier", e.work.Tier().String())
	return nil
}

// incrementalLocked is the tier ladder of FR-S.5: QRESYNC replays
// vanished+flag deltas from the select itself, CONDSTORE fetches
// CHANGEDSINCE and diffs uids, baseline refetches uid+flags wholesale.
// New messages are found the same way at every tier: the uidnext range.
func (e *Engine) incrementalLocked(ctx context.Context, folder string, status imapdrv.FolderStatus, fs store.FolderSync) error {
	var anchor *imapdrv.Anchor
	if e.work.Tier() == imapdrv.TierQResync && fs.UIDValidity != 0 {
		anchor = &imapdrv.Anchor{UIDValidity: fs.UIDValidity, ModSeq: fs.HighestModSeq}
	}
	sel, err := e.work.Examine(ctx, folder, anchor)
	if err != nil {
		return err
	}
	if sel.Status.UIDValidity != fs.UIDValidity {
		// Changed between STATUS and EXAMINE: reset and let the next
		// pass backfill from scratch (FR-S.6).
		if err := e.st.ResetFolder(ctx, e.cfg.Account, folder, sel.Status.UIDValidity); err != nil {
			return err
		}
		if err := e.st.ClearFolderSync(ctx, e.cfg.Account, folder); err != nil {
			return err
		}
		e.log.Info("sync: uidvalidity changed mid-pass", "folder", folder)
		return nil
	}
	if sel.ResyncRejected {
		// The anchor was ignored: every cached uid may be stale.
		if err := e.st.ResetFolder(ctx, e.cfg.Account, folder, sel.Status.UIDValidity); err != nil {
			return err
		}
		if err := e.st.ClearFolderSync(ctx, e.cfg.Account, folder); err != nil {
			return err
		}
		e.log.Warn("sync: resync rejected, rescanning", "folder", folder)
		return nil
	}

	// 1. Expunges QRESYNC already told us about.
	if len(sel.Vanished) > 0 {
		if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, sel.Status.UIDValidity, sel.Vanished); err != nil {
			return err
		}
	}

	// 2. New messages: everything at or beyond the stored uidnext.
	newCount := 0
	if sel.Status.UIDNext > fs.UIDNext {
		var uids []uint32
		for u := fs.UIDNext; u < sel.Status.UIDNext; u++ {
			uids = append(uids, uint32(u))
		}
		for start := 0; start < len(uids); start += e.cfg.BatchSize {
			end := min(start+e.cfg.BatchSize, len(uids))
			msgs, err := e.work.FetchHeaders(ctx, uids[start:end])
			if err != nil {
				return fmt.Errorf("new uids %q: %w", folder, err)
			}
			if err := e.ingest(ctx, folder, sel.Status.UIDValidity, msgs); err != nil {
				return err
			}
			newCount += len(msgs)
		}
	}

	// 3. Flags (and expunges where the tier cannot name them).
	switch e.work.Tier() {
	case imapdrv.TierQResync:
		if err := e.applyFlagChanges(ctx, folder, sel.Status.UIDValidity, sel.FlagChanges); err != nil {
			return err
		}
	case imapdrv.TierCondStore:
		if fs.HighestModSeq > 0 && !sel.NoModSeq {
			changed, vanished, err := e.work.ChangesSince(ctx, fs.HighestModSeq)
			if err != nil {
				return err
			}
			if len(vanished) > 0 {
				if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, sel.Status.UIDValidity, vanished); err != nil {
					return err
				}
			}
			if err := e.applyFlagChanges(ctx, folder, sel.Status.UIDValidity, changed); err != nil {
				return err
			}
		}
		// CONDSTORE names no expunges: the uid list is the truth.
		if err := e.reconcileUIDs(ctx, folder, sel.Status.UIDValidity); err != nil {
			return err
		}
	default: // TierBaseline
		all, err := e.work.AllFlags(ctx)
		if err != nil {
			return err
		}
		if err := e.reconcileUIDsFrom(ctx, folder, sel.Status.UIDValidity, all); err != nil {
			return err
		}
	}

	fs.UIDValidity = sel.Status.UIDValidity
	fs.UIDNext = sel.Status.UIDNext
	if sel.Status.HighestModSeq > 0 {
		fs.HighestModSeq = sel.Status.HighestModSeq
	}
	return e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs)
}

// reconcileUIDs diffs the server's uid list against the store's and
// tombstones the difference (FR-S.5: tombstones, never silent row
// deletions).
func (e *Engine) reconcileUIDs(ctx context.Context, folder string, uv uint32) error {
	server, err := e.work.UIDs(ctx)
	if err != nil {
		return err
	}
	known, err := e.st.FolderUIDs(ctx, e.cfg.Account, folder)
	if err != nil {
		return err
	}
	gone := diffGone(known, server)
	if len(gone) > 0 {
		return e.st.RemoveUIDs(ctx, e.cfg.Account, folder, uv, gone)
	}
	return nil
}

// reconcileUIDsFrom does the same with a uid set that arrived attached
// to a flag scan (baseline tier fetches both at once).
func (e *Engine) reconcileUIDsFrom(ctx context.Context, folder string, uv uint32, all []imapdrv.FlagChange) error {
	server := make([]uint32, 0, len(all))
	byUID := make(map[uint32][]string, len(all))
	for _, fc := range all {
		server = append(server, fc.UID)
		byUID[fc.UID] = fc.Flags
	}
	known, err := e.st.FolderUIDs(ctx, e.cfg.Account, folder)
	if err != nil {
		return err
	}
	gone := diffGone(known, server)
	if len(gone) > 0 {
		if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, uv, gone); err != nil {
			return err
		}
	}
	updates := make([]store.FlagUpdate, 0, len(byUID))
	for uid, flags := range byUID {
		updates = append(updates, store.FlagUpdate{UID: uid, Flags: flags})
	}
	return e.st.UpdateFlagsBulk(ctx, e.cfg.Account, folder, uv, updates)
}

// applyFlagChanges folds a tier's flag deltas into the store; a uid the
// store does not know yet is a new message (CONDSTORE reports new
// messages as flag traffic), so its headers are fetched right there.
func (e *Engine) applyFlagChanges(ctx context.Context, folder string, uv uint32, changes []imapdrv.FlagChange) error {
	var unknown []uint32
	for _, ch := range changes {
		applied, err := e.st.UpdateFlags(ctx, e.cfg.Account, folder, uv, ch.UID, ch.Flags)
		if err != nil {
			return err
		}
		if !applied {
			unknown = append(unknown, ch.UID)
		}
	}
	for start := 0; start < len(unknown); start += e.cfg.BatchSize {
		end := min(start+e.cfg.BatchSize, len(unknown))
		msgs, err := e.work.FetchHeaders(ctx, unknown[start:end])
		if err != nil {
			return fmt.Errorf("flagged-new uids %q: %w", folder, err)
		}
		if err := e.ingest(ctx, folder, uv, msgs); err != nil {
			return err
		}
	}
	return nil
}

// ingest converts driver header records into store records and commits
// them (FR-S.3).
func (e *Engine) ingest(ctx context.Context, folder string, uv uint32, msgs []imapdrv.HeaderMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	recs := make([]store.MessageRec, 0, len(msgs))
	for _, m := range msgs {
		rec := convert.Summary(m.Envelope, m.Structure, m.InternalDate, m.Size)
		rec.UID = m.UID
		rec.UIDValidity = uv
		rec.Flags = m.Flags
		recs = append(recs, rec)
	}
	return e.st.PutMessages(ctx, e.cfg.Account, folder, recs)
}

// diffGone returns known uids the server no longer lists.
func diffGone(known, server []uint32) []uint32 {
	have := make(map[uint32]bool, len(server))
	for _, u := range server {
		have[u] = true
	}
	var gone []uint32
	for _, u := range known {
		if !have[u] {
			gone = append(gone, u)
		}
	}
	return gone
}

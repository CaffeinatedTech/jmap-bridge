package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// discoverLocked is the work pass's discovery (FR-S.1).
func (e *Engine) discoverLocked(ctx context.Context) (map[string]imapdrv.FolderStatus, error) {
	return e.discoverWith(ctx, e.work, true)
}

// discoverWith lists folders with STATUS on conn and reconciles them
// into the store. It returns each folder's fresh status and is also
// where UIDVALIDITY resets surface (FR-S.6): SyncFolders names the
// folders it reset and their cursors start over.
//
// record marks whether this call may update the engine's folder list —
// the write path refreshes roles after Mailbox/set from its own
// connection (FR-M.12) without touching state the work pass owns under
// workMu.
func (e *Engine) discoverWith(ctx context.Context, conn *imapdrv.Conn, record bool) (map[string]imapdrv.FolderStatus, error) {
	list, err := conn.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	folders := make([]store.Folder, 0, len(list))
	statuses := make(map[string]imapdrv.FolderStatus, len(list))
	idleCandidate := ""
	for _, f := range list {
		if f.NoSelect {
			// A hierarchy container the server refuses to select
			// (Gmail's "[Gmail]"): it answers STATUS with NO
			// [NONEXISTENT], so it gets no status and never enters the
			// pass order or IDLE. The mailbox row still exists — the
			// server does list it — with zero sync state.
			folders = append(folders, store.Folder{
				Name: f.Name, Delim: f.Delim, Role: f.Role, NoSelect: f.NoSelect,
			})
			continue
		}
		st, err := conn.Status(ctx, f.Name)
		if err != nil {
			return nil, fmt.Errorf("status %q: %w", f.Name, err)
		}
		statuses[f.Name] = imapdrv.FolderStatus{
			UIDValidity:   st.UIDValidity,
			UIDNext:       st.UIDNext,
			HighestModSeq: st.HighestModSeq,
			Messages:      st.Messages,
		}
		folders = append(folders, store.Folder{
			Name: f.Name, Delim: f.Delim, Role: f.Role, NoSelect: f.NoSelect,
			Implicit:    f.AllMail,
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
	if record {
		// The pass order covers selectable folders only; a NoSelect
		// container has no status entry, so a pass can never target it.
		names := make([]string, 0, len(statuses))
		for name := range statuses {
			names = append(names, name)
		}
		e.folders = names
	}
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
// (FR-S.3): one EXAMINE, then fetches up to the snapshot's end — never
// bodies, never more than batch_size messages per round trip (FR-S.12).
//
// On Gmail the walk is by SEQUENCE number up to EXISTS (FR-S.10):
// Gmail's uid spaces are sparse — a 73k-uidnext folder can hold 31
// messages — and sweeping expunged uid ranges trips Gmail's response
// slow-walking (observed live: 22 s per batch on [Gmail]/Spam, then a
// silent stall). Sequence ranges touch only live messages, and each
// response carries the uid the records key on. fs.BackfillUID holds the
// last sequence fetched for Gmail folders (uid cursor elsewhere).
func (e *Engine) backfillLocked(ctx context.Context, folder string, status imapdrv.FolderStatus, fs store.FolderSync) error {
	if _, err := e.work.Examine(ctx, folder, nil); err != nil {
		return err
	}
	batch := e.cfg.BatchSize
	if e.work.GmailExt() {
		return e.backfillGmailLocked(ctx, folder, status, fs, batch)
	}
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
		// Gmail (FR-S.10): a uid the account already knows through
		// another folder needs no header fetch — Gmail UIDs are global
		// and flags are per-message, so only the membership and the
		// folder mapping are missing. One shared-uid lookup replaces a
		// full ENVELOPE round trip per already-seen message, which is
		// most of them once All Mail has been backfilled.
		if e.work.GmailExt() {
			known, err := e.st.KnownSharedUIDs(ctx, e.cfg.Account, status.UIDValidity, uids)
			if err != nil {
				return err
			}
			var need []uint32
			for _, u := range uids {
				if _, ok := known[u]; !ok {
					need = append(need, u)
				}
			}
			if len(need) > 0 {
				msgs, err := e.work.FetchHeaders(ctx, need)
				if err != nil {
					return fmt.Errorf("backfill %q [%d..%d]: %w", folder, cursor, end, err)
				}
				if err := e.ingest(ctx, folder, status.UIDValidity, msgs); err != nil {
					return err
				}
			}
			if len(known) > 0 {
				knownList := make([]uint32, 0, len(known))
				for u := range known {
					knownList = append(knownList, u)
				}
				if err := e.st.LinkSharedUIDs(ctx, e.cfg.Account, folder, status.UIDValidity, knownList); err != nil {
					return err
				}
			}
		} else {
			msgs, err := e.work.FetchHeaders(ctx, uids)
			if err != nil {
				return fmt.Errorf("backfill %q [%d..%d]: %w", folder, cursor, end, err)
			}
			if err := e.ingest(ctx, folder, status.UIDValidity, msgs); err != nil {
				return err
			}
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

	// 1. Expunges QRESYNC already told us about. The grace window
	// spares uids our own writes touched (FR-S.12): a list that has not
	// caught up must not tombstone membership the server still holds.
	if len(sel.Vanished) > 0 {
		vanished := e.graceFilter(folder, sel.Vanished)
		if len(vanished) > 0 {
			if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, sel.Status.UIDValidity, vanished); err != nil {
				return err
			}
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
			if vanished = e.graceFilter(folder, vanished); len(vanished) > 0 {
				if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, sel.Status.UIDValidity, vanished); err != nil {
					return err
				}
			}
			if err := e.applyFlagChanges(ctx, folder, sel.Status.UIDValidity, changed); err != nil {
				return err
			}
		}
		// CONDSTORE names no expunges: the uid list is the truth,
		// swept only when the server's count disagrees with ours.
		if err := e.reconcileUIDs(ctx, folder, sel.Status.UIDValidity, sel.Status.Messages); err != nil {
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
// deletions). The sweep is gated on the server's own count first: when
// EXAMINE's EXISTS equals the store's live count for the folder, no
// expunge can have gone unnoticed — an expunge plus an arrival in the
// same window is caught anyway, because the arrival is ingested before
// this check and tips the counts. On Gmail's sparse folders the sweep
// is a five-figure response, so skipping it when counts agree is what
// keeps a pass light enough never to trip the provider's throttle
// (FR-S.12). A count mismatch sweeps as before.
func (e *Engine) reconcileUIDs(ctx context.Context, folder string, uv uint32, exists uint32) error {
	known, err := e.st.FolderUIDs(ctx, e.cfg.Account, folder)
	if err != nil {
		return err
	}
	if exists == uint32(len(known)) {
		return nil
	}
	server, err := e.work.UIDs(ctx)
	if err != nil {
		return err
	}
	gone := e.graceFilter(folder, diffGone(known, server))
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
	gone := e.graceFilter(folder, diffGone(known, server))
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
// them (FR-S.3). On X-GM-EXT-1 servers the records carry the shared-UID
// hint, so a message first seen through another folder dedupes instead
// of becoming a second JMAP object (FR-S.10).
func (e *Engine) ingest(ctx context.Context, folder string, uv uint32, msgs []imapdrv.HeaderMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	shared := e.work.GmailExt()
	recs := make([]store.MessageRec, 0, len(msgs))
	for _, m := range msgs {
		rec := convert.Summary(m.Envelope, m.Structure, m.InternalDate, m.Size)
		rec.UID = m.UID
		rec.UIDValidity = uv
		rec.Flags = m.Flags
		rec.SharedUIDs = shared
		rec.GmThrid = m.Thrid
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

// backfillGmailLocked is the sequence-walking Gmail half of backfill:
// batches of live messages by sequence number, uid-keyed records, one
// save per batch so a restart resumes where the store says it stopped
// (FR-S.3), and a short inter-batch pause — 500-header round trips at
// full speed are how a provider starts slow-walking you (FR-S.12).
func (e *Engine) backfillGmailLocked(ctx context.Context, folder string, status imapdrv.FolderStatus, fs store.FolderSync, batch int) error {
	for cursor := fs.BackfillUID + 1; uint64(cursor) <= uint64(status.Messages); {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := uint64(cursor) + uint64(batch) - 1
		if end > uint64(status.Messages) {
			end = uint64(status.Messages)
		}
		seqs := make([]uint32, 0, end-uint64(cursor)+1)
		for s := uint64(cursor); s <= end; s++ {
			seqs = append(seqs, uint32(s))
		}
		msgs, err := e.work.FetchHeadersSeq(ctx, seqs)
		if err != nil {
			return fmt.Errorf("backfill %q seq [%d..%d]: %w", folder, cursor, end, err)
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
			"folder", folder, "through", end, "of", status.Messages)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gmailBatchPace):
		}
	}
	fs.UIDValidity = status.UIDValidity
	fs.UIDNext = status.UIDNext
	fs.HighestModSeq = status.HighestModSeq
	fs.BackfillDone = true
	if err := e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs); err != nil {
		return err
	}
	e.log.Info("sync: backfill complete",
		"folder", folder, "messages", status.Messages, "tier", e.work.Tier().String())
	return nil
}

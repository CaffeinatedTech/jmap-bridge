package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file is the read half of the sync engine: discovery, resumable
// header backfill and tier-aware incremental passes. All provider
// strategy (IMAP tiers, per-folder cursors, Gmail label reconcile) lives
// behind mailbackend.Backend (M8); the engine orchestrates passes and
// commits what the backend reported.

// discoverLocked is the work pass's discovery (FR-S.1).
func (e *Engine) discoverLocked(ctx context.Context) (map[string]mb.FolderStatus, error) {
	return e.discoverWith(ctx, e.work, true)
}

// discoverWith lists folders with STATUS on backend and reconciles them
// into the store. It returns each folder's fresh status and is also
// where UIDVALIDITY resets surface (FR-S.6): SyncFolders names the
// folders it reset and their cursors start over.
//
// record marks whether this call may update the engine's folder list —
// the write path refreshes roles after Mailbox/set from its own
// connection (FR-M.12) without touching state the work pass owns under
// workMu.
func (e *Engine) discoverWith(ctx context.Context, backend mb.Backend, record bool) (map[string]mb.FolderStatus, error) {
	list, err := backend.Folders(ctx)
	if err != nil {
		return nil, err
	}
	folders := make([]store.Folder, 0, len(list))
	statuses := make(map[string]mb.FolderStatus, len(list))
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
		st, err := backend.FolderStatus(ctx, f.Container)
		if err != nil {
			return nil, fmt.Errorf("status %q: %w", f.Name, err)
		}
		statuses[f.Name] = st
		folders = append(folders, store.Folder{
			Name: f.Name, Delim: f.Delim, Role: f.Role, NoSelect: f.NoSelect,
			NativeID:    f.NativeID,
			Implicit:    f.Implicit,
			UIDValidity: st.Version, UIDNext: st.Next, HighestModSeq: st.ModSeq,
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
func (e *Engine) syncFolderLocked(ctx context.Context, folder string, status mb.FolderStatus) error {
	fs, err := e.st.FolderSync(ctx, e.cfg.Account, folder)
	if err != nil {
		return err
	}
	// Defence in depth: a UIDVALIDITY the store reset elsewhere (or a
	// stale cursor row) must never anchor a pass (FR-S.6).
	if fs.UIDValidity != 0 && fs.UIDValidity != status.Version {
		if err := e.st.ResetFolder(ctx, e.cfg.Account, folder, status.Version); err != nil {
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
// (FR-S.3): the backend walks (by uid, or by sequence on sparse-uid
// servers), the engine commits each batch and saves the cursor so a
// restart resumes where the store says it stopped.
func (e *Engine) backfillLocked(ctx context.Context, folder string, status mb.FolderStatus, fs store.FolderSync) error {
	c := mb.Cursor{
		Version: status.Version, Next: status.Next, ModSeq: fs.HighestModSeq,
		Messages: status.Messages, BackfillMark: fs.BackfillUID,
	}
	labelPace := e.capsSnapshot().Labels
	for !c.BackfillDone {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !e.yieldToHydration(ctx) {
			return ctx.Err()
		}
		headers, next, err := e.work.Backfill(ctx, folder, c, e.cfg.BatchSize, mb.Hooks{})
		if err != nil {
			return err
		}
		if err := e.ingest(ctx, folder, status.Version, headers); err != nil {
			return err
		}
		c = next
		fs = store.FolderSync{
			UIDValidity: c.Version, UIDNext: c.Next, HighestModSeq: c.ModSeq,
			BackfillUID: c.BackfillMark, BackfillDone: c.BackfillDone,
		}
		if c.BackfillDone {
			fs.HighestModSeq = status.ModSeq
		}
		if err := e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs); err != nil {
			return err
		}
		e.log.Debug("sync: backfill batch", "folder", folder, "mark", c.BackfillMark)
		if c.BackfillDone {
			break
		}
		if labelPace {
			// FR-S.12: 500-header round trips at full speed are how a
			// provider starts slow-walking you.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(gmailBatchPace):
			}
		}
	}
	e.log.Info("sync: backfill complete",
		"folder", folder, "messages", fs.BackfillUID, "backend", e.work.Kind())
	return nil
}

// incrementalLocked applies one folder's post-backfill sync and folds
// the backend's delta into the store. The backend owns the tier/history
// strategy; the engine owns the local commit and the own-write grace
// window (FR-S.12).
func (e *Engine) incrementalLocked(ctx context.Context, folder string, status mb.FolderStatus, fs store.FolderSync) error {
	c := mb.Cursor{
		Version: fs.UIDValidity, Next: fs.UIDNext,
		ModSeq: fs.HighestModSeq, Messages: status.Messages,
	}
	hooks := mb.Hooks{KnownUIDs: func() ([]mb.Ref, error) {
		uids, err := e.st.FolderUIDs(ctx, e.cfg.Account, folder)
		if err != nil {
			return nil, err
		}
		refs := make([]mb.Ref, 0, len(uids))
		for _, u := range uids {
			refs = append(refs, mb.NewRef(folder, c.Version, u))
		}
		return refs, nil
	}}
	delta, next, err := e.work.Incremental(ctx, folder, c, hooks)
	if err != nil {
		return err
	}
	if delta.Reset {
		// The anchor was ignored or the container identity changed:
		// every cached reference may be stale (FR-S.6).
		if err := e.st.ResetFolder(ctx, e.cfg.Account, folder, next.Version); err != nil {
			return err
		}
		if err := e.st.ClearFolderSync(ctx, e.cfg.Account, folder); err != nil {
			return err
		}
		e.log.Warn("sync: folder reset, rescanning", "folder", folder)
		return nil
	}
	if err := e.applyDelta(ctx, folder, next.Version, delta); err != nil {
		return err
	}
	fs.UIDValidity = next.Version
	fs.UIDNext = next.Next
	if next.ModSeq > 0 {
		fs.HighestModSeq = next.ModSeq
	}
	fs.BackfillDone = true
	return e.st.SaveFolderSync(ctx, e.cfg.Account, folder, fs)
}

// applyDelta folds a backend delta into the store: tombstones (grace
// filtered), new headers, and flag changes.
func (e *Engine) applyDelta(ctx context.Context, folder string, uv uint32, delta mb.Delta) error {
	if len(delta.Removed) > 0 {
		uids := make([]uint32, 0, len(delta.Removed))
		for _, r := range delta.Removed {
			if u, ok := r.UID(); ok {
				uids = append(uids, u)
			}
		}
		// The grace window spares uids our own writes touched (FR-S.12):
		// a list that has not caught up must not tombstone membership
		// the server still holds.
		if gone := e.graceFilter(folder, uids); len(gone) > 0 {
			if err := e.st.RemoveUIDs(ctx, e.cfg.Account, folder, uv, gone); err != nil {
				return err
			}
		}
	}
	if err := e.ingest(ctx, folder, uv, delta.Headers); err != nil {
		return err
	}
	if delta.Bulk {
		updates := make([]store.FlagUpdate, 0, len(delta.Flags))
		for _, fc := range delta.Flags {
			if u, ok := fc.Ref.UID(); ok {
				updates = append(updates, store.FlagUpdate{UID: u, Flags: fc.Flags})
			}
		}
		return e.st.UpdateFlagsBulk(ctx, e.cfg.Account, folder, uv, updates)
	}
	var unknown []mb.Ref
	for _, fc := range delta.Flags {
		uid, ok := fc.Ref.UID()
		if !ok {
			continue
		}
		applied, err := e.st.UpdateFlags(ctx, e.cfg.Account, folder, uv, uid, fc.Flags)
		if err != nil {
			return err
		}
		if !applied {
			unknown = append(unknown, fc.Ref)
		}
	}
	if len(unknown) > 0 {
		// A uid the store does not know yet is a new message (CONDSTORE
		// reports new messages as flag traffic), so fetch its headers.
		headers, err := e.work.FetchHeaders(ctx, folder, unknown)
		if err != nil {
			return fmt.Errorf("flagged-new uids %q: %w", folder, err)
		}
		return e.ingest(ctx, folder, uv, headers)
	}
	return nil
}

// ingest converts neutral header records into store records and commits
// them (FR-S.3). On shared-UID servers the records carry the dedupe
// hint, so a message first seen through another folder dedupes instead
// of becoming a second JMAP object (FR-S.10).
func (e *Engine) ingest(ctx context.Context, folder string, uv uint32, headers []mb.Header) error {
	if len(headers) == 0 {
		return nil
	}
	shared := e.capsSnapshot().Labels
	recs := make([]store.MessageRec, 0, len(headers))
	for _, h := range headers {
		rec := convert.Summary(h.Envelope, h.Structure, h.InternalDate, h.Size)
		if uid, ok := h.Ref.UID(); ok {
			rec.UID = uid
		}
		rec.UIDValidity = uv
		rec.Flags = h.Flags
		rec.SharedUIDs = shared
		rec.GmThrid = h.ThreadHint
		rec.ThreadKey = h.ThreadKey
		rec.Preview = h.Preview
		recs = append(recs, rec)
	}
	return e.st.PutMessages(ctx, e.cfg.Account, folder, recs)
}

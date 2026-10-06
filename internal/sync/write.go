package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file is the write half of the engine (PLAN §7.1): the account's
// dedicated write session and the jmapapi.Backend implementation. Every
// mutation runs server-first — provider command, then local commit — so
// the cache never reports a change the server did not accept (golden
// rule 1) and never loses one it did (D-14). The provider-specific
// strategy (IMAP COPY/MOVE vs Gmail labels) lives behind
// mailbackend.Backend (M8).

// writer owns one account's write session: one backend session behind
// one mutex, never shared with the sync passes, so a triage action cannot
// queue behind a backfill (PLAN §10's per-account mutation queue).
type writer struct {
	mu         sync.Mutex
	newBackend func() mb.Backend
	log        *slog.Logger
	backend    mb.Backend
	delim      rune
	// labelFlag records a write session that speaks the label model
	// (Gmail), so the write path can fold membership accordingly
	// (FR-S.10) even before the work session's capabilities are known.
	labelFlag bool
	// caps reports the work session's capability profile.
	caps func() mb.Capabilities
	// count reports each dial attempt (FR-D.6); nil disables counting.
	count reconnectCounter
}

// gmail reports whether the account uses label-style membership.
func (w *writer) gmail() bool {
	w.mu.Lock()
	flag := w.labelFlag
	w.mu.Unlock()
	return flag || w.caps().Labels
}

// noteGmail records a label-capable session observed by the engine or a
// test.
func (w *writer) noteGmail(v bool) {
	w.mu.Lock()
	w.labelFlag = w.labelFlag || v
	w.mu.Unlock()
}

func newWriter(newBackend func() mb.Backend, log *slog.Logger, count reconnectCounter, caps func() mb.Capabilities) *writer {
	return &writer{newBackend: newBackend, log: log, count: count, caps: caps}
}

func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropLocked()
}

func (w *writer) dropLocked() {
	if w.backend != nil {
		_ = w.backend.Close()
		w.backend = nil
	}
}

// withBackend runs fn on a healthy session. An existing session is
// probed with Ping first — servers drop idle sessions, and a COPY must
// not be issued into a socket that died hours ago. A command that fails
// is never retried here: COPY and APPEND are not idempotent, so the
// client decides whether to try again.
func (w *writer) withBackend(ctx context.Context, fn func(mb.Backend) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	backend, err := w.ensureLocked(ctx)
	if err != nil {
		return err
	}
	if err := fn(backend); err != nil {
		if !mb.IsRejected(err) {
			// Not a refusal: the transport is suspect. Drop it so the
			// next write dials fresh instead of queueing behind a dead
			// socket.
			w.dropLocked()
		}
		return err
	}
	return nil
}

func (w *writer) ensureLocked(ctx context.Context) (mb.Backend, error) {
	if w.backend != nil {
		if err := w.backend.Ping(ctx); err == nil {
			return w.backend, nil
		} else {
			w.log.Debug("sync: write session stale", "err", err)
		}
		w.dropLocked()
	}
	if w.count != nil {
		w.count("write")
	}
	backend := w.newBackend()
	if err := backend.Connect(ctx); err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("sync: write connect: %w", err)
	}
	w.labelFlag = w.labelFlag || backend.Capabilities().Labels
	w.backend = backend
	w.log.Info("sync: write session up", "backend", backend.Kind())
	return backend, nil
}

// hierarchyDelim reads the separator once per session (Mailbox/set
// needs it to build a child path from parentId + name).
func (w *writer) hierarchyDelim(ctx context.Context) (rune, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.delim != 0 {
		return w.delim, nil
	}
	backend, err := w.ensureLocked(ctx)
	if err != nil {
		return 0, err
	}
	delim, err := backend.HierarchyDelim(ctx)
	if err != nil {
		w.dropLocked()
		return 0, err
	}
	w.delim = delim
	return delim, nil
}

// adminOp runs a mailbox administration command. Servers refuse
// DELETE/RENAME of a mailbox some session holds selected, and the sync
// pass may be holding one — so the first refusal releases the work
// session's selection and tries once more.
func (e *Engine) adminOp(ctx context.Context, op func(mb.Backend) error) error {
	err := e.wr.withBackend(ctx, op)
	if err == nil || !mb.IsRejected(err) {
		return err
	}
	e.releaseWorkSelection()
	return e.wr.withBackend(ctx, op)
}

// releaseWorkSelection waits for the work session (a pass may be
// running) and drops its mailbox selection.
func (e *Engine) releaseWorkSelection() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work == nil {
		return
	}
	if err := e.work.Release(context.Background()); err != nil {
		e.log.Debug("sync: release selection for admin op", "err", err)
	}
}

// --- jmapapi.Backend (FR-M.9–.12) ---

var _ jmapapi.Backend = (*Engine)(nil)

// ApplyEmailPatch runs one Email/set update: effective deltas only, one
// provider command family per folder copy, then a single local commit.
func (e *Engine) ApplyEmailPatch(ctx context.Context, account, emailID string, p jmapapi.EmailPatch) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: patch for foreign account %q", account)
	}
	exists, live, err := e.st.EmailState(ctx, account, emailID)
	if err != nil {
		return err
	}
	if !exists || !live {
		return jmapapi.ErrObjectNotFound
	}
	copies, err := e.st.EmailCopies(ctx, account, emailID)
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, c := range copies {
		current[c.MailboxID] = true
	}

	cur, err := e.st.EmailKeywords(ctx, account, emailID)
	if err != nil {
		return err
	}
	kwAdd, kwRemove := p.KeywordAdd, p.KeywordRemove
	if p.ReplaceKeywords {
		kwRemove = nil
		for k := range cur {
			if !contains(kwAdd, k) {
				kwRemove = append(kwRemove, k)
			}
		}
	}
	// Effective keyword deltas: a keyword already set is not added, and an
	// absent one is not removed. The server call must see the effective set
	// — Gmail refuses to remove a label the message does not carry (e.g.
	// DRAFT once a submission has consumed the draft), while a redundant
	// remove is a no-op on IMAP.
	var kwAddSet, kwRemoveSet []string
	for _, k := range kwAdd {
		if !cur[k] {
			kwAddSet = append(kwAddSet, k)
		}
	}
	for _, k := range kwRemove {
		if cur[k] {
			kwRemoveSet = append(kwRemoveSet, k)
		}
	}
	mbAdd, mbRemove := p.MailboxAdd, p.MailboxRemove
	if p.ReplaceMailboxes {
		mbRemove = nil
		for id := range current {
			if !contains(mbAdd, id) {
				mbRemove = append(mbRemove, id)
			}
		}
	}
	// Effective deltas: adding a membership that exists would double the
	// mailbox's counters, removing one that is gone is not a change.
	var addSet, remSet []string
	for _, id := range mbAdd {
		if !current[id] {
			addSet = append(addSet, id)
		}
	}
	for _, id := range mbRemove {
		if current[id] {
			remSet = append(remSet, id)
		}
	}
	if len(addSet) > 0 || len(remSet) > 0 {
		target := 0
		for id := range current {
			if !contains(remSet, id) {
				target++
			}
		}
		target += len(addSet)
		// Gmail (FR-M.18): the archive-role mailbox ([Gmail]/All Mail)
		// holds every message implicitly, so it counts as a membership
		// even before this account's All Mail pass has recorded it —
		// otherwise archiving the only copy would be refused here, and
		// "archive removes from INBOX only" is exactly the flow Gmail
		// users need.
		if e.wr.gmail() {
			if implicitID, err := e.st.ImplicitMailboxID(ctx, account); err == nil && implicitID != "" &&
				!contains(remSet, implicitID) && !current[implicitID] && !contains(addSet, implicitID) {
				target++
			}
		}
		if target == 0 {
			// An Email belongs to one or more Mailboxes at all times
			// (RFC 8621 §4.1); destroying it is destroy's job.
			return jmapapi.ErrWouldLeaveEmpty
		}
	}
	if len(kwAddSet) > 0 || len(kwRemoveSet) > 0 {
		if len(copies) == 0 {
			// Nothing addressable to STORE into; the mapping arrives
			// with the next sync pass.
			return jmapapi.ErrNoLocation
		}
	}
	if len(kwAddSet) == 0 && len(kwRemoveSet) == 0 && len(addSet) == 0 && len(remSet) == 0 {
		return nil // a patch whose effective delta is empty still succeeds
	}

	// Resolve destination mailboxes up front: an unknown id must fail
	// before any server command runs (all-or-nothing, FR-M.13).
	addMbs, err := e.mailboxesFor(ctx, addSet)
	if err != nil {
		return err
	}
	remMbs, err := e.mailboxesFor(ctx, remSet)
	if err != nil {
		return err
	}

	if len(kwAddSet) > 0 || len(kwRemoveSet) > 0 {
		if err := e.storeFlagsFor(ctx, copies, kwAddSet, kwRemoveSet); err != nil {
			return err
		}
	}
	adds, err := e.changeMembershipFor(ctx, emailID, copies, addMbs, remMbs)
	if err != nil {
		return err
	}

	kwChanged, err := e.st.CommitPatch(ctx, account, emailID, kwAddSet, kwRemoveSet, adds, remSet)
	e.log.Debug("sync: email patch committed",
		"email", emailID, "kwAdd", kwAddSet, "kwRemove", kwRemoveSet,
		"addedMailboxes", len(adds), "removedMailboxes", len(remSet),
		"keywordsChanged", kwChanged, "copies", len(copies))
	return err
}

// mailboxesFor resolves mailbox ids into the backend's neutral mailbox
// descriptors. An unknown id is ErrUnknownMailbox.
func (e *Engine) mailboxesFor(ctx context.Context, ids []string) ([]mb.Mailbox, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	mbs, _, notFound, err := e.st.MailboxesByID(ctx, e.cfg.Account, ids)
	if err != nil {
		return nil, err
	}
	if len(notFound) > 0 {
		return nil, jmapapi.ErrUnknownMailbox
	}
	allID, err := e.st.ImplicitMailboxID(ctx, e.cfg.Account)
	if err != nil {
		return nil, err
	}
	out := make([]mb.Mailbox, 0, len(mbs))
	for _, m := range mbs {
		out = append(out, mb.Mailbox{ID: m.ID, Path: m.Path, Role: m.Role, Implicit: m.ID == allID})
	}
	return out, nil
}

// backendCopies converts store copies into neutral backend copies.
func backendCopies(copies []store.Copy) []mb.Copy {
	out := make([]mb.Copy, 0, len(copies))
	for _, c := range copies {
		out = append(out, mb.Copy{
			MailboxID: c.MailboxID,
			Ref:       mb.NewRef(c.Folder, c.UIDValidity, c.UID),
		})
	}
	return out
}

// storeFlagsFor applies keyword deltas to every folder copy — IMAP
// flags are per-copy, so a message in two folders must be STOREd twice
// or the next sync pass would flip the keyword back (FR-M.8). A label
// backend folds this to one command itself.
func (e *Engine) storeFlagsFor(ctx context.Context, copies []store.Copy, kwAdd, kwRemove []string) error {
	if len(copies) == 0 {
		return jmapapi.ErrNoLocation
	}
	for _, c := range copies {
		if c.UID == 0 {
			// The uid mapping arrives with the next sync pass; acting
			// without it would desynchronise this copy from the server.
			return fmt.Errorf("%w: no uid for folder %q yet", jmapapi.ErrNoLocation, c.Folder)
		}
	}
	custom := customKeywords(append(append([]string{}, kwAdd...), kwRemove...))
	err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		return b.StoreKeywords(ctx, backendCopies(copies), kwAdd, kwRemove)
	})
	if err != nil {
		// A backend that names the keywords it cannot store (the Gmail
		// API's fixed keyword set) reports exactly those (FR-M.8 amended
		// to be backend-conditional, D-API-5).
		var unsupported *mb.UnsupportedKeywordsError
		if errors.As(err, &unsupported) {
			return &jmapapi.KeywordError{Keywords: unsupported.Keywords}
		}
		if mb.IsRejected(err) && len(custom) > 0 {
			// FR-M.8: a keyword the server will not store fails the write,
			// naming it — never dropped silently.
			return &jmapapi.KeywordError{Keywords: custom}
		}
	}
	return err
}

// messageFor builds the neutral write view of an email: its Message-ID
// (for backends that address a copy by header search) and the implicit
// archive container when one exists.
func (e *Engine) messageFor(ctx context.Context, emailID string) mb.Message {
	msg := mb.Message{ID: emailID}
	if msgid, err := e.st.EmailMessageID(ctx, e.cfg.Account, emailID); err == nil {
		msg.MessageID = msgid
	}
	if allID, err := e.st.ImplicitMailboxID(ctx, e.cfg.Account); err == nil && allID != "" {
		if p, err := e.st.MailboxPath(ctx, e.cfg.Account, allID); err == nil {
			msg.ImplicitContainer = p
		}
	}
	return msg
}

// changeMembershipFor delegates a membership change to the backend and
// records the copies it touched for the grace window (FR-S.12).
func (e *Engine) changeMembershipFor(ctx context.Context, emailID string, copies []store.Copy, add, remove []mb.Mailbox) ([]store.MembershipAdd, error) {
	msg := e.messageFor(ctx, emailID)
	var added []mb.Copy
	var touched []mb.Ref
	err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		var e2 error
		added, touched, e2 = b.SetMembership(ctx, msg, backendCopies(copies), add, remove)
		return e2
	})
	if err != nil {
		return nil, err
	}
	e.recordOwnWrite(touched)
	out := make([]store.MembershipAdd, 0, len(added))
	for _, c := range added {
		uid, _ := c.Ref.UID()
		out = append(out, store.MembershipAdd{
			MailboxID: c.MailboxID, UID: uid, UIDValidity: c.Ref.VersionNum(),
		})
	}
	return out, nil
}

// DestroyEmails expunges every copy of one email and tombstones it
// (FR-M.10). An id the cache has never seen is notFound; one it already
// tombstoned is a no-op success.
func (e *Engine) DestroyEmails(ctx context.Context, account, emailID string) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: destroy for foreign account %q", account)
	}
	exists, _, err := e.st.EmailState(ctx, account, emailID)
	if err != nil {
		return err
	}
	if !exists {
		return jmapapi.ErrObjectNotFound
	}
	copies, err := e.st.EmailCopies(ctx, account, emailID)
	if err != nil {
		return err
	}
	refs := make([]mb.Ref, 0, len(copies))
	for _, c := range copies {
		if c.UID == 0 {
			return fmt.Errorf("%w: no uid for folder %q yet", jmapapi.ErrNoLocation, c.Folder)
		}
		refs = append(refs, mb.NewRef(c.Folder, c.UIDValidity, c.UID))
	}
	if len(refs) > 0 {
		// On a label server destroy routes through trash (FR-M.18);
		// the adapter falls back to expunge-in-place without one.
		trash := ""
		if e.wr.gmail() {
			if id, err := e.st.MailboxIDByRole(ctx, account, "trash"); err == nil && id != "" {
				if p, err := e.st.MailboxPath(ctx, account, id); err == nil {
					trash = p
				}
			}
		}
		var touched []mb.Ref
		if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
			var e2 error
			touched, e2 = b.Destroy(ctx, refs, trash)
			return e2
		}); err != nil {
			return err
		}
		e.recordOwnWrite(touched)
	}
	return e.st.CommitDestroy(ctx, account, []string{emailID})
}

// CreateDraft builds, appends and records one draft (FR-M.11).
func (e *Engine) CreateDraft(ctx context.Context, account string, spec jmapapi.DraftSpec) (*jmapapi.CreatedEmail, error) {
	if account != e.cfg.Account {
		return nil, fmt.Errorf("sync: create for foreign account %q", account)
	}
	folder, isDrafts, err := e.draftFolder(ctx, account, spec.MailboxIDs)
	if err != nil {
		return nil, err
	}

	parts := make([]convert.DraftPart, 0, len(spec.Parts))
	for _, p := range spec.Parts {
		dp := convert.DraftPart{
			Type: p.Type, Disposition: p.Disposition, Charset: p.Charset,
			Name: p.Name, Text: p.Text,
		}
		if p.BlobID != "" {
			data, _, err := e.st.ReadBlob(ctx, account, p.BlobID)
			if err != nil {
				return nil, fmt.Errorf("%w: attachment blob %s", jmapapi.ErrBlobNotFound, p.BlobID)
			}
			dp.Data = data
		}
		parts = append(parts, dp)
	}

	received := spec.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	draft, err := convert.BuildDraft(convert.DraftInput{
		From:       toConvertAddrs(spec.From),
		To:         toConvertAddrs(spec.To),
		Cc:         toConvertAddrs(spec.Cc),
		Bcc:        toConvertAddrs(spec.Bcc),
		ReplyTo:    toConvertAddrs(spec.ReplyTo),
		Subject:    spec.Subject,
		Parts:      parts,
		InReplyTo:  spec.InReplyTo,
		References: spec.References,
		ReceivedAt: received,
	})
	if err != nil {
		return nil, err
	}

	kws := boolKeys(spec.Keywords)
	if isDrafts && !contains(kws, "$draft") {
		kws = append(kws, "$draft") // PLAN §7.1: drafts carry \Draft
	}

	// Store the bytes before appending: a blob write failure must not
	// leave a message on the server that no create reported (and its
	// blobId is part of the created object, RFC 8621 §4.6).
	blobID, err := e.st.PutBlob(ctx, account, "message/rfc822", draft.Raw)
	if err != nil {
		return nil, err
	}

	var ref mb.Ref
	var flags []string
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		var e2 error
		ref, flags, e2 = b.Append(ctx, folder, draft.Raw, kws, &received)
		return e2
	}); err != nil {
		return nil, err
	}

	rec := draft.Rec
	rec.Flags = flags
	rec.UID, _ = ref.UID()
	rec.UIDValidity = ref.VersionNum()
	created, err := e.st.CommitAppend(ctx, account, folder, rec)
	if err != nil {
		return nil, err
	}
	// The bytes we just appended are the message's raw copy: linking
	// them gives Email/get its blobId and lets a later submission send
	// exactly what the client saved (FR-M.15, RFC 8621 §4.1.1).
	if err := e.st.LinkRawBlob(ctx, account, created.ID, blobID); err != nil {
		e.log.Warn("sync: draft raw copy not linked", "email", created.ID, "err", err)
	}
	// Body values and attachment blobs are already known from the build;
	// a failure here only costs a hydration fetch on the next read.
	if err := e.st.PutHydrated(ctx, account, created.ID, draft.Body); err != nil {
		e.log.Warn("sync: draft body not cached", "email", created.ID, "err", err)
	}
	// The APPEND filed the message in the first named mailbox; the rest of
	// the create's mailboxIds go through the proven membership path, so a
	// create into several mailboxes lands in all of them (RFC 8621 §4.6).
	if len(spec.MailboxIDs) > 1 {
		if err := e.ApplyEmailPatch(ctx, account, created.ID, jmapapi.EmailPatch{
			MailboxAdd: spec.MailboxIDs[1:],
		}); err != nil {
			return nil, err
		}
	}
	return &jmapapi.CreatedEmail{
		ID: created.ID, BlobID: blobID, ThreadID: created.ThreadID, Size: created.Size,
	}, nil
}

// ImportEmail files client-supplied raw RFC 5322 bytes, already held in
// this account's blob store, into the named mailboxes (RFC 8621 §4.8).
// The bytes ARE the message, so there is no build step: APPEND once to
// the first mailbox, commit, then file any extra mailboxes through the
// proven membership path.
func (e *Engine) ImportEmail(ctx context.Context, account string, spec jmapapi.ImportSpec) (*jmapapi.CreatedEmail, error) {
	if account != e.cfg.Account {
		return nil, fmt.Errorf("sync: import for foreign account %q", account)
	}
	raw, _, err := e.st.ReadBlob(ctx, account, spec.BlobID)
	if err != nil {
		return nil, fmt.Errorf("%w: blob %s", jmapapi.ErrBlobNotFound, spec.BlobID)
	}
	mbs, _, notFound, err := e.st.MailboxesByID(ctx, account, spec.MailboxIDs)
	if err != nil {
		return nil, err
	}
	if len(spec.MailboxIDs) == 0 || len(notFound) > 0 || len(mbs) == 0 {
		return nil, jmapapi.ErrUnknownMailbox
	}
	folder := mbs[0].Path

	kws := boolKeys(spec.Keywords)
	received := spec.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	// Build the summary before appending: a message only the server
	// knows about (a parse the cache cannot record) is a lost import.
	rec, err := convert.SummaryFromRaw(raw, received)
	if err != nil {
		return nil, err
	}

	var ref mb.Ref
	var flags []string
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		var e2 error
		ref, flags, e2 = b.Append(ctx, folder, raw, kws, &received)
		return e2
	}); err != nil {
		return nil, err
	}
	rec.Flags = flags
	rec.UID, _ = ref.UID()
	rec.UIDValidity = ref.VersionNum()

	created, err := e.st.CommitAppend(ctx, account, folder, rec)
	if err != nil {
		return nil, err
	}
	// The bytes we appended are the message's raw copy; linking them
	// gives Email/get its blobId (FR-M.4, RFC 8621 §4.1.1).
	if err := e.st.LinkRawBlob(ctx, account, created.ID, spec.BlobID); err != nil {
		e.log.Warn("sync: imported raw copy not linked", "email", created.ID, "err", err)
	}
	// Body values are already in the imported bytes; a failure here only
	// costs a hydration fetch on the next read.
	res, _ := convert.ParseBody(raw) // a soft error still yields usable parts
	if err := e.st.PutHydrated(ctx, account, created.ID, res); err != nil {
		e.log.Warn("sync: imported body not cached", "email", created.ID, "err", err)
	}
	if len(spec.MailboxIDs) > 1 {
		if err := e.ApplyEmailPatch(ctx, account, created.ID, jmapapi.EmailPatch{
			MailboxAdd: spec.MailboxIDs[1:],
		}); err != nil {
			// The message exists in the first mailbox; the extras did
			// not land, so the create fails rather than under-reporting
			// its membership (FR-M.13).
			return nil, err
		}
	}
	return &jmapapi.CreatedEmail{
		ID: created.ID, BlobID: spec.BlobID, ThreadID: created.ThreadID, Size: created.Size,
	}, nil
}

// draftFolder resolves where a create goes: the mailbox the client
// named (first id), or the account's drafts mailbox (FR-M.11).
func (e *Engine) draftFolder(ctx context.Context, account string, mailboxIDs []string) (folder string, isDrafts bool, err error) {
	if len(mailboxIDs) == 0 {
		id, err := e.st.MailboxIDByRole(ctx, account, "drafts")
		if err != nil {
			return "", false, err
		}
		if id == "" {
			return "", false, jmapapi.ErrUnknownMailbox
		}
		mailboxIDs = []string{id}
	}
	mbs, _, notFound, err := e.st.MailboxesByID(ctx, account, mailboxIDs[:1])
	if err != nil {
		return "", false, err
	}
	if len(notFound) > 0 || len(mbs) == 0 {
		return "", false, jmapapi.ErrUnknownMailbox
	}
	return mbs[0].Path, mbs[0].Role == "drafts", nil
}

// CreateMailbox runs CREATE, refreshes discovery (role detection,
// FR-M.12) and returns the id the refresh minted. sortOrder is the
// client-requested display order (0 = none requested): the engine
// applies it to the cache after discovery, since IMAP folders carry
// no order — a later discovery pass may re-derive it.
func (e *Engine) CreateMailbox(ctx context.Context, account, name, parentID string, sortOrder int) (string, error) {
	if account != e.cfg.Account {
		return "", fmt.Errorf("sync: create mailbox for foreign account %q", account)
	}
	delim, err := e.wr.hierarchyDelim(ctx)
	if err != nil {
		return "", err
	}
	path, err := e.mailboxPath(ctx, account, name, parentID, delim)
	if err != nil {
		return "", err
	}
	if existing, err := e.st.MailboxIDByPath(ctx, account, path); err != nil {
		return "", err
	} else if existing != "" {
		return "", jmapapi.ErrMailboxExists
	}
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		return b.CreateMailbox(ctx, path)
	}); err != nil {
		return "", err
	}
	if err := e.refreshFolders(ctx); err != nil {
		return "", err
	}
	id, err := e.st.MailboxIDByPath(ctx, account, path)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("server accepted CREATE %q but does not list it", path)
	}
	if sortOrder != 0 {
		if err := e.st.SetMailboxSortOrder(ctx, account, id, sortOrder); err != nil {
			return "", err
		}
	}
	return id, nil
}

// RenameMailbox rewrites a mailbox's name and/or parent. The id is
// stable (RFC 8621 §5.1): the local rename lands before discovery runs,
// so the refresh updates the row instead of tombstoning it and minting
// a new id.
func (e *Engine) RenameMailbox(ctx context.Context, account, id, name, parentID string) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: rename mailbox for foreign account %q", account)
	}
	delim, err := e.wr.hierarchyDelim(ctx)
	if err != nil {
		return err
	}
	oldPath, err := e.st.MailboxPath(ctx, account, id)
	if err != nil {
		if errors.Is(err, store.ErrMailboxUnknown) {
			return jmapapi.ErrObjectNotFound
		}
		return err
	}
	newPath, err := e.mailboxPath(ctx, account, name, parentID, delim)
	if err != nil {
		return err
	}
	if parentID == id {
		return fmt.Errorf("%w: a mailbox cannot be its own parent", jmapapi.ErrInvalidMailboxName)
	}
	if parentID != "" && e.isDescendant(ctx, account, parentID, id) {
		return fmt.Errorf("%w: the new parent is inside this mailbox", jmapapi.ErrInvalidMailboxName)
	}
	if existing, err := e.st.MailboxIDByPath(ctx, account, newPath); err != nil {
		return err
	} else if existing != "" && existing != id {
		return jmapapi.ErrMailboxExists
	}
	if newPath == oldPath {
		return nil // rename to the same path: a successful no-op
	}
	// RENAME, then LIST in the same session: the server's answer about
	// the subtree decides what the cache rewrites (RFC 3501 §6.3.5 says
	// descendants follow; not every implementation does).
	var serverNames []string
	if err := e.adminOp(ctx, func(b mb.Backend) error {
		if err := b.RenameMailbox(ctx, oldPath, newPath); err != nil {
			return err
		}
		list, err := b.Folders(ctx)
		if err != nil {
			return err
		}
		for _, f := range list {
			serverNames = append(serverNames, f.Name)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := e.st.CommitMailboxRename(ctx, account, id, oldPath, newPath, parentID, delim,
		e.serverRenamedChildren(ctx, account, oldPath, newPath, delim, serverNames)); err != nil {
		return err
	}
	return e.refreshFolders(ctx)
}

// DestroyMailbox deletes a folder. With removeEmails (RFC 8621 §2.5
// onDestroyRemoveEmails) every message in it is expunged first — in
// IMAP that is the only way to remove a message from a folder, and it
// matches the RFC: mail that lived nowhere else is destroyed, mail that
// lives elsewhere keeps its id.
func (e *Engine) DestroyMailbox(ctx context.Context, account, id string, removeEmails bool) error {
	if account != e.cfg.Account {
		return fmt.Errorf("sync: destroy mailbox for foreign account %q", account)
	}
	mbs, _, notFound, err := e.st.MailboxesByID(ctx, account, []string{id})
	if err != nil {
		return err
	}
	if len(notFound) > 0 || len(mbs) == 0 {
		return jmapapi.ErrObjectNotFound
	}
	mbx := mbs[0]

	all, _, err := e.st.Mailboxes(ctx, account)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ParentID == id {
			return jmapapi.ErrMailboxHasChild
		}
	}
	// A Gmail label never owns its messages, so onDestroyRemoveEmails=true
	// cannot be honoured without permanently deleting mail the label API
	// never asked us to touch. Refuse it by name rather than acknowledge a
	// bulk destroy we cannot perform (GMAIL_API_PLAN §16.4, FR-M.20).
	if removeEmails && e.wr.gmail() {
		return jmapapi.ErrOnDestroyRemoveEmails
	}
	if mbx.TotalEmails > 0 {
		if !removeEmails {
			return jmapapi.ErrMailboxHasEmail
		}
		uids, err := e.st.FolderUIDs(ctx, account, mbx.Path)
		if err != nil {
			return err
		}
		if len(uids) > 0 {
			refs := make([]mb.Ref, 0, len(uids))
			for _, u := range uids {
				refs = append(refs, mb.NewRef(mbx.Path, 0, u))
			}
			if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
				_, e2 := b.Destroy(ctx, refs, "")
				return e2
			}); err != nil {
				return err
			}
		}
		if err := e.st.EmptyFolder(ctx, account, mbx.Path); err != nil {
			return err
		}
	}
	if err := e.adminOp(ctx, func(b mb.Backend) error {
		return b.DeleteMailbox(ctx, mbx.Path)
	}); err != nil {
		return err
	}
	// Discovery reconciles the row away (the vanished-folder path), so
	// /changes reports the destruction it earned.
	return e.refreshFolders(ctx)
}

// serverRenamedChildren reports whether LIST shows the subtree moved
// with its parent: a child's new path is listed and its old one is not.
func (e *Engine) serverRenamedChildren(ctx context.Context, account, oldPath, newPath string, delim rune, serverNames []string) bool {
	all, _, err := e.st.Mailboxes(ctx, account)
	if err != nil {
		return false
	}
	for _, mbx := range all {
		if !strings.HasPrefix(mbx.Path, oldPath+string(delim)) {
			continue
		}
		suffix := strings.TrimPrefix(mbx.Path, oldPath)
		if contains(serverNames, newPath+suffix) && !contains(serverNames, mbx.Path) {
			return true
		}
	}
	return false
}

// mailboxPath builds the IMAP path for a name under a parent, refusing
// names the separator would turn into hidden hierarchy.
func (e *Engine) mailboxPath(ctx context.Context, account, name, parentID string, delim rune) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return "", fmt.Errorf("%w: name must not be empty", jmapapi.ErrInvalidMailboxName)
	}
	if delim != 0 && strings.ContainsRune(name, delim) {
		return "", fmt.Errorf("%w: name must not contain the hierarchy separator %q",
			jmapapi.ErrInvalidMailboxName, string(delim))
	}
	if len(name) > 255 {
		return "", fmt.Errorf("%w: name is longer than 255 characters", jmapapi.ErrInvalidMailboxName)
	}
	if parentID == "" {
		return name, nil
	}
	parent, err := e.st.MailboxPath(ctx, account, parentID)
	if errors.Is(err, store.ErrMailboxUnknown) {
		return "", jmapapi.ErrUnknownMailbox
	}
	if err != nil {
		return "", err
	}
	return parent + string(delim) + name, nil
}

// isDescendant reports whether candidate is inside ancestor — a parent
// swap that would fold a mailbox into its own subtree.
func (e *Engine) isDescendant(ctx context.Context, account, candidate, ancestor string) bool {
	seen := map[string]bool{}
	for candidate != "" && !seen[candidate] {
		if candidate == ancestor {
			return true
		}
		seen[candidate] = true
		mbs, _, notFound, err := e.st.MailboxesByID(ctx, account, []string{candidate})
		if err != nil || len(notFound) > 0 || len(mbs) == 0 {
			return false
		}
		candidate = mbs[0].ParentID
	}
	return false
}

// refreshFolders re-runs discovery on the write session: LIST + STATUS +
// SyncFolders, which is where SPECIAL-USE roles are re-detected
// (FR-M.12) and where a deleted folder is reconciled away.
func (e *Engine) refreshFolders(ctx context.Context) error {
	return e.wr.withBackend(ctx, func(b mb.Backend) error {
		_, err := e.discoverWith(ctx, b)
		return err
	})
}

// --- helpers ---

func boolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// customKeywords filters out the five system keywords (FR-M.8): those
// map to IMAP system flags and are refused for a different reason.
func customKeywords(keys []string) []string {
	var out []string
	for _, k := range keys {
		if keyword.SystemFlag(k) == "" && k != "" {
			out = append(out, k)
		}
	}
	return out
}

func toConvertAddrs(in []jmapapi.Address) []convert.MailboxAddress {
	out := make([]convert.MailboxAddress, 0, len(in))
	for _, a := range in {
		out = append(out, convert.MailboxAddress{Name: a.Name, Email: a.Email})
	}
	return out
}

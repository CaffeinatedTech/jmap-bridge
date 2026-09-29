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
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/keyword"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file is the write half of the engine (PLAN §7.1): the account's
// dedicated IMAP session and the jmapapi.Backend implementation. Every
// mutation runs server-first — IMAP command, then local commit — so the
// cache never reports a change the server did not accept (golden rule
// 1) and never loses one it did (D-14).

// writer owns one account's write session: one connection behind one
// mutex, never shared with the sync passes, so a triage action cannot
// queue behind a backfill (PLAN §10's per-account mutation queue).
type writer struct {
	mu    sync.Mutex
	cfg   imapdrv.Config
	log   *slog.Logger
	conn  *imapdrv.Conn
	delim rune
}

func newWriter(cfg imapdrv.Config, log *slog.Logger) *writer {
	return &writer{cfg: cfg, log: log}
}

func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropLocked()
}

func (w *writer) dropLocked() {
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

// withConn runs fn on a healthy connection. An existing session is
// probed with NOOP first — servers drop idle sessions, and a COPY must
// not be issued into a socket that died hours ago. A command that fails
// is never retried here: COPY and APPEND are not idempotent, so the
// client decides whether to try again.
func (w *writer) withConn(ctx context.Context, fn func(*imapdrv.Conn) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	conn, err := w.ensureLocked(ctx)
	if err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		if !imapdrv.ServerRejected(err) {
			// Not a refusal: the transport is suspect. Drop it so the
			// next write dials fresh instead of queueing behind a dead
			// socket.
			w.dropLocked()
		}
		return err
	}
	return nil
}

func (w *writer) ensureLocked(ctx context.Context) (*imapdrv.Conn, error) {
	if w.conn != nil {
		if err := w.conn.Ping(ctx); err == nil {
			return w.conn, nil
		} else {
			w.log.Debug("sync: write session stale", "err", err)
		}
		w.dropLocked()
	}
	conn, err := imapdrv.Dial(ctx, w.cfg)
	if err != nil {
		return nil, fmt.Errorf("sync: write connect: %w", err)
	}
	w.conn = conn
	w.log.Info("sync: write session up",
		"tier", conn.Tier().String(), "compressed", conn.Compressed())
	return conn, nil
}

// hierarchyDelim reads the separator once per session (Mailbox/set
// needs it to build a child path from parentId + name).
func (w *writer) hierarchyDelim(ctx context.Context) (rune, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.delim != 0 {
		return w.delim, nil
	}
	conn, err := w.ensureLocked(ctx)
	if err != nil {
		return 0, err
	}
	delim, err := conn.HierarchyDelim(ctx)
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
// connection's selection and tries once more.
func (e *Engine) adminOp(ctx context.Context, op func(*imapdrv.Conn) error) error {
	err := e.wr.withConn(ctx, op)
	if err == nil || !imapdrv.ServerRejected(err) {
		return err
	}
	e.releaseWorkSelection()
	return e.wr.withConn(ctx, op)
}

// releaseWorkSelection waits for the work connection (a pass may be
// running) and drops its mailbox selection.
func (e *Engine) releaseWorkSelection() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work == nil {
		return
	}
	if err := e.work.Unselect(context.Background()); err != nil {
		e.log.Debug("sync: release selection for admin op", "err", err)
	}
}

// --- jmapapi.Backend (FR-M.9–.12) ---

var _ jmapapi.Backend = (*Engine)(nil)

// ApplyEmailPatch runs one Email/set update: effective deltas only, one
// IMAP command family per folder copy, then a single local commit.
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

	kwAdd, kwRemove := p.KeywordAdd, p.KeywordRemove
	if p.ReplaceKeywords {
		cur, err := e.st.EmailKeywords(ctx, account, emailID)
		if err != nil {
			return err
		}
		kwRemove = nil
		for k := range cur {
			if !contains(kwAdd, k) {
				kwRemove = append(kwRemove, k)
			}
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
		if target == 0 {
			// An Email belongs to one or more Mailboxes at all times
			// (RFC 8621 §4.1); destroying it is destroy's job.
			return jmapapi.ErrWouldLeaveEmpty
		}
	}
	if len(kwAdd) > 0 || len(kwRemove) > 0 {
		if len(copies) == 0 {
			// Nothing addressable to STORE into; the mapping arrives
			// with the next sync pass.
			return jmapapi.ErrNoLocation
		}
	}
	if len(kwAdd) == 0 && len(kwRemove) == 0 && len(addSet) == 0 && len(remSet) == 0 {
		return nil // a patch whose effective delta is empty still succeeds
	}

	// Resolve destination paths up front: an unknown id must fail
	// before any server command runs (all-or-nothing, FR-M.13).
	addPaths := make(map[string]string, len(addSet))
	for _, id := range addSet {
		path, err := e.st.MailboxPath(ctx, account, id)
		if errors.Is(err, store.ErrMailboxUnknown) {
			return jmapapi.ErrUnknownMailbox
		}
		if err != nil {
			return err
		}
		addPaths[id] = path
	}

	if len(kwAdd) > 0 || len(kwRemove) > 0 {
		if err := e.storeFlagsFor(ctx, copies, kwAdd, kwRemove); err != nil {
			return err
		}
	}
	adds, err := e.changeMembership(ctx, copies, addSet, addPaths, remSet)
	if err != nil {
		return err
	}

	kwChanged, err := e.st.CommitPatch(ctx, account, emailID, kwAdd, kwRemove, adds, remSet)
	e.log.Debug("sync: email patch committed",
		"email", emailID, "kwAdd", kwAdd, "kwRemove", kwRemove,
		"addedMailboxes", len(adds), "removedMailboxes", len(remSet),
		"keywordsChanged", kwChanged, "copies", len(copies))
	return err
}

// storeFlagsFor applies keyword deltas to every folder copy — IMAP
// flags are per-copy, so a message in two folders must be STOREd twice
// or the next sync pass would flip the keyword back (FR-M.8).
func (e *Engine) storeFlagsFor(ctx context.Context, copies []store.Copy, kwAdd, kwRemove []string) error {
	byFolder := map[string][]uint32{}
	for _, c := range copies {
		if c.UID == 0 {
			// The uid mapping arrives with the next sync pass; acting
			// without it would desynchronise this copy from the server.
			return fmt.Errorf("%w: no uid for folder %q yet", jmapapi.ErrNoLocation, c.Folder)
		}
		byFolder[c.Folder] = append(byFolder[c.Folder], c.UID)
	}
	if len(byFolder) == 0 {
		return jmapapi.ErrNoLocation
	}
	add := keyword.ToIMAP(truthy(kwAdd))
	remove := keyword.ToIMAP(truthy(kwRemove))
	custom := customKeywords(append(append([]string{}, kwAdd...), kwRemove...))
	return e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		for folder, uids := range byFolder {
			if err := conn.StoreFlags(ctx, folder, uids, add, remove); err != nil {
				if imapdrv.ServerRejected(err) && len(custom) > 0 {
					// FR-M.8: a keyword the server will not store fails
					// the write, naming it — never dropped silently.
					return &jmapapi.KeywordError{Keywords: custom}
				}
				return err
			}
		}
		return nil
	})
}

// changeMembership performs the server side of a membership change and
// returns what to commit. One removal plus one addition becomes a MOVE
// (PLAN §7.1); anything else is copy-then-expunge, which is the only
// sequence IMAP can express for "this message is in these folders".
func (e *Engine) changeMembership(ctx context.Context, copies []store.Copy, addSet []string, addPaths map[string]string, remSet []string) ([]store.MembershipAdd, error) {
	if len(addSet) == 0 {
		return nil, e.expungeCopies(ctx, copies, remSet)
	}

	// A source copy we are allowed to move: it must be in a folder the
	// patch is removing, and its uid must be known.
	var moveSrc *store.Copy
	for i := range copies {
		if contains(remSet, copies[i].MailboxID) && copies[i].UID != 0 {
			moveSrc = &copies[i]
			break
		}
	}
	if len(addSet) == 1 && moveSrc != nil {
		dstID := addSet[0]
		var res imapdrv.CopyResult
		err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
			var err error
			res, err = conn.MoveUIDs(ctx, moveSrc.Folder, addPaths[dstID], []uint32{moveSrc.UID})
			return err
		})
		if err != nil {
			return nil, err
		}
		adds := []store.MembershipAdd{{
			MailboxID: dstID, UID: res.DestUIDs[moveSrc.UID], UIDValidity: res.DestUIDValidity,
		}}
		rest := make([]string, 0, len(remSet))
		for _, id := range remSet {
			if id != moveSrc.MailboxID {
				rest = append(rest, id)
			}
		}
		if err := e.expungeCopies(ctx, copies, rest); err != nil {
			return nil, err
		}
		return adds, nil
	}

	// Copy into every destination first (so a failure leaves the source
	// intact), then expunge the removals.
	var src *store.Copy
	for i := range copies {
		if copies[i].UID != 0 {
			src = &copies[i]
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("%w: no folder holds a known uid for this message", jmapapi.ErrNoLocation)
	}
	adds := make([]store.MembershipAdd, 0, len(addSet))
	for _, dstID := range addSet {
		var res imapdrv.CopyResult
		err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
			var err error
			res, err = conn.CopyUIDs(ctx, src.Folder, addPaths[dstID], []uint32{src.UID})
			return err
		})
		if err != nil {
			return nil, err
		}
		adds = append(adds, store.MembershipAdd{
			MailboxID: dstID, UID: res.DestUIDs[src.UID], UIDValidity: res.DestUIDValidity,
		})
	}
	if err := e.expungeCopies(ctx, copies, remSet); err != nil {
		return nil, err
	}
	return adds, nil
}

// expungeCopies permanently removes the named mailboxes' copies of the
// message (FR-M.10's per-folder half).
func (e *Engine) expungeCopies(ctx context.Context, copies []store.Copy, remove []string) error {
	if len(remove) == 0 {
		return nil
	}
	byFolder := map[string][]uint32{}
	for _, c := range copies {
		if !contains(remove, c.MailboxID) {
			continue
		}
		if c.UID == 0 {
			return fmt.Errorf("%w: no uid for folder %q yet", jmapapi.ErrNoLocation, c.Folder)
		}
		byFolder[c.Folder] = append(byFolder[c.Folder], c.UID)
	}
	if len(byFolder) == 0 {
		return nil
	}
	return e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		for folder, uids := range byFolder {
			if err := conn.ExpungeUIDs(ctx, folder, uids); err != nil {
				return err
			}
		}
		return nil
	})
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
	byFolder := map[string][]uint32{}
	for _, c := range copies {
		if c.UID == 0 {
			return fmt.Errorf("%w: no uid for folder %q yet", jmapapi.ErrNoLocation, c.Folder)
		}
		byFolder[c.Folder] = append(byFolder[c.Folder], c.UID)
	}
	if len(byFolder) > 0 {
		if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
			for folder, uids := range byFolder {
				if err := conn.ExpungeUIDs(ctx, folder, uids); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
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

	flags := keyword.ToIMAP(truthy(boolKeys(spec.Keywords)))
	if isDrafts && !contains(flags, `\Draft`) {
		flags = append(flags, `\Draft`) // PLAN §7.1: drafts carry \Draft
	}

	// Store the bytes before appending: a blob write failure must not
	// leave a message on the server that no create reported (and its
	// blobId is part of the created object, RFC 8621 §4.6).
	blobID, err := e.st.PutBlob(ctx, account, "message/rfc822", draft.Raw)
	if err != nil {
		return nil, err
	}

	var uid, uidValidity uint32
	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		var err error
		uid, uidValidity, err = conn.AppendMessage(ctx, folder, draft.Raw, flags, &received)
		return err
	}); err != nil {
		return nil, err
	}

	rec := draft.Rec
	rec.Flags = flags
	rec.UID = uid
	rec.UIDValidity = uidValidity
	created, err := e.st.CommitAppend(ctx, account, folder, rec)
	if err != nil {
		return nil, err
	}
	// Body values and attachment blobs are already known from the build;
	// a failure here only costs a hydration fetch on the next read.
	if err := e.st.PutHydrated(ctx, account, created.ID, draft.Body); err != nil {
		e.log.Warn("sync: draft body not cached", "email", created.ID, "err", err)
	}
	return &jmapapi.CreatedEmail{
		ID: created.ID, BlobID: blobID, ThreadID: created.ThreadID, Size: created.Size,
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
	return mbs[0].Name, mbs[0].Role == "drafts", nil
}

// CreateMailbox runs CREATE, refreshes discovery (role detection,
// FR-M.12) and returns the id the refresh minted.
func (e *Engine) CreateMailbox(ctx context.Context, account, name, parentID string) (string, error) {
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
	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		return conn.CreateMailbox(ctx, path)
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
	if err := e.adminOp(ctx, func(conn *imapdrv.Conn) error {
		if err := conn.RenameMailbox(ctx, oldPath, newPath); err != nil {
			return err
		}
		list, err := conn.ListFolders(ctx)
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
	mb := mbs[0]

	all, _, err := e.st.Mailboxes(ctx, account)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ParentID == id {
			return jmapapi.ErrMailboxHasChild
		}
	}
	if mb.TotalEmails > 0 {
		if !removeEmails {
			return jmapapi.ErrMailboxHasEmail
		}
		uids, err := e.st.FolderUIDs(ctx, account, mb.Name)
		if err != nil {
			return err
		}
		if len(uids) > 0 {
			if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
				return conn.ExpungeUIDs(ctx, mb.Name, uids)
			}); err != nil {
				return err
			}
		}
		if err := e.st.EmptyFolder(ctx, account, mb.Name); err != nil {
			return err
		}
	}
	if err := e.adminOp(ctx, func(conn *imapdrv.Conn) error {
		return conn.DeleteMailbox(ctx, mb.Name)
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
	for _, mb := range all {
		if !strings.HasPrefix(mb.Name, oldPath+string(delim)) {
			continue
		}
		suffix := strings.TrimPrefix(mb.Name, oldPath)
		if contains(serverNames, newPath+suffix) && !contains(serverNames, mb.Name) {
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

// refreshFolders re-runs discovery on the write connection: LIST +
// STATUS + SyncFolders, which is where SPECIAL-USE roles are
// re-detected (FR-M.12) and where a deleted folder is reconciled away.
func (e *Engine) refreshFolders(ctx context.Context) error {
	return e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		_, err := e.discoverWith(ctx, conn, false)
		return err
	})
}

// --- helpers ---

func truthy(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

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

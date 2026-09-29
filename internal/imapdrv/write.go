package imapdrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// This file is the write half of the driver (PLAN §7.1): the commands
// that make a change on the server. Like the read half it exposes no
// library types, and like the rest of the package it stays deliberately
// dumb — which folder a JMAP id lives in, and what the local commit
// looks like, belong to the store and the API layer. None of these
// methods is safe for concurrent use; the caller serialises them (the
// sync engine's write session holds one mutex per account, PLAN §10).

// ServerRejected reports whether err is the server answering NO/BAD to a
// command it understood, as opposed to the connection failing — the
// distinction FR-M.8 needs to turn a refused custom keyword into
// invalidArguments while a dropped link stays serverFail.
func ServerRejected(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr) &&
		(imapErr.Type == imap.ErrorTypeNo || imapErr.Type == imap.ErrorTypeBad)
}

// RejectText is the server's human-readable refusal, for SetError
// descriptions; it is empty when the failure was not a refusal.
func RejectText(err error) string {
	var imapErr *imap.Error
	if errors.As(err, &imapErr) {
		return imapErr.Text
	}
	return ""
}

// selectRW selects folder for read-write access and releases whatever
// was selected before. Every mutating command needs a selected mailbox;
// EXAMINE's read-only selection would make STORE fail (RFC 3501 §6.3.1).
func (c *Conn) selectRW(ctx context.Context, folder string) error {
	if _, err := waitCmd(ctx, c.client.Select(folder, nil)); err != nil {
		return fmt.Errorf("imapdrv: select %q for write: %w", folder, err)
	}
	c.selected = folder
	c.drainNotes()
	return nil
}

// releaseSelection drops the write selection so the next command starts
// clean — and so a DELETE/RENAME of the folder we just wrote to is not
// refused for being selected by this session (FR-M.12).
func (c *Conn) releaseSelection(ctx context.Context) {
	if err := c.Unselect(ctx); err != nil {
		c.log.Debug("imapdrv: unselect after write", "err", err)
	}
	c.selected = ""
}

// StoreFlags adds and/or removes IMAP flags on the given uids of folder
// (PLAN §7.1 keyword row). Both directions are separate commands; a
// caller that only needs one passes an empty slice for the other.
func (c *Conn) StoreFlags(ctx context.Context, folder string, uids []uint32, add, remove []string) error {
	if len(uids) == 0 || (len(add) == 0 && len(remove) == 0) {
		return nil
	}
	if err := c.selectRW(ctx, folder); err != nil {
		return err
	}
	defer c.releaseSelection(ctx)
	set := uidSet(uids)
	if len(add) > 0 {
		if err := waitVoid(ctx, c.client.StoreUID(set, flagsOf(add),
			&imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd})); err != nil {
			return fmt.Errorf("imapdrv: store +flags %q: %w", folder, err)
		}
	}
	if len(remove) > 0 {
		if err := waitVoid(ctx, c.client.StoreUID(set, flagsOf(remove),
			&imapclient.StoreOptions{Op: imapclient.StoreFlagsRemove})); err != nil {
			return fmt.Errorf("imapdrv: store -flags %q: %w", folder, err)
		}
	}
	return nil
}

// CopyResult is what a COPY or MOVE reported: the source→destination
// uid pairs (empty when the server sent no COPYUID, RFC 4315 §3) and
// the destination's UIDVALIDITY, which the local commit needs to key
// the new uid mapping.
type CopyResult struct {
	DestUIDs        map[uint32]uint32
	DestUIDValidity uint32
}

// CopyUIDs copies uids from one folder to another. An empty DestUIDs
// with no error means the copy happened but the server sent no COPYUID
// (no UIDPLUS): the destination uid must come from the next sync pass
// instead — a documented degradation, not a failure.
func (c *Conn) CopyUIDs(ctx context.Context, from, to string, uids []uint32) (CopyResult, error) {
	if len(uids) == 0 {
		return CopyResult{}, nil
	}
	if err := c.selectRW(ctx, from); err != nil {
		return CopyResult{}, err
	}
	defer c.releaseSelection(ctx)
	data, err := waitCmd(ctx, c.client.CopyUID(uidSet(uids), to, nil))
	if err != nil {
		return CopyResult{}, fmt.Errorf("imapdrv: copy %q → %q: %w", from, to, err)
	}
	res := CopyResult{DestUIDs: uidPairs(data)}
	if data != nil {
		res.DestUIDValidity = data.UIDValidity
	}
	return res, nil
}

// MoveUIDs moves uids from one folder to another (RFC 6851), returning
// the same mapping CopyUIDs does. Native MOVE is used when the server
// advertises it; otherwise the library's documented emulation runs
// (COPY + STORE \Deleted + EXPUNGE), which is not atomic — the caller
// learns about that only through the log line here, and the local commit
// that follows is driven by the mapping, never by hope.
func (c *Conn) MoveUIDs(ctx context.Context, from, to string, uids []uint32) (CopyResult, error) {
	if len(uids) == 0 {
		return CopyResult{}, nil
	}
	if err := c.selectRW(ctx, from); err != nil {
		return CopyResult{}, err
	}
	defer c.releaseSelection(ctx)
	data, err := c.client.MoveUID(ctx, uidSet(uids), to, &imapclient.MoveOptions{
		AllowNonAtomicFallback: true,
	})
	if err != nil {
		return CopyResult{}, fmt.Errorf("imapdrv: move %q → %q: %w", from, to, err)
	}
	if data.RespCode == throttledOK {
		return CopyResult{}, ErrThrottled
	}
	if data.ExpungedEveryDeletedMessage {
		c.log.Warn("imapdrv: move fell back to bare EXPUNGE (no MOVE, no UIDPLUS)",
			"from", from, "to", to)
	}
	res := CopyResult{DestUIDs: uidPairs(&data.UIDPlus)}
	if data.UIDPlus.HasUIDs {
		res.DestUIDValidity = data.UIDPlus.UIDValidity
	}
	return res, nil
}

// StoreGmLabels adds and/or removes Gmail labels (X-GM-EXT-1, FR-S.10)
// on the given uids of folder. Labels are the exact strings Gmail uses:
// system labels in flag form ("\Inbox"), user labels as the mailbox
// names LIST reports. Label changes are visible in every folder the
// message lives in, so one command per direction covers the whole
// account — this is why the Gmail write path never copies to change
// membership. The .SILENT form skips the untagged FETCH echo; the next
// sync pass re-reads the label list anyway.
func (c *Conn) StoreGmLabels(ctx context.Context, folder string, uids []uint32, add, remove []string) error {
	if len(uids) == 0 || (len(add) == 0 && len(remove) == 0) {
		return nil
	}
	if err := c.selectRW(ctx, folder); err != nil {
		return err
	}
	defer c.releaseSelection(ctx)
	set := uidSet(uids)
	if len(add) > 0 {
		if err := waitVoid(ctx, c.client.StoreUIDGmailLabels(set, imapclient.StoreFlagsAdd, add,
			&imapclient.StoreOptions{Silent: true})); err != nil {
			return fmt.Errorf("imapdrv: store +x-gm-labels %q: %w", folder, err)
		}
	}
	if len(remove) > 0 {
		if err := waitVoid(ctx, c.client.StoreUIDGmailLabels(set, imapclient.StoreFlagsRemove, remove,
			&imapclient.StoreOptions{Silent: true})); err != nil {
			return fmt.Errorf("imapdrv: store -x-gm-labels %q: %w", folder, err)
		}
	}
	return nil
}

// ExpungeUIDs permanently removes uids from folder: STORE \Deleted,
// then UID EXPUNGE where UIDPLUS is advertised (FR-M.10). Without
// UIDPLUS the only portable option is a bare EXPUNGE, which also removes
// messages other clients marked \Deleted — RFC 4315 §2.1 exists for
// exactly this, so it is logged loudly rather than done quietly.
func (c *Conn) ExpungeUIDs(ctx context.Context, folder string, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	if err := c.selectRW(ctx, folder); err != nil {
		return err
	}
	defer c.releaseSelection(ctx)
	set := uidSet(uids)
	if err := c.client.StoreUID(set, []imap.Flag{imap.FlagDeleted},
		&imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd}).Wait(ctx); err != nil {
		return fmt.Errorf("imapdrv: mark \\Deleted %q: %w", folder, err)
	}
	if c.caps()["UIDPLUS"] {
		if err := waitVoid(ctx, c.client.UIDExpunge(set, nil)); err != nil {
			return fmt.Errorf("imapdrv: uid expunge %q: %w", folder, err)
		}
		return nil
	}
	c.log.Warn("imapdrv: expunge without UIDPLUS removes every \\Deleted message in the folder",
		"folder", folder)
	if err := waitVoid(ctx, c.client.Expunge(nil)); err != nil {
		return fmt.Errorf("imapdrv: expunge %q: %w", folder, err)
	}
	return nil
}

// AppendMessage stores raw RFC 5322 bytes in folder with the given
// flags and returns the uid the server assigned (0 when it sent no
// APPENDUID, so the caller leaves the mapping to the next sync pass).
// internalDate is optional; when nil the server uses arrival time.
func (c *Conn) AppendMessage(ctx context.Context, folder string, raw []byte, flags []string, internalDate *time.Time) (uid, uidValidity uint32, err error) {
	if len(raw) == 0 {
		return 0, 0, fmt.Errorf("imapdrv: append to %q: empty message", folder)
	}
	opts := &imapclient.AppendOptions{Flags: flagsOf(flags), InternalDate: internalDate}
	data, err := waitCmd(ctx, c.client.Append(ctx, folder, opts, int64(len(raw)), bytes.NewReader(raw)))
	if err != nil {
		return 0, 0, fmt.Errorf("imapdrv: append %q: %w", folder, err)
	}
	if !data.HasUID {
		return 0, 0, nil
	}
	return uint32(data.UID), data.UIDValidity, nil
}

// CreateMailbox creates a folder (FR-M.12).
func (c *Conn) CreateMailbox(ctx context.Context, path string) error {
	if err := waitVoid(ctx, c.client.Create(path, nil)); err != nil {
		return fmt.Errorf("imapdrv: create %q: %w", path, err)
	}
	return nil
}

// RenameMailbox renames (or reparents) a folder. The server's refusal —
// typically a non-empty destination, or a hierarchy it will not break —
// is returned verbatim so the SetError description can quote it.
func (c *Conn) RenameMailbox(ctx context.Context, from, to string) error {
	if err := waitVoid(ctx, c.client.Rename(from, to, nil)); err != nil {
		return fmt.Errorf("imapdrv: rename %q → %q: %w", from, to, err)
	}
	return nil
}

// DeleteMailbox deletes a folder; a server that refuses (non-empty, or
// selected elsewhere) surfaces its own message through the error.
func (c *Conn) DeleteMailbox(ctx context.Context, path string) error {
	if err := waitVoid(ctx, c.client.Delete(path, nil)); err != nil {
		return fmt.Errorf("imapdrv: delete %q: %w", path, err)
	}
	return nil
}

// Ping proves the connection is still alive (a write session probes
// before its first command so a socket the server dropped hours ago
// fails before a COPY can be half-applied).
func (c *Conn) Ping(ctx context.Context) error {
	return waitVoid(ctx, c.client.Noop(nil))
}

// SupportsMove reports RFC 6851 availability (the caller prefers a
// native MOVE over copy-plus-expunge, PLAN §7.1).
func (c *Conn) SupportsMove() bool { return c.caps()["MOVE"] }

// SupportsUIDPlus reports RFC 4315 availability (UID EXPUNGE, COPYUID,
// APPENDUID — the precision half of FR-M.9/FR-M.10).
func (c *Conn) SupportsUIDPlus() bool { return c.caps()["UIDPLUS"] }

// HierarchyDelim returns the personal namespace's hierarchy separator,
// which Mailbox/set needs to build a child path from parentId + name
// (PLAN §7.1 Mailbox/set row). NAMESPACE is the cheap answer; a server
// without it still answers LIST "" "" (RFC 3501 §6.3.8).
func (c *Conn) HierarchyDelim(ctx context.Context) (rune, error) {
	if _, delim, err := c.Namespace(ctx); err == nil && delim != 0 {
		return delim, nil
	}
	data, err := c.client.ListMailboxes(ctx, "", "", &imapclient.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("imapdrv: hierarchy delimiter: %w", err)
	}
	for _, d := range data {
		if d.Delimiter != 0 {
			return d.Delimiter, nil
		}
	}
	return 0, fmt.Errorf("imapdrv: server reports no hierarchy delimiter")
}

// uidPairs flattens a COPYUID response into a source→destination uid
// map; nil when the server sent none.
func uidPairs(data *imap.CopyData) map[uint32]uint32 {
	if data == nil || !data.HasUIDs {
		return nil
	}
	src := expandUIDSet(data.SourceUIDs)
	dst := expandUIDSet(data.DestinationUIDs)
	if len(src) == 0 || len(src) != len(dst) {
		return nil
	}
	out := make(map[uint32]uint32, len(src))
	for i := range src {
		out[src[i]] = dst[i]
	}
	return out
}

// flagsOf converts flag names to the library's type.
func flagsOf(names []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(names))
	for _, n := range names {
		out = append(out, imap.Flag(n))
	}
	return out
}

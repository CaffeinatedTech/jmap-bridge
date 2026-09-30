package imapdrv

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// Anchor is a folder's cached QRESYNC resync point (FR-S.5 tier 1). A
// nil anchor (or zero UIDValidity) selects without parameters — the
// first pass against a folder, where a full backfill follows anyway.
type Anchor struct {
	UIDValidity uint32
	ModSeq      uint64
}

// FlagChange is one message's flag state as the server reported it.
type FlagChange struct {
	UID    uint32
	Flags  []string
	ModSeq uint64
}

// Selection is what EXAMINE/SELECT reported for a folder (FR-S.5).
type Selection struct {
	Status FolderStatus
	// Vanished lists uids expunged while we were away (QRESYNC).
	Vanished []uint32
	// FlagChanges are the flag deltas the QRESYNC select replayed.
	FlagChanges []FlagChange
	// ResyncRejected means the server ignored the QRESYNC anchor: treat
	// every cached uid for this folder as suspect and rescan (RFC 7162
	// §3.2.5 — the failure that arrives as silence).
	ResyncRejected bool
	// NoModSeq reports the NOMODSEQ response code: no CONDSTORE sync on
	// this mailbox (RFC 7162 §3.1.2.2).
	NoModSeq bool
	// UIDValidityChanged reports that UIDValidity differs from what this
	// client last observed for the folder.
	UIDValidityChanged bool
}

// HeaderMsg is one message's header-level summary: the ENVELOPE,
// BODYSTRUCTURE, flags, size and internal date — everything backfill
// needs, never a body (FR-S.3, golden rule 2). On X-GM-EXT-1 servers it
// also carries the Gmail label list and thread id (FR-S.10).
type HeaderMsg struct {
	UID          uint32
	Flags        []string
	ModSeq       uint64
	InternalDate time.Time
	Size         int64
	Envelope     convert.Envelope
	Structure    *convert.Part
	Labels       []string
	Thrid        uint64
}

// Examine selects the folder read-only at the connection's current
// tier, passing the QRESYNC anchor when there is one. A tier-specific
// rejection demotes the session and retries once at the next tier
// (FR-S.2 downgrade without restart).
func (c *Conn) Examine(ctx context.Context, folder string, anchor *Anchor) (Selection, error) {
	attempt := c.tier
	for {
		sel, err := c.examineOnce(ctx, folder, anchor, attempt)
		if err == nil {
			c.selected = folder
			c.drainNotes()
			return sel, nil
		}
		if !c.downgradeAfterError(err, attempt) {
			return Selection{}, fmt.Errorf("imapdrv: examine %q: %w", folder, err)
		}
		attempt = c.tier
	}
}

func (c *Conn) examineOnce(ctx context.Context, folder string, anchor *Anchor, tier Tier) (Selection, error) {
	switch tier {
	case TierQResync:
		opts := &imapclient.SyncSelectOptions{CondStore: true}
		if anchor != nil && anchor.UIDValidity != 0 {
			opts.QResync = &imapclient.QResyncOptions{
				UIDValidity: anchor.UIDValidity,
				ModSeq:      anchor.ModSeq,
			}
		}
		st, err := waitCmd(ctx, c.client.ExamineSync(folder, opts))
		if err != nil {
			return Selection{}, err
		}
		sel := Selection{
			Status:             statusFrom(&st.Status.MailboxStatus),
			ResyncRejected:     st.ResyncRejected,
			NoModSeq:           st.NoModSeq,
			UIDValidityChanged: st.Status.UIDValidityChanged,
		}
		for _, v := range st.Vanished {
			sel.Vanished = append(sel.Vanished, expandUIDSet(v.UIDs)...)
		}
		for _, f := range st.Fetched {
			if ch, ok := flagChangeOf(f); ok {
				sel.FlagChanges = append(sel.FlagChanges, ch)
			}
		}
		return sel, nil
	case TierCondStore:
		// Same command shape as QRESYNC without the anchor: SELECT
		// (CONDSTORE) gives UIDNEXT/HIGHESTMODSEQ; deltas come from
		// ChangesSince (FR-S.5 tier 2).
		st, err := waitCmd(ctx, c.client.ExamineSync(folder, &imapclient.SyncSelectOptions{CondStore: true}))
		if err != nil {
			return Selection{}, err
		}
		return Selection{Status: statusFrom(&st.Status.MailboxStatus), UIDValidityChanged: st.Status.UIDValidityChanged}, nil
	default:
		st, err := waitCmd(ctx, c.client.Examine(folder, nil))
		if err != nil {
			return Selection{}, err
		}
		return Selection{Status: statusFrom(&st.MailboxStatus), UIDValidityChanged: st.UIDValidityChanged}, nil
	}
}

// Unselect releases the connection's selection without expunging
// (UNSELECT, RFC 3691; CLOSE would expunge other clients' \Deleted
// mail, so it is never used on a read path). Servers without UNSELECT
// keep the selection — harmless, and logged once.
func (c *Conn) Unselect(ctx context.Context) error {
	if !c.caps()["UNSELECT"] && !c.caps()["IMAP4REV2"] {
		return nil
	}
	return waitVoid(ctx, c.client.Unselect(nil))
}

// FetchHeaders fetches header-level records for the given uids of the
// selected folder (FR-S.3 backfill, FR-S.5 new-message details).
func (c *Conn) FetchHeaders(ctx context.Context, uids []uint32) ([]HeaderMsg, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	items := c.headerItems()
	cmd := c.client.FetchUID(uidSet(uids), nil, items...)
	return drainHeaderFetch(ctx, cmd)
}

// FetchHeadersSeq is FetchHeaders by sequence numbers. Gmail backfill
// walks 1..EXISTS: the server's uid spaces are sparse (a 73k-uidnext
// folder can hold 31 messages), and sweeping the expunged desert trips
// Gmail's response slow-walking, while sequence ranges only ever touch
// live messages. Each response still carries the message's UID, so the
// caller's records are uid-keyed as usual (FR-S.10).
func (c *Conn) FetchHeadersSeq(ctx context.Context, seqs []uint32) ([]HeaderMsg, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	items := c.headerItems()
	cmd := c.client.Fetch(seqSet(seqs), nil, items...)
	return drainHeaderFetch(ctx, cmd)
}

// headerItems is the header-level fetch item set: never a body (golden
// rule 2), plus modseq where CONDSTORE is on and the Gmail extensions
// when X-GM-EXT-1 is advertised (FR-S.10).
func (c *Conn) headerItems() []imap.FetchItem {
	items := []imap.FetchItem{
		imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemInternalDate,
		imap.FetchItemRFC822Size, imap.FetchItemEnvelope,
		&imap.FetchItemBodyStructure{Extended: true},
	}
	if c.tier != TierBaseline {
		items = append(items, imap.FetchItemModSeq)
	}
	if c.GmailExt() {
		// Open-ended item names are first-class in the request encoder;
		// the responses come back as FetchDataRaw and are parsed in
		// headerMsgOf.
		items = append(items, imap.FetchItemKeyword("X-GM-LABELS"), imap.FetchItemKeyword("X-GM-THRID"))
	}
	return items
}

func drainHeaderFetch(ctx context.Context, cmd *imapclient.FetchCommand) ([]HeaderMsg, error) {
	var out []HeaderMsg
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			// The tagged completion has run by EOF: a [THROTTLED]-tagged
			// OK means the fetch did not complete, and a partial batch
			// must never be reported as the folder's whole truth.
			if cmd.RespCode() == throttledOK {
				return nil, ErrThrottled
			}
			return out, nil
		}
		if err != nil {
			if IsThrottled(err) {
				return nil, ErrThrottled
			}
			return nil, fmt.Errorf("imapdrv: fetch headers: %w", err)
		}
		msg, ok := headerMsgOf(data)
		if ok {
			out = append(out, msg)
		}
	}
}

// ChangesSince reports flag changes and expunges since a mod-sequence
// on the selected folder (FR-S.5 tier 2; tier 1 passes the VANISHED
// modifier). New messages are included as flag-only entries — the
// caller recognises unknown uids and fetches their headers.
func (c *Conn) ChangesSince(ctx context.Context, modseq uint64) (changed []FlagChange, vanished []uint32, err error) {
	if c.tier == TierBaseline {
		return nil, nil, fmt.Errorf("imapdrv: ChangesSince requires CONDSTORE")
	}
	opts := &imapclient.SyncFetchOptions{ChangedSince: modseq, ReportVanished: c.tier == TierQResync}
	cmd := c.client.FetchUIDSync(imap.UIDSetRange(1, 0), opts,
		imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemModSeq)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			if cmd.RespCode() == throttledOK {
				return nil, nil, ErrThrottled
			}
			break
		}
		if err != nil {
			if IsThrottled(err) {
				return nil, nil, ErrThrottled
			}
			return nil, nil, fmt.Errorf("imapdrv: changes since %d: %w", modseq, err)
		}
		if ch, ok := flagChangeOf(data); ok {
			changed = append(changed, ch)
		}
	}
	for _, v := range cmd.Vanished() {
		vanished = append(vanished, expandUIDSet(v.UIDs)...)
	}
	return changed, vanished, nil
}

// AllFlags refetches uid+flags for every message of the selected
// folder — the baseline tier's only way to notice a flag flip
// (FR-S.5 tier 3), and cheap enough against a compressed link.
func (c *Conn) AllFlags(ctx context.Context) ([]FlagChange, error) {
	cmd := c.client.FetchUID(imap.UIDSetRange(1, 0), nil, imap.FetchItemUID, imap.FetchItemFlags)
	var out []FlagChange
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			if cmd.RespCode() == throttledOK {
				return nil, ErrThrottled
			}
			return out, nil
		}
		if err != nil {
			if IsThrottled(err) {
				return nil, ErrThrottled
			}
			return nil, fmt.Errorf("imapdrv: all flags: %w", err)
		}
		if ch, ok := flagChangeOf(data); ok {
			out = append(out, ch)
		}
	}
}

// UIDs lists every uid of the selected folder (expunge detection for
// tiers 2 and 3: a uid the store knows that is absent here has been
// expunged). A [THROTTLED]-tagged completion means the list is not the
// folder's truth — returning it as if it were would tombstone live
// messages (the cache lying at scale).
func (c *Conn) UIDs(ctx context.Context) ([]uint32, error) {
	cmd := c.client.FetchUID(imap.UIDSetRange(1, 0), nil, imap.FetchItemUID)
	var out []uint32
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			if cmd.RespCode() == throttledOK {
				return nil, ErrThrottled
			}
			return out, nil
		}
		if err != nil {
			if IsThrottled(err) {
				return nil, ErrThrottled
			}
			return nil, fmt.Errorf("imapdrv: uid list: %w", err)
		}
		if uid, ok := uidOf(data); ok {
			out = append(out, uid)
		}
	}
}

// FetchPreviews returns preview text per uid: the server's PREVIEW
// (RFC 8970) when advertised, otherwise a 4 KiB PEEK partial of the
// message (PLAN §5).
func (c *Conn) FetchPreviews(ctx context.Context, uids []uint32) (map[uint32]string, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	out := make(map[uint32]string, len(uids))
	if c.SupportsPREVIEW() {
		cmd := c.client.FetchUID(uidSet(uids), nil, imap.FetchItemUID, &imap.FetchItemPreview{})
		for {
			data, err := cmd.Next(ctx)
			if err == io.EOF {
				if cmd.RespCode() == throttledOK {
					return nil, ErrThrottled
				}
				return out, nil
			}
			if err != nil {
				if IsThrottled(err) {
					return nil, ErrThrottled
				}
				return nil, fmt.Errorf("imapdrv: preview: %w", err)
			}
			uid, ok := uidOf(data)
			if !ok {
				continue
			}
			if text, ok := previewOf(data); ok && text != nil {
				out[uid] = *text
			}
		}
	}
	cmd := c.client.FetchUID(uidSet(uids), nil, imap.FetchItemUID, &imap.FetchItemBodySection{
		Peek:    true,
		Partial: &imap.SectionPartial{Size: 4096},
	})
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			if cmd.RespCode() == throttledOK {
				return nil, ErrThrottled
			}
			return out, nil
		}
		if err != nil {
			if IsThrottled(err) {
				return nil, ErrThrottled
			}
			return nil, fmt.Errorf("imapdrv: partial preview: %w", err)
		}
		uid, ok := uidOf(data)
		if !ok {
			continue
		}
		raw, ok := sectionBytes(data)
		if !ok {
			continue
		}
		out[uid] = convert.PreviewFromPartial(raw)
	}
}

// FetchBody downloads one full message with BODY.PEEK — never BODY[], so
// reading through the bridge never sets \Seen behind the client's back
// (FR-M.8: flags are the client's business).
func (c *Conn) FetchBody(ctx context.Context, uid uint32) ([]byte, error) {
	cmd := c.client.FetchUID(uidSet([]uint32{uid}), nil, &imap.FetchItemBodySection{Peek: true})
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			return nil, fmt.Errorf("imapdrv: no body for uid %d", uid)
		}
		if err != nil {
			return nil, fmt.Errorf("imapdrv: fetch body %d: %w", uid, err)
		}
		raw, ok := sectionBytes(data)
		if !ok {
			continue
		}
		return raw, nil
	}
}

// FetchBodies downloads full messages for one folder in one batch:
// one EXAMINE, then a single-UID BODY.PEEK fetch per message while the
// folder stays selected, one Unselect at the end. The per-message
// select/fetch/unselect round trip is what makes hydration collapse on
// large folders — the server's select-time bookkeeping (maildir
// directory rescan, index refresh) is per EXAMINE, not per FETCH, so
// amortising it is the difference between a hydration rate clients can
// feel and one they can't (NFR-1's ≥ 15 msg/s floor assumes it).
// Responses still carry the full message with BODY.PEEK — never
// BODY[], so reading through the bridge never sets \Seen behind the
// client's back (FR-M.8).
func (c *Conn) FetchBodies(ctx context.Context, folder string, uids []uint32) (map[uint32][]byte, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if _, err := c.Examine(ctx, folder, nil); err != nil {
		return nil, fmt.Errorf("imapdrv: examine for bodies: %w", err)
	}
	defer func() { _ = c.Unselect(context.Background()) }()
	out := make(map[uint32][]byte, len(uids))
	for _, uid := range uids {
		raw, err := c.FetchBody(ctx, uid)
		if err != nil {
			return nil, fmt.Errorf("imapdrv: fetch body %d: %w", uid, err)
		}
		out[uid] = raw
	}
	return out, nil
}

// --- response mapping ---

func statusFrom(st *imap.MailboxStatus) FolderStatus {
	if st == nil {
		return FolderStatus{}
	}
	// SELECT does not carry UNSEEN; Status() fills it where needed.
	return FolderStatus{
		UIDValidity:   st.UIDValidity,
		UIDNext:       uint64(st.UIDNext),
		HighestModSeq: st.HighestModSeq,
		Messages:      st.NumMessages,
	}
}

// flagChangeOf extracts uid/flags/modseq from a FETCH response.
func flagChangeOf(data *imap.FetchMessageData) (FlagChange, bool) {
	uid, ok := uidOf(data)
	if !ok {
		return FlagChange{}, false
	}
	ch := FlagChange{UID: uid}
	if v, ok := data.Items[imap.FetchDataKey("FLAGS")]; ok && len(v) > 0 {
		if flags, ok := v[0].(imap.FetchDataFlags); ok {
			for _, f := range flags {
				ch.Flags = append(ch.Flags, string(f))
			}
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("MODSEQ")]; ok && len(v) > 0 {
		if m, ok := v[0].(imap.FetchDataModSeq); ok {
			ch.ModSeq = uint64(m)
		}
	}
	return ch, true
}

func uidOf(data *imap.FetchMessageData) (uint32, bool) {
	v, ok := data.Items[imap.FetchDataKey("UID")]
	if !ok || len(v) == 0 {
		return 0, false
	}
	uid, ok := v[0].(imap.FetchDataUID)
	return uint32(uid), ok
}

// headerMsgOf maps one FETCH response into the driver's header record.
func headerMsgOf(data *imap.FetchMessageData) (HeaderMsg, bool) {
	uid, ok := uidOf(data)
	if !ok {
		return HeaderMsg{}, false
	}
	msg := HeaderMsg{UID: uid}
	if v, ok := data.Items[imap.FetchDataKey("FLAGS")]; ok && len(v) > 0 {
		if flags, ok := v[0].(imap.FetchDataFlags); ok {
			for _, f := range flags {
				msg.Flags = append(msg.Flags, string(f))
			}
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("MODSEQ")]; ok && len(v) > 0 {
		if m, ok := v[0].(imap.FetchDataModSeq); ok {
			msg.ModSeq = uint64(m)
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("INTERNALDATE")]; ok && len(v) > 0 {
		if d, ok := v[0].(*imap.FetchDataInternalDate); ok {
			msg.InternalDate = d.Time
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("RFC822.SIZE")]; ok && len(v) > 0 {
		if sz, ok := v[0].(imap.FetchDataRFC822Size); ok {
			msg.Size = int64(sz)
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("ENVELOPE")]; ok && len(v) > 0 {
		if env, ok := v[0].(*imap.FetchDataEnvelope); ok {
			msg.Envelope = mapEnvelope(env.Envelope)
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("BODYSTRUCTURE")]; ok && len(v) > 0 {
		if bs, ok := v[0].(*imap.FetchDataBodyStructure); ok {
			msg.Structure = mapStructure(bs.BodyStructure)
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("X-GM-LABELS")]; ok && len(v) > 0 {
		if raw, ok := v[0].(*imap.FetchDataRaw); ok {
			msg.Labels = parseGmLabels(rawValue(raw))
		}
	}
	if v, ok := data.Items[imap.FetchDataKey("X-GM-THRID")]; ok && len(v) > 0 {
		if raw, ok := v[0].(*imap.FetchDataRaw); ok {
			msg.Thrid = parseGmThrid(rawValue(raw))
		}
	}
	return msg, true
}

// rawValue drains a preserved response value's bytes; the library
// buffers, so a single Read suffices for these bounded values (label
// lists and thread ids never approach the in-memory capture limit).
func rawValue(raw *imap.FetchDataRaw) string {
	if raw == nil || raw.Reader == nil {
		return ""
	}
	buf, err := io.ReadAll(raw.Reader)
	if err != nil {
		return ""
	}
	return string(buf)
}

// parseGmLabels decodes the wire form of an X-GM-LABELS value: a
// parenthesised list of system labels in flag form ("\Inbox") and user
// labels as astrings ("my receipts"). Values are kept exactly as sent —
// the leading backslash included — because that is the form label writes
// take.
func parseGmLabels(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return nil
	}
	s = s[1 : len(s)-1]
	var out []string
	for i := 0; i < len(s); {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			// A quoted label: honour backslash escapes.
			i++
			var b strings.Builder
			closed := false
			for ; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					b.WriteByte(s[i])
					continue
				}
				if s[i] == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(s[i])
			}
			if !closed {
				return nil // malformed: drop the whole list rather than guess
			}
			out = append(out, b.String())
			continue
		}
		// An atom or flag-form label, up to the next space or paren.
		start := i
		for i < len(s) && s[i] != ' ' && s[i] != '(' && s[i] != ')' {
			i++
		}
		if i == start {
			return nil // a stray paren: malformed
		}
		out = append(out, s[start:i])
	}
	return out
}

// parseGmThrid decodes X-GM-THRID: a 64-bit unsigned number Gmail
// reports as plain digits.
func parseGmThrid(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func previewOf(data *imap.FetchMessageData) (*string, bool) {
	v, ok := data.Items[imap.FetchDataKey("PREVIEW")]
	if !ok || len(v) == 0 {
		return nil, false
	}
	p, ok := v[0].(*imap.FetchDataPreview)
	if !ok {
		return nil, false
	}
	return p.Text, true
}

// sectionBytes drains a BODY[...] section's literal.
func sectionBytes(data *imap.FetchMessageData) ([]byte, bool) {
	for _, vals := range data.Items {
		for _, v := range vals {
			if sec, ok := v.(*imap.FetchDataBodySection); ok && sec.Literal != nil {
				raw, err := io.ReadAll(sec.Literal)
				if err != nil {
					return nil, false
				}
				return raw, true
			}
		}
	}
	return nil, false
}

// mapEnvelope converts the library's ENVELOPE into the neutral convert
// form; the raw Date string survives so Summary can parse what the
// server actually sent (including malformed dates).
func mapEnvelope(env *imap.Envelope) convert.Envelope {
	if env == nil {
		return convert.Envelope{}
	}
	date := env.RawDate
	if date == "" && !env.Date.IsZero() {
		date = env.Date.Format("Mon, 2 Jan 2006 15:04:05 -0700")
	}
	return convert.Envelope{
		Date:       date,
		Subject:    env.Subject,
		From:       mapAddresses(env.From),
		Sender:     mapAddresses(env.Sender),
		ReplyTo:    mapAddresses(env.ReplyTo),
		To:         mapAddresses(env.To),
		Cc:         mapAddresses(env.Cc),
		Bcc:        mapAddresses(env.Bcc),
		MessageID:  env.MessageID,
		InReplyTo:  strings.Join(env.InReplyTo, " "),
		References: "",
	}
}

func mapAddresses(in []imap.Address) []convert.Address {
	out := make([]convert.Address, 0, len(in))
	for _, a := range in {
		out = append(out, convert.Address{Name: a.Name, Mailbox: a.Mailbox, Host: a.Host})
	}
	return out
}

// mapStructure converts the library's BODYSTRUCTURE into the neutral
// convert tree. message/rfc822 parts become opaque leaves — the raw
// parse path treats them the same way, so partIds agree (FR-M.4).
func mapStructure(bs imap.BodyStructure) *convert.Part {
	switch s := bs.(type) {
	case *imap.BodyStructureMultiPart:
		p := &convert.Part{Type: "multipart", SubType: s.Subtype}
		if s.Extended != nil {
			p.Params = s.Extended.Params
			p.Disposition, p.DispositionParams = mapDisposition(s.Extended.Disp)
		}
		for _, child := range s.Children {
			p.Children = append(p.Children, mapStructure(child))
		}
		return p
	case *imap.BodyStructureSinglePart:
		p := &convert.Part{
			Type: s.Type, SubType: s.Subtype, Params: s.Params,
			ID: s.ID, Description: s.Description, Encoding: s.Encoding,
			Size: int64(s.Size),
		}
		if s.Text != nil {
			p.Lines = s.Text.NumLines
		}
		if s.Message != nil {
			p.Lines = s.Message.NumLines
		}
		if s.Extended != nil {
			p.Disposition, p.DispositionParams = mapDisposition(s.Extended.Disp)
		}
		return p
	default:
		// A future shape: keep it as an opaque leaf rather than
		// panicking (the library documents this stance).
		return &convert.Part{Type: "application", SubType: "octet-stream"}
	}
}

func mapDisposition(d *imap.BodyStructureDisposition) (string, map[string]string) {
	if d == nil {
		return "", nil
	}
	return d.Value, d.Params
}

// uidSet builds a compact UID set from a list, coalescing runs so a
// large backfill batch stays one short command line.
func uidSet(uids []uint32) imap.UIDSet {
	if len(uids) == 0 {
		return imap.UIDSet{}
	}
	sorted := append([]uint32(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var set imap.UIDSet
	i := 0
	for i < len(sorted) {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if i == j {
			set = append(set, imap.UIDSetNum(imap.UID(sorted[i]))...)
		} else {
			set = append(set, imap.UIDSetRange(imap.UID(sorted[i]), imap.UID(sorted[j]))...)
		}
		i = j + 1
	}
	return set
}

// expandUIDSet flattens a UID set into individual uids by parsing its
// wire form ("1:5,7") — the library keeps range bounds unexported.
func expandUIDSet(set imap.UIDSet) []uint32 {
	s := set.String()
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, ok := parseUIDRange(part)
		if !ok {
			continue
		}
		for v := lo; v <= hi; v++ {
			out = append(out, v)
		}
	}
	return out
}

func parseUIDRange(s string) (uint32, uint32, bool) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		lo, err1 := strconv.ParseUint(s[:i], 10, 32)
		hiStr := s[i+1:]
		if hiStr == "*" {
			// "*" only appears in query sets, never in responses the
			// library echoes; treat as single-sided.
			return uint32(lo), uint32(lo), true
		}
		hi, err2 := strconv.ParseUint(hiStr, 10, 32)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return uint32(lo), uint32(hi), true
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return uint32(v), uint32(v), true
}

// seqSet builds a compact sequence-number set from a list, mirroring
// uidSet's run coalescing.
func seqSet(seqs []uint32) imap.SeqSet {
	if len(seqs) == 0 {
		return imap.SeqSet{}
	}
	sorted := append([]uint32(nil), seqs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var set imap.SeqSet
	i := 0
	for i < len(sorted) {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if i == j {
			set = append(set, imap.SeqSetNum(imap.SeqNum(sorted[i]))...)
		} else {
			set = append(set, imap.SeqSetRange(imap.SeqNum(sorted[i]), imap.SeqNum(sorted[j]))...)
		}
		i = j + 1
	}
	return set
}

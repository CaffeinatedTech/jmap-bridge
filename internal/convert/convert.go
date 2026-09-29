// Package convert transforms mail between its wire representations and
// what the store keeps: IMAP ENVELOPE/BODYSTRUCTURE (or raw RFC 5322)
// in, JMAP summary records and body values out (PLAN §3). It is pure
// transformation — no network, no database — so its golden fixtures are
// the contract for both sync paths (AGENTS.md testing rules).
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// Address is one RFC 5322 mailbox as IMAP reported it.
type Address struct {
	Name    string
	Mailbox string
	Host    string
}

// Email renders the address as JMAP email ("mailbox@host").
func (a Address) Email() string {
	switch {
	case a.Mailbox == "":
		return a.Host
	case a.Host == "":
		return a.Mailbox
	default:
		return a.Mailbox + "@" + a.Host
	}
}

// Envelope is the driver-neutral header summary (IMAP ENVELOPE, RFC
// 3501 §7.4.2). imapdrv builds it from the client library's type so
// library types never leave that package (D-7).
type Envelope struct {
	Date       string // RFC 5322 Date, raw
	Subject    string
	From       []Address
	Sender     []Address
	ReplyTo    []Address
	To         []Address
	Cc         []Address
	Bcc        []Address
	MessageID  string // single msg-id, including angle brackets
	InReplyTo  string // possibly several msg-ids, whitespace separated
	References string // possibly several msg-ids, whitespace separated
}

// Part is the driver-neutral body structure (IMAP BODYSTRUCTURE).
type Part struct {
	Type              string
	SubType           string
	Params            map[string]string
	ID                string
	Description       string
	Encoding          string
	Size              int64
	Lines             int64
	Children          []*Part
	Disposition       string // BODYSTRUCTURE extension, "" when absent
	DispositionParams map[string]string

	// PartID is filled by BuildStructure; containers stay empty per
	// RFC 8621 (a multipart root carries no partId).
	PartID string
}

// Media returns the full lowercased media type ("text/plain").
func (p *Part) Media() string {
	if p.SubType == "" {
		return strings.ToLower(p.Type)
	}
	return strings.ToLower(p.Type + "/" + p.SubType)
}

// isMultipart reports whether the node is a container. Only
// multipart/* counts: a message/rfc822 part's nested structure is
// deliberately not walked, because the raw-parse path treats attached
// messages as opaque leaves too (they travel as their own blob, RFC
// 8621 §4.1) — the two paths must number parts identically.
func (p *Part) isMultipart() bool { return strings.EqualFold(p.Type, "multipart") }

// Summary converts an ENVELOPE + BODYSTRUCTURE pair into the store's
// header-level record. It never touches a body (golden rule 2): previews
// and body values come later, from the lazy paths.
func Summary(env Envelope, root *Part, receivedAt time.Time, size int64) store.MessageRec {
	subject := decodeWords(env.Subject)
	headers := map[string]any{}
	if subject != "" {
		headers["subject"] = subject
	}
	for key, addrs := range map[string][]Address{
		"from": env.From, "to": env.To, "cc": env.Cc,
		"bcc": env.Bcc, "replyTo": env.ReplyTo,
	} {
		if j := jmapAddresses(addrs); len(j) > 0 {
			headers[key] = j
		}
	}
	hj, err := json.Marshal(headers)
	if err != nil {
		hj = []byte("{}")
	}

	structure := "{}"
	hasAttachment := false
	if root != nil {
		BuildStructure(root, "")
		if raw, err := json.Marshal(structureObject(root)); err == nil {
			structure = string(raw)
		}
		hasAttachment = hasAnyAttachment(root)
	}

	var sentAt *time.Time
	if t, err := mail.ParseDate(env.Date); err == nil {
		sentAt = &t
	}

	return store.MessageRec{
		ReceivedAt:  receivedAt,
		Size:        size,
		HeadersJSON: string(hj),
		Subject:     subject,
		SubjectL:    strings.ToLower(subject),
		FromL:       strings.ToLower(allAddresses(env.From, env.Sender)),
		ToL:         strings.ToLower(allAddresses(env.To, env.Cc)),
		MessageIDs:  splitMsgIDs(env.MessageID),
		InReplyTo:   splitMsgIDs(env.InReplyTo),
		References:  splitMsgIDs(env.References),
		SentAt:      sentAt,
		Structure:   structure,

		HasAttachment: hasAttachment,
	}
}

// SummaryFromRaw builds the store's header-level record from a complete
// RFC 5322 message the bridge already holds in memory. It is how a
// submission files its Sent copy (PLAN §7.2): the record is derived
// from exactly the bytes that were sent, instead of from an IMAP
// ENVELOPE the next sync pass would have to deliver first.
//
// A header the parser cannot read is left out of the record rather than
// failing it: a Sent copy must exist even for a message with an odd
// address header, and the summary fields the query mirrors feed are the
// ones that parsed.
func SummaryFromRaw(raw []byte, receivedAt time.Time) (store.MessageRec, error) {
	mr, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return store.MessageRec{}, fmt.Errorf("convert: read message headers: %w", err)
	}
	h := mr.Header
	subject := decodeWords(h.Get("Subject"))
	from := headerAddresses(h, "From")
	to := headerAddresses(h, "To")
	cc := headerAddresses(h, "Cc")

	headers := map[string]any{}
	if subject != "" {
		headers["subject"] = subject
	}
	for key, addrs := range map[string][]Address{
		"from": from, "to": to, "cc": cc,
		"bcc": headerAddresses(h, "Bcc"), "replyTo": headerAddresses(h, "Reply-To"),
	} {
		if j := jmapAddresses(addrs); len(j) > 0 {
			headers[key] = j
		}
	}
	hj, err := json.Marshal(headers)
	if err != nil {
		return store.MessageRec{}, fmt.Errorf("convert: encode headers: %w", err)
	}
	structure, err := structureOfRaw(raw)
	if err != nil {
		return store.MessageRec{}, fmt.Errorf("convert: structure of raw message: %w", err)
	}
	body, _ := ParseBody(raw)

	var sentAt *time.Time
	if t, err := mail.ParseDate(h.Get("Date")); err == nil {
		sentAt = &t
	}
	return store.MessageRec{
		ReceivedAt:  receivedAt,
		Size:        int64(len(raw)),
		HeadersJSON: string(hj),
		Subject:     subject,
		SubjectL:    strings.ToLower(subject),
		FromL:       strings.ToLower(allAddresses(from)),
		ToL:         strings.ToLower(allAddresses(to, cc)),
		MessageIDs:  splitMsgIDs(h.Get("Message-Id")),
		InReplyTo:   splitMsgIDs(h.Get("In-Reply-To")),
		References:  splitMsgIDs(h.Get("References")),
		SentAt:      sentAt,
		Structure:   structure,
		// The same test BuildDraft makes: the parsed body decides
		// whether the message carries attachments (FR-M.4 and the
		// summary's hasAttachment can never disagree).
		HasAttachment: len(body.Attachments) > 0,
		Preview:       body.Preview,
	}, nil
}

// headerAddresses reads one address header, decoding RFC 2047 words in
// the display names. An unparseable header reads as absent.
func headerAddresses(h mail.Header, key string) []Address {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		return nil
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		mailbox, host := "", a.Address
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			mailbox, host = a.Address[:i], a.Address[i+1:]
		}
		out = append(out, Address{Name: a.Name, Mailbox: mailbox, Host: host})
	}
	return out
}

// BuildStructure numbers a BODYSTRUCTURE tree with JMAP partIds using
// the MIME section scheme ("1", "2", "2.1", …): leaves get ids,
// multipart containers do not (RFC 8621 §4.1). Both sync paths call it
// — the envelope path on the reported structure, the raw path while
// walking the parsed message — so partIds line up between them.
func BuildStructure(p *Part, section string) {
	if p == nil {
		return
	}
	if p.isMultipart() {
		for i, child := range p.Children {
			label := strconv.Itoa(i + 1)
			if section != "" {
				label = section + "." + label
			}
			BuildStructure(child, label)
		}
		return
	}
	if section == "" {
		section = "1"
	}
	p.PartID = section
}

// structureObject renders a Part as a JMAP EmailBodyPart object.
func structureObject(p *Part) map[string]any {
	obj := map[string]any{"type": p.Media()}
	if p.PartID != "" {
		obj["partId"] = p.PartID
	}
	if cs := p.Params["charset"]; cs != "" && strings.HasPrefix(strings.ToLower(p.Type), "text/") {
		obj["charset"] = strings.ToLower(cs)
	}
	if p.Size > 0 {
		obj["size"] = p.Size
	}
	if name := decodeParam(firstNonEmpty(p.DispositionParams["filename"], p.Params["filename"])); name != "" {
		obj["name"] = name
	}
	if p.Disposition != "" {
		obj["disposition"] = strings.ToLower(p.Disposition)
	}
	if p.isMultipart() && len(p.Children) > 0 {
		subs := make([]any, 0, len(p.Children))
		for _, c := range p.Children {
			subs = append(subs, structureObject(c))
		}
		obj["subParts"] = subs
	}
	return obj
}

// hasAnyAttachment walks the tree for a part a client should offer as
// downloadable (RFC 8621 §4.1: attachments that are not inline).
func hasAnyAttachment(p *Part) bool {
	if p == nil {
		return false
	}
	if p.isMultipart() {
		for _, c := range p.Children {
			if hasAnyAttachment(c) {
				return true
			}
		}
		return false
	}
	if isAttachmentPart(p.Media(), p.Disposition) && !strings.EqualFold(p.Disposition, "inline") {
		return true
	}
	return false
}

// isAttachmentPart is the classification both sync paths share: a part
// is an attachment when it is explicitly marked so, or when it is not a
// body representation (text/plain prefers rendering, text/html too).
// message/rfc822 therefore lands in attachments, matching RFC 8621's
// note that attached messages are fetched via their blobId.
func isAttachmentPart(media, disposition string) bool {
	if strings.EqualFold(disposition, "attachment") {
		return true
	}
	return media != "text/plain" && media != "text/html" &&
		!strings.HasPrefix(media, "multipart/")
}

// --- small conversions ---

var wordDecoder = mime.WordDecoder{} //nolint:gochecknoglobals // stateless decoder

func decodeWords(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	if dec, err := wordDecoder.DecodeHeader(s); err == nil {
		return dec
	}
	return s
}

// decodeParam unfolds an RFC 2231 parameter ("utf-8”%E2%82%AC") and
// falls back to RFC 2047 words.
func decodeParam(s string) string {
	if i := strings.Index(s, "''"); i > 0 && !strings.Contains(s, " ") {
		if unescaped, err := percentDecode(s[i+2:]); err == nil {
			return unescaped
		}
	}
	return decodeWords(s)
}

func percentDecode(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v byte
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", err
			}
			v = byte(n)
			b.WriteByte(v)
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func jmapAddresses(in []Address) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, a := range in {
		email := a.Email()
		if email == "" {
			continue
		}
		entry := map[string]string{"email": email}
		if name := decodeWords(a.Name); name != "" {
			entry["name"] = name
		}
		out = append(out, entry)
	}
	return out
}

func allAddresses(groups ...[]Address) string {
	var parts []string
	for _, g := range groups {
		for _, a := range g {
			if name := decodeWords(a.Name); name != "" {
				parts = append(parts, name)
			}
			if e := a.Email(); e != "" {
				parts = append(parts, e)
			}
		}
	}
	return strings.Join(parts, " ")
}

func splitMsgIDs(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

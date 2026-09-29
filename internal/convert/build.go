package convert

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

// This file is the write half of the conversion package: it builds the
// RFC 5322 bytes of a draft from a JMAP Email/set create (FR-M.11,
// PLAN §7.1). It is pure — no network, no database — and it deliberately
// parses its own output back, so what the cache records (structure,
// partIds, body values, preview) is exactly what a hydration of the same
// bytes would have produced. A draft therefore reads like any other
// message.

// MailboxAddress is one JMAP EmailAddress: the wire spelling, so the
// built header and the stored headers JSON agree.
type MailboxAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// DraftPart is one MIME leaf of a draft. Exactly one of Text (inline
// body text) or Data (bytes, e.g. from a blobId) carries the content.
type DraftPart struct {
	Type        string // "text/plain" (default), "text/html", or an attachment media type
	Disposition string // "", "attachment" or "inline"
	Charset     string // defaults to utf-8 for text/*
	Name        string // filename for attachments
	Text        string // content when the part is inline text
	Data        []byte // content when the part came from a blob (nil for text)
}

// isAttachment reports how the store's own classifier will see this
// part (RFC 8621 §4.1): explicitly marked, or not a body representation.
func (p DraftPart) isAttachment() bool {
	if strings.EqualFold(p.Disposition, "attachment") {
		return true
	}
	return !strings.EqualFold(p.Type, "text/plain") && !strings.EqualFold(p.Type, "text/html")
}

// DraftInput is one Email/set create reduced to what a message needs.
// The jmapapi layer resolves blobIds to bytes, picks the target mailbox
// and validates the RFC 8621 §4.6 create constraints; convert only
// builds bytes.
type DraftInput struct {
	From, To, Cc, Bcc, ReplyTo []MailboxAddress
	Subject                    string
	Parts                      []DraftPart // ordered: bodies first, then attachments
	MessageID                  string      // "" → generated (RFC 8621 §4.6 requires one)
	InReplyTo, References      []string
	ReceivedAt                 time.Time // zero → now; becomes the Date header
}

// Draft is a built draft: the wire bytes plus everything the cache
// needs to record them without re-fetching.
type Draft struct {
	// Raw is the complete RFC 5322 message, ready for APPEND.
	Raw []byte
	// Rec is the header-level record (structure, sizes, preview filled
	// from parsing Raw) for store.CommitAppend.
	Rec store.MessageRec
	// Body is the hydration record (body values, attachment bytes,
	// preview) for store.PutHydrated.
	Body store.BodyResult
}

// BuildDraft renders in as an RFC 5322 message and parses it back.
func BuildDraft(in DraftInput) (*Draft, error) {
	received := in.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	msgID := strings.Trim(in.MessageID, "<>")
	if msgID == "" {
		msgID = generateMessageID()
	}

	parts := normaliseParts(in.Parts)
	if len(parts) == 0 {
		// A message must have a body; an empty text part keeps the
		// partId "1" every client expects (RFC 8621 §4.6).
		parts = []DraftPart{{Type: "text/plain"}}
	}
	boundary := "----=_jmapbridge_" + randomToken(12)
	// A second, independent boundary for the alternative inside a mixed
	// message: deriving it from the first would make one a prefix of the
	// other, which multipart scanners are entitled to confuse.
	altBoundary := "----=_jmapbridge_" + randomToken(12)

	var b strings.Builder
	writeHeader(&b, "From", formatAddresses(in.From))
	if len(in.To) > 0 {
		writeHeader(&b, "To", formatAddresses(in.To))
	}
	if len(in.Cc) > 0 {
		writeHeader(&b, "Cc", formatAddresses(in.Cc))
	}
	if len(in.Bcc) > 0 {
		writeHeader(&b, "Bcc", formatAddresses(in.Bcc))
	}
	if len(in.ReplyTo) > 0 {
		writeHeader(&b, "Reply-To", formatAddresses(in.ReplyTo))
	}
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", in.Subject))
	writeHeader(&b, "Date", received.Format("Mon, 2 Jan 2006 15:04:05 -0700"))
	writeHeader(&b, "Message-ID", "<"+msgID+">")
	if len(in.InReplyTo) > 0 {
		writeHeader(&b, "In-Reply-To", strings.Join(in.InReplyTo, " "))
	}
	if len(in.References) > 0 {
		writeHeader(&b, "References", strings.Join(in.References, " "))
	}
	b.WriteString("MIME-Version: 1.0\r\n")

	bodies, atts := splitParts(parts)
	writeBody(&b, bodies, atts, boundary, altBoundary)
	raw := []byte(b.String())

	// Parse the bytes we just wrote: the structure, partIds, values and
	// preview the cache stores are the ones hydration would produce.
	bodyRes, err := ParseBody(raw)
	if err != nil {
		return nil, fmt.Errorf("convert: parse built draft: %w", err)
	}
	tree, err := structureOfRaw(raw)
	if err != nil {
		return nil, fmt.Errorf("convert: structure of built draft: %w", err)
	}

	rec := store.MessageRec{
		ReceivedAt:    received,
		Size:          int64(len(raw)),
		HeadersJSON:   headersJSON(in),
		Subject:       in.Subject,
		SubjectL:      strings.ToLower(in.Subject),
		FromL:         lowerAddresses(in.From),
		ToL:           lowerAddresses(append(append([]MailboxAddress{}, in.To...), in.Cc...)),
		MessageIDs:    []string{"<" + msgID + ">"},
		InReplyTo:     append([]string(nil), in.InReplyTo...),
		References:    append([]string(nil), in.References...),
		SentAt:        &received,
		Structure:     tree,
		HasAttachment: len(bodyRes.Attachments) > 0,
		Preview:       bodyRes.Preview,
	}
	return &Draft{Raw: raw, Rec: rec, Body: bodyRes}, nil
}

// normaliseParts fills in the defaults a client may leave out.
func normaliseParts(in []DraftPart) []DraftPart {
	out := make([]DraftPart, 0, len(in))
	for _, p := range in {
		if p.Type == "" {
			p.Type = "text/plain"
		}
		if p.Charset == "" && strings.HasPrefix(strings.ToLower(p.Type), "text/") {
			p.Charset = "utf-8"
		}
		out = append(out, p)
	}
	return out
}

// splitParts separates body representations from attachments, keeping
// the client's order inside each group.
func splitParts(parts []DraftPart) (bodies, atts []DraftPart) {
	for _, p := range parts {
		if p.isAttachment() {
			atts = append(atts, p)
			continue
		}
		bodies = append(bodies, p)
	}
	return bodies, atts
}

// writeBody renders the MIME tree: multipart/mixed wraps the body
// alternatives plus attachments; multipart/alternative wraps text+html;
// a lone body is a single part. The container shape is normalised (RFC
// 8621 §4.6 lets the server choose), and partIds come back from parsing
// the result, so no client-visible numbering depends on this layout.
func writeBody(b *strings.Builder, bodies, atts []DraftPart, boundary, altBoundary string) {
	switch {
	case len(atts) == 0 && len(bodies) == 1:
		writePart(b, bodies[0])
	case len(atts) == 0:
		writeAlternative(b, bodies, altBoundary)
	case len(bodies) == 0:
		writeMixed(b, nil, atts, boundary)
	default:
		var inner strings.Builder
		if len(bodies) == 1 {
			writePart(&inner, bodies[0])
		} else {
			writeAlternative(&inner, bodies, altBoundary)
		}
		writeMixed(b, []byte(inner.String()), atts, boundary)
	}
}

func writeAlternative(b *strings.Builder, bodies []DraftPart, boundary string) {
	writeHeader(b, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range bodies {
		b.WriteString("--" + boundary + "\r\n")
		writePart(b, p)
	}
	b.WriteString("--" + boundary + "--\r\n")
}

func writeMixed(b *strings.Builder, inner []byte, atts []DraftPart, boundary string) {
	writeHeader(b, "Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	if len(inner) > 0 {
		// Every child of a multipart container opens with the
		// container's delimiter; without it the block is preamble.
		b.WriteString("--" + boundary + "\r\n")
		b.Write(inner)
	}
	for _, p := range atts {
		b.WriteString("--" + boundary + "\r\n")
		writePart(b, p)
	}
	b.WriteString("--" + boundary + "--\r\n")
}

// writePart renders one leaf: its Content-Type / Content-Disposition,
// a transfer encoding chosen from the content, then the body.
func writePart(b *strings.Builder, p DraftPart) {
	ct := strings.ToLower(p.Type)
	if p.Charset != "" && strings.HasPrefix(ct, "text/") {
		writeHeader(b, "Content-Type", p.Type+`; charset=`+p.Charset)
	} else {
		writeHeader(b, "Content-Type", p.Type)
	}
	if p.Name != "" {
		writeHeader(b, "Content-Disposition", `attachment; filename="`+escapeQuoted(p.Name)+`"`)
	} else if p.Disposition != "" {
		writeHeader(b, "Content-Disposition", p.Disposition)
	}

	content := p.Text
	if p.Data != nil {
		content = string(p.Data)
	} else {
		// MIME bodies are CRLF-delimited (RFC 2046 §5.1); a client's
		// editor supplies bare LFs.
		content = crlf(content)
	}
	if p.Data != nil || !isAsciiText(content) {
		// Binary (or anything not 7-bit clean) travels base64: no
		// encoding games with 8-bit bytes on the wire.
		writeHeader(b, "Content-Transfer-Encoding", "base64")
		b.WriteString("\r\n")
		b.WriteString(wrapBase64([]byte(content)))
		return
	}
	if p.Data == nil && p.Text != "" {
		writeHeader(b, "Content-Transfer-Encoding", "7bit")
	}
	b.WriteString("\r\n")
	b.WriteString(content)
	if !strings.HasSuffix(content, "\r\n") {
		b.WriteString("\r\n")
	}
}

// addressHeaders are the only fields folded by writeHeader: address
// lists are what actually runs long, and folding anything else risks
// splitting a boundary parameter or an encoded word (parsers unfold
// correctly in theory; in practice a folded multipart/Content-Type is a
// interoperability trap, and RFC 5322 §2.2.3 makes 78 columns a
// recommendation, not a rule).
var addressHeaders = map[string]bool{
	"From": true, "To": true, "Cc": true, "Bcc": true, "Reply-To": true,
}

// writeHeader writes one header field, folding long address lists at
// comma boundaries so no line approaches the 998-octet limit (RFC 5322
// §2.2.3) and no encoded word is ever split (RFC 2047 §6).
func writeHeader(b *strings.Builder, name, value string) {
	line := name + ": " + value
	if len(line) <= 78 || !addressHeaders[name] {
		b.WriteString(line + "\r\n")
		return
	}
	b.WriteString(name + ":")
	written := 0
	for _, item := range splitHeaderValue(value) {
		if written+len(item)+2 > 76 {
			b.WriteString("\r\n ") // folding whitespace
			written = 1
		}
		b.WriteString(" " + item)
		written += len(item) + 1
	}
	b.WriteString("\r\n")
}

// splitHeaderValue breaks a value on ", " (address-list separators) so
// folds never land inside an address or an encoded word.
func splitHeaderValue(value string) []string {
	parts := strings.Split(value, ", ")
	for i := range parts {
		if i < len(parts)-1 {
			parts[i] += ","
		}
	}
	return parts
}

func formatAddresses(addrs []MailboxAddress) string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, formatAddress(a))
	}
	return strings.Join(out, ", ")
}

func formatAddress(a MailboxAddress) string {
	if a.Name == "" {
		return a.Email
	}
	name := mime.QEncoding.Encode("utf-8", a.Name)
	switch {
	case name != a.Name:
		// RFC 2047 encoded word in phrase position: never quote it.
		return name + " <" + a.Email + ">"
	case strings.ContainsAny(name, `",;<>@\`):
		return `"` + strings.ReplaceAll(name, `"`, `\"`) + `" <` + a.Email + ">"
	default:
		return name + " <" + a.Email + ">"
	}
}

// headersJSON is the canonical JMAP header summary the store keeps
// (same shape convert.Summary writes from an ENVELOPE).
func headersJSON(in DraftInput) string {
	h := map[string]any{}
	if in.Subject != "" {
		h["subject"] = in.Subject
	}
	for key, addrs := range map[string][]MailboxAddress{
		"from": in.From, "to": in.To, "cc": in.Cc,
		"bcc": in.Bcc, "replyTo": in.ReplyTo,
	} {
		if j := jmapAddressList(addrs); len(j) > 0 {
			h[key] = j
		}
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func jmapAddressList(addrs []MailboxAddress) []map[string]string {
	out := make([]map[string]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Email == "" {
			continue
		}
		e := map[string]string{"email": a.Email}
		if a.Name != "" {
			e["name"] = a.Name
		}
		out = append(out, e)
	}
	return out
}

func lowerAddresses(addrs []MailboxAddress) string {
	parts := make([]string, 0, len(addrs)*2)
	for _, a := range addrs {
		if a.Name != "" {
			parts = append(parts, strings.ToLower(a.Name))
		}
		if a.Email != "" {
			parts = append(parts, strings.ToLower(a.Email))
		}
	}
	return strings.Join(parts, " ")
}

// generateMessageID mints an opaque, globally unique msg-id (RFC 8621
// §4.6: the server must set one when the client did not).
func generateMessageID() string {
	return randomToken(16) + "@" + hostname()
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

// crlf normalises line endings to CRLF without doubling them.
func crlf(s string) string {
	if !strings.Contains(s, "\n") && !strings.Contains(s, "\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func isAsciiText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' || s[i] == '\t' {
			continue
		}
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// wrapBase64 encodes content and wraps it at the 76-octet line limit
// MIME requires (RFC 2045 §6.8).
func wrapBase64(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	b.Grow(len(enc) + len(enc)/76*2)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.String()
}

func escapeQuoted(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

package convert

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers x/text charset codecs
)

// previewLimit is the maximum preview length in runes; PLAN §5 sizes
// previews at a 4 KiB partial fetch.
const previewLimit = 4096

// ParseBody converts a raw RFC 5322 message into the store's hydration
// record: body values keyed by partId (numbered exactly as
// BuildStructure numbers the ENVELOPE-path tree), decoded attachment
// bytes, and a flat preview derived from the first text/plain part.
//
// A soft error (unknown charset or transfer encoding) is returned
// alongside usable data — go-message degrades instead of failing — so
// the caller can log it and still store what was read. A hard error
// (unparseable headers) returns a nil-ish result with err set.
func ParseBody(raw []byte) (store.BodyResult, error) {
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && !isSoft(err) {
		return store.BodyResult{}, err
	}
	soft := err

	res := store.BodyResult{Values: map[string]string{}}
	walker := &bodyWalker{res: &res}
	walker.walk(ent, "")
	if soft == nil {
		soft = walker.err
	}
	res.Preview = flatPreview(walker.firstText)
	// The structure is built from the same bytes with the same partId
	// numbering as the values, so bodyStructure and bodyValues can never
	// disagree (FR-M.4). It is the only structure an API-mode message gets:
	// its header pass has no IMAP BODYSTRUCTURE.
	if structure, serr := structureOfRaw(raw); serr == nil {
		res.Structure = structure
	} else if soft == nil {
		soft = serr
	}
	return res, soft
}

// bodyWalker carries the walk's accumulated state.
type bodyWalker struct {
	res       *store.BodyResult
	firstText string
	err       error
}

func (w *bodyWalker) walk(ent *message.Entity, section string) {
	if mr := ent.MultipartReader(); mr != nil {
		defer func() { _ = mr.Close() }()
		for i := 1; ; i++ {
			part, err := mr.NextPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				if w.err == nil {
					w.err = err
				}
				return
			}
			label := strconv.Itoa(i)
			if section != "" {
				label = section + "." + label
			}
			w.walk(part, label)
		}
	}

	media, params, _ := ent.Header.ContentType()
	media = strings.ToLower(media)
	disposition, dispParams, _ := ent.Header.ContentDisposition()

	body, err := io.ReadAll(ent.Body)
	if err != nil && w.err == nil {
		w.err = err
	}
	if section == "" {
		section = "1" // a single-part message's body is section 1
	}

	if isAttachmentPart(media, disposition) {
		name := decodeParam(firstNonEmpty(dispParams["filename"], params["filename"]))
		w.res.Attachments = append(w.res.Attachments, store.AttachmentData{
			PartID:    section,
			MediaType: media,
			Name:      name,
			Data:      body,
		})
		return
	}
	if media == "text/plain" || media == "text/html" {
		w.res.Values[section] = string(body)
		if media == "text/plain" && w.firstText == "" {
			w.firstText = string(body)
		}
	}
}

// flatPreview flattens a body excerpt into a single line: newlines and
// runs of whitespace collapse to single spaces (the shape jmap-tui and
// other list UIs render), capped at previewLimit runes.
func flatPreview(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	lastSpace := false
	for _, r := range text {
		if r == '\r' {
			continue
		}
		if r == '\n' || r == '\t' || r == ' ' {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > previewLimit {
		runes := []rune(out)
		out = string(runes[:previewLimit])
	}
	return out
}

// isSoft reports the go-message errors that still leave usable data.
func isSoft(err error) bool {
	return message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)
}

// PreviewFromPartial extracts preview text from a partial message
// fetch (BODY.PEEK[]<0.n>): the header block is dropped, multipart
// preambles yield nothing (their first bytes are boundary markers, not
// prose), and the rest is flattened like any other preview (PLAN §5).
func PreviewFromPartial(raw []byte) string {
	i := indexHeaderEnd(raw)
	if i < 0 {
		return "" // headers only (or no terminator): nothing to show
	}
	trimmed := trimLeadingSpace(raw[i:])
	if len(trimmed) >= 2 && trimmed[0] == '-' && trimmed[1] == '-' {
		return "" // multipart preamble/boundary — partial fetch cannot see prose
	}
	if bytes.IndexByte(trimmed, 0) >= 0 {
		return "" // binary payload: leave it to hydration
	}
	return flatPreview(string(trimmed))
}

func indexHeaderEnd(raw []byte) int {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	return -1
}

func trimLeadingSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	return b
}

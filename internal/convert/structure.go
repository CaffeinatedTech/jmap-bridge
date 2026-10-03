package convert

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-message"
)

// structureOf walks a parsed message into the JMAP EmailBodyPart tree
// the store keeps (RFC 8621 §4.1.3): leaf parts carry partId, type,
// charset, size, name and disposition; containers carry subParts and no
// partId. Numbering mirrors bodyWalker's exactly — the same section
// labels the body values are keyed by — so structure and bodyValues can
// never disagree (FR-M.4).
func structureOf(ent *message.Entity, section string) map[string]any {
	media, params, _ := ent.Header.ContentType()
	media = strings.ToLower(media)

	if mr := ent.MultipartReader(); mr != nil {
		defer func() { _ = mr.Close() }()
		obj := map[string]any{"type": media}
		var subs []any
		for i := 1; ; i++ {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			label := strconv.Itoa(i)
			if section != "" {
				label = section + "." + label
			}
			subs = append(subs, structureOf(part, label))
		}
		if len(subs) > 0 {
			obj["subParts"] = subs
		}
		return obj
	}

	disposition, dispParams, _ := ent.Header.ContentDisposition()
	body, _ := io.ReadAll(ent.Body)
	if section == "" {
		section = "1" // a single-part message's body is section 1
	}
	obj := map[string]any{"type": media, "partId": section, "size": len(body)}
	if cs := params["charset"]; cs != "" && strings.HasPrefix(media, "text/") {
		obj["charset"] = strings.ToLower(cs)
	}
	if name := decodeParam(firstNonEmpty(dispParams["filename"], params["filename"])); name != "" {
		obj["name"] = name
	}
	if disposition != "" {
		obj["disposition"] = strings.ToLower(disposition)
	}
	if cid := stripAngle(ent.Header.Get("Content-Id")); cid != "" {
		obj["cid"] = cid
	}
	return obj
}

// stripAngle removes the angle brackets RFC 5322 puts around a
// Content-ID value; JMAP carries the bare content id (RFC 8621 §4.1.4).
func stripAngle(s string) string {
	return strings.Trim(strings.TrimSpace(s), "<>")
}

// structureOfRaw parses raw bytes and renders that tree as JSON.
func structureOfRaw(raw []byte) (string, error) {
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && !isSoft(err) {
		return "", err
	}
	out, err := json.Marshal(structureOf(ent, ""))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

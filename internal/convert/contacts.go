// contacts.go is the vCard ⇄ JSContact half of the package (PLAN §8,
// FR-P.12): RFC 2426/6350 vCard in, RFC 9553 JSContact out, guided by
// RFC 9554 so no registered property is silently dropped. Properties the
// bridge does not model travel inside "@vcard-ext" (an RFC 9553 extension
// member a conformant client preserves), which is what makes the
// golden-pair round-trips lossless. PHOTO bytes are handed over by the
// caller because they live in the blob store, not in a row (FR-P.11).

package convert

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-vcard"
)

// PhotoRef is a card photo after conversion: once the blob layer has run,
// BlobID names the bytes in the store; Original keeps the vCard PHOTO
// value when it was an external URI (which has no bytes to store).
type PhotoRef struct {
	BlobID   string `json:"blobId,omitempty"`
	Type     string `json:"type,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Original string `json:"uri,omitempty"`
}

// Contact is the JSContact content of one vCard as the store keeps and
// the API serves it. The fields cover the property set the bridge models
// (PLAN §8); everything else rides in Ext and survives round-trips.
type Contact struct {
	UID           string              `json:"uid,omitempty"`
	Kind          string              `json:"kind,omitempty"`
	Name          *JSName             `json:"name,omitempty"`
	Emails        map[string]JSEntry  `json:"emails,omitempty"`
	Phones        map[string]JSEntry  `json:"phones,omitempty"`
	Organizations map[string]JSOrg    `json:"organizations,omitempty"`
	Titles        map[string]JSOrg    `json:"titles,omitempty"`
	Notes         map[string]JSNote   `json:"notes,omitempty"`
	Addresses     map[string]JSAddr   `json:"addresses,omitempty"`
	Links         map[string]JSLink   `json:"links,omitempty"`
	Birthday      string              `json:"birthday,omitempty"`
	Members       map[string]JSMember `json:"members,omitempty"`
	Photo         *PhotoRef           `json:"photo,omitempty"`

	// Ext preserves unmodelled and grouped vCard properties verbatim.
	Ext map[string][]string `json:"@vcard-ext,omitempty"`

	// inlinePhoto carries decoded PHOTO bytes out of VCFToContact; being
	// unexported it never marshals — the blob store owns the bytes.
	inlinePhoto []byte
}

// JSName is the RFC 9553 Name object as the bridge emits it.
type JSName struct {
	Components []JSNameComponent `json:"components"`
	Full       string            `json:"full"`
	IsOrdered  bool              `json:"isOrdered"`
}

// JSNameComponent is one name part (given, surname, …).
type JSNameComponent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// JSEntry is one keyed entry of the simple maps (emails, phones).
type JSEntry struct {
	Address string `json:"address,omitempty"`
	Number  string `json:"number,omitempty"`
	Label   string `json:"label,omitempty"`
	Pref    *int   `json:"pref,omitempty"`
	Ext     *VCExt `json:"@vcard,omitempty"`
}

// JSOrg is one entry of organizations/titles (RFC 9553 Organization).
type JSOrg struct {
	Name  string   `json:"name"`
	Units []string `json:"units,omitempty"`
	Ext   *VCExt   `json:"@vcard,omitempty"`
}

// JSNote is one Note entry.
type JSNote struct {
	Note string `json:"note"`
	Ext  *VCExt `json:"@vcard,omitempty"`
}

// JSAddr is one postal address (RFC 9553 Address).
type JSAddr struct {
	Label      string   `json:"label,omitempty"`
	Street     []string `json:"street,omitempty"`
	POBox      string   `json:"poBox,omitempty"`
	City       string   `json:"city,omitempty"`
	Region     string   `json:"region,omitempty"`
	PostalCode string   `json:"postalCode,omitempty"`
	Country    string   `json:"country,omitempty"`
	Ext        *VCExt   `json:"@vcard,omitempty"`
}

// JSLink is one entry of links (a URL, or a logo URI).
type JSLink struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
	Ext  *VCExt `json:"@vcard,omitempty"`
}

// JSMember is one group member, referenced by vCard UID. The reference
// key is an RFC 9553 extension member on purpose: RFC 9610 groups are
// modelled by UID, and clients preserve unknown members.
type JSMember struct {
	UID string `json:"@vcard-uid"`
	Ext *VCExt `json:"@vcard,omitempty"`
}

// VCExt is the preserved wire form of one vCard field: the params of the
// instance the entry came from, so a rebuild reproduces them exactly.
type VCExt struct {
	Params map[string][]string `json:"params,omitempty"`
}

// UnmarshalContact decodes the stored canonical JSON back into the typed
// form the builder needs.
func UnmarshalContact(data []byte) (*Contact, error) {
	var c Contact
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("convert: decode stored JSContact: %w", err)
	}
	return &c, nil
}

// VCFToContact parses one vCard (3.0 or 4.0) into the JSContact content.
// A card the decoder cannot fully read is still converted from whatever
// parsed, because one malformed contact must not fail a whole book sync
// (PLAN §8 conversion is lossless, not all-or-nothing); only a card with
// nothing in it is an error. When PHOTO carries inline bytes they land in
// Contact.inlinePhoto for the caller to file as a blob (FR-P.11).
func VCFToContact(raw []byte) (*Contact, error) {
	card, err := vcard.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil && len(card) == 0 {
		return nil, fmt.Errorf("convert: decode vCard: %w", err)
	}
	if len(card) == 0 {
		return nil, fmt.Errorf("convert: empty vCard")
	}
	c := &Contact{Kind: "individual"}

	c.UID = strings.TrimSpace(card.Value("UID"))
	if kind := strings.ToLower(strings.TrimSpace(card.Value("KIND"))); kind != "" && kind != "individual" {
		c.Kind = kind
	}
	c.Name = convertName(card)
	c.Emails = convertEntries(card, "EMAIL", func(f *vcard.Field) JSEntry {
		return JSEntry{Address: f.Value, Label: typeLabel(f), Pref: prefParam(f)}
	})
	c.Phones = convertEntries(card, "TEL", func(f *vcard.Field) JSEntry {
		return JSEntry{Number: f.Value, Label: typeLabel(f), Pref: prefParam(f)}
	})
	c.Organizations = convertOrgs(card, "ORG")
	c.Titles = convertOrgs(card, "TITLE")
	c.Notes = convertNotes(card)
	c.Addresses = convertAddrs(card)
	c.Links = convertLinks(card)
	c.Birthday = strings.TrimSpace(card.Value("BDAY"))
	c.Members = convertMembers(card)
	c.Photo, c.inlinePhoto = convertPhoto(card)

	c.Ext = collectExt(card)
	if len(c.Ext) == 0 {
		c.Ext = nil
	}
	return c, nil
}

// InlinePhoto returns the photo bytes parsed out of the vCard, if any.
// The caller files them as a blob and the reference survives on the card.
func (c *Contact) InlinePhoto() []byte { return c.inlinePhoto }

// SetInlinePhotoBytes clears parsed bytes after blob storage (the wire
// JSON must never carry them).
func (c *Contact) ClearInlinePhoto() { c.inlinePhoto = nil }

var modeledProps = map[string]bool{
	"BEGIN": true, "END": true, "VERSION": true, "UID": true, "KIND": true,
	"FN": true, "N": true, "EMAIL": true, "TEL": true, "ORG": true,
	"TITLE": true, "NOTE": true, "ADR": true, "URL": true, "BDAY": true,
	"MEMBER": true, "PHOTO": true, "LOGO": true,
}

// collectExt sweeps every field the modelled conversions did not consume
// into the preserved map. Unmodelled names travel whole; modelled names
// only add a grouped instance (RFC 2426 X- sources), whose group would
// otherwise be lost.
func collectExt(card vcard.Card) map[string][]string {
	ext := map[string][]string{}
	for name, fields := range card {
		if !modeledProps[name] {
			raw := make([]string, 0, len(fields))
			for _, f := range fields {
				raw = append(raw, encodeFieldWire(f))
			}
			ext[name] = raw
			continue
		}
		// PHOTO and BDAY model exactly one instance; a card carrying
		// more keeps the rest rather than dropping them.
		for i, f := range fields {
			switch {
			case f.Group != "":
				ext[name] = append(ext[name], encodeFieldWire(f))
			case i > 0 && (name == "PHOTO" || name == "BDAY"):
				ext[name] = append(ext[name], encodeFieldWire(f))
			}
		}
	}
	return ext
}

// encodeFieldWire renders one field back to a storable string: value and
// params round-trip; the property name is the map key.
func encodeFieldWire(f *vcard.Field) string {
	var b strings.Builder
	b.WriteString(strconv.Quote(f.Value))
	if f.Group != "" || len(f.Params) > 0 {
		b.WriteByte('\n')
		meta, err := json.Marshal(struct {
			Group  string              `json:"g,omitempty"`
			Params map[string][]string `json:"p,omitempty"`
		}{f.Group, f.Params})
		if err != nil {
			meta = []byte("{}")
		}
		b.Write(meta)
	}
	return b.String()
}

// decodeFieldWire is encodeFieldWire's inverse.
func decodeFieldWire(s string) *vcard.Field {
	f := &vcard.Field{Value: s}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		if value, err := strconv.Unquote(s[:i]); err == nil {
			f.Value = value
		}
		var meta struct {
			Group  string              `json:"g,omitempty"`
			Params map[string][]string `json:"p,omitempty"`
		}
		if json.Unmarshal([]byte(s[i+1:]), &meta) == nil {
			f.Group = meta.Group
			f.Params = vcard.Params(meta.Params)
		}
	}
	return f
}

func convertName(card vcard.Card) *JSName {
	n := card.Value("N")
	fn := strings.TrimSpace(card.Value("FN"))
	if n == "" && fn == "" {
		return nil
	}
	name := &JSName{Full: fn, IsOrdered: true}
	parts := splitStructured(n, ';')
	if len(parts) > 0 {
		name.addComponent("surname", parts[0])
	}
	if len(parts) > 1 {
		name.addComponent("given", parts[1])
	}
	if len(parts) > 2 {
		for _, add := range splitStructured(parts[2], ',') {
			name.addComponent("additional", add)
		}
	}
	if len(parts) > 3 {
		for _, pre := range splitStructured(parts[3], ',') {
			name.addComponent("honorific-prefix", pre)
		}
	}
	if len(parts) > 4 {
		for _, suf := range splitStructured(parts[4], ',') {
			name.addComponent("honorific-suffix", suf)
		}
	}
	if name.Full == "" {
		var b []string
		for _, comp := range name.Components {
			if comp.Kind == "given" || comp.Kind == "surname" {
				b = append(b, comp.Value)
			}
		}
		name.Full = strings.Join(b, " ")
	}
	if len(name.Components) == 0 && name.Full == "" {
		return nil
	}
	return name
}

// splitStructured splits one structured vCard value on sep, honouring the
// backslash escapes of RFC 6350 §6.3, and unescapes the pieces. Empty
// input yields no parts.
func splitStructured(s string, sep byte) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte('\\')
			}
		case sep:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// escapeValue is splitStructured's inverse.
func escapeValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\', ';', ',', ':':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (n *JSName) addComponent(kind, value string) {
	if value = strings.TrimSpace(value); value == "" {
		return
	}
	n.Components = append(n.Components, JSNameComponent{Kind: kind, Value: value})
}

// typeLabel maps the TYPE parameter to the JSContact label: the
// recognisable categories stay; structural ones (INTERNET, VALUE-shaping
// types) do not become labels (RFC 9554 §3.4). TYPE is a comma-separated
// list, and the decoder hands list values as repeated entries — both
// spellings are scanned.
func typeLabel(f *vcard.Field) string {
	for _, t := range allTypeValues(f) {
		switch strings.TrimSpace(t) {
		case "home", "work", "cell", "mobile", "voice", "fax", "video",
			"pager", "text", "main", "car", "isdn", "pcs":
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// allTypeValues flattens the TYPE parameter, splitting any comma lists
// inside single values too (servers differ on how they emit them).
func allTypeValues(f *vcard.Field) []string {
	var out []string
	for _, v := range f.Params["TYPE"] {
		for _, p := range strings.Split(strings.ToLower(v), ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// prefParam reads the JSContact preference: the v4 PREF parameter, or the
// v3 TYPE=pref marker (RFC 9554 §3.4.1).
func prefParam(f *vcard.Field) *int {
	if p := f.Params.Get("PREF"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 1 {
			return &n
		}
	}
	for _, t := range allTypeValues(f) {
		if t == "pref" {
			one := 1
			return &one
		}
	}
	return nil
}

func convertEntries(card vcard.Card, name string, build func(*vcard.Field) JSEntry) map[string]JSEntry {
	fields := card[name]
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]JSEntry, len(fields))
	for i, f := range fields {
		if f.Group != "" {
			continue // preserved in Ext instead
		}
		e := build(f)
		if preserveParams(f, e.Label) {
			e.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = e
	}
	return out
}

func copyParams(p vcard.Params) map[string][]string {
	out := make(map[string][]string, len(p))
	for k, v := range p {
		out[k] = append([]string{}, v...)
	}
	return out
}

func convertOrgs(card vcard.Card, name string) map[string]JSOrg {
	fields := card[name]
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]JSOrg, len(fields))
	for i, f := range fields {
		if f.Group != "" {
			continue
		}
		o := JSOrg{}
		if name == "ORG" {
			parts := strings.Split(f.Value, ";")
			o.Name = parts[0]
			for _, u := range parts[1:] {
				if u != "" {
					o.Units = append(o.Units, u)
				}
			}
		} else {
			o.Name = f.Value
		}
		if preserveParams(f, "") {
			o.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = o
	}
	return out
}

func convertNotes(card vcard.Card) map[string]JSNote {
	fields := card["NOTE"]
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]JSNote, len(fields))
	for i, f := range fields {
		if f.Group != "" {
			continue
		}
		n := JSNote{Note: f.Value}
		if preserveParams(f, "") {
			n.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = n
	}
	return out
}

// convertAddrs reads the seven ADR components of RFC 6350 §6.3.1.1:
// post-office-box, extended-address, street, locality, region,
// postal-code, country. Both the extended address and the street lines
// join the JSContact street, which is how RFC 9554 maps them.
func convertAddrs(card vcard.Card) map[string]JSAddr {
	fields := card["ADR"]
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]JSAddr, len(fields))
	for i, f := range fields {
		if f.Group != "" {
			continue
		}
		parts := splitStructured(f.Value, ';')
		get := func(idx int) string {
			if idx < len(parts) {
				return parts[idx]
			}
			return ""
		}
		a := JSAddr{
			POBox:      get(0),
			City:       get(3),
			Region:     get(4),
			PostalCode: get(5),
			Country:    get(6),
			Label:      typeLabel(f),
		}
		for _, comp := range []string{get(1), get(2)} {
			for _, line := range splitStructured(comp, ',') {
				if line != "" {
					a.Street = append(a.Street, line)
				}
			}
		}
		if preserveParams(f, a.Label) {
			a.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = a
	}
	return out
}

// preserveParams reports whether a field's params carry anything the
// JSContact entry cannot represent on its own: params beyond TYPE/PREF,
// or a TYPE list richer than the single recognised label. Those fields
// keep their exact params in the entry extension so the rebuild is
// faithful and stability survives providers that encode more in TYPE
// than a label.
func preserveParams(f *vcard.Field, label string) bool {
	types := allTypeValues(f)
	for k := range f.Params {
		switch k {
		case "TYPE", "PREF":
		default:
			return true
		}
	}
	if len(types) == 0 {
		return false
	}
	if len(types) > 1 {
		return true
	}
	return types[0] != label
}

func convertLinks(card vcard.Card) map[string]JSLink {
	urls, logos := card["URL"], card["LOGO"]
	if len(urls) == 0 && len(logos) == 0 {
		return nil
	}
	out := map[string]JSLink{}
	i := 0
	for _, f := range urls {
		l := JSLink{URI: f.Value}
		if preserveParams(f, "") {
			l.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = l
		i++
	}
	for _, f := range logos {
		l := JSLink{URI: f.Value, Name: "logo"}
		if preserveParams(f, "") {
			l.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = l
		i++
	}
	return out
}

func convertMembers(card vcard.Card) map[string]JSMember {
	fields := card["MEMBER"]
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]JSMember, len(fields))
	for i, f := range fields {
		uid := strings.TrimSpace(f.Value)
		// urn:uuid: is the v4 spelling; anything else (mailto:, tel:,
		// a bare uid) stays as-is apart from its scheme prefix, because
		// the bridge references members by vCard UID (FR-P.13).
		uid = strings.TrimPrefix(uid, "urn:uuid:")
		m := JSMember{UID: uid}
		if preserveParams(f, "") {
			m.Ext = &VCExt{Params: copyParams(f.Params)}
		}
		out[strconv.Itoa(i)] = m
	}
	return out
}

// convertPhoto resolves the PHOTO property. Inline base64 (v4 data: URI
// or v3 ENCODING=b) is decoded here for the caller's blob store; an
// external http(s) URI stays on the card untouched.
func convertPhoto(card vcard.Card) (*PhotoRef, []byte) {
	fs := card["PHOTO"]
	if len(fs) == 0 {
		return nil, nil
	}
	f := fs[0]
	p := &PhotoRef{}
	v := strings.TrimSpace(f.Value)
	switch {
	case strings.HasPrefix(strings.ToLower(v), "http://"),
		strings.HasPrefix(strings.ToLower(v), "https://"):
		p.Original = v
	case strings.HasPrefix(strings.ToLower(v), "data:"):
		if data, media, ok := decodeDataURL(v); ok {
			p.Type = media
			p.Size = int64(len(data))
			return p, data
		}
		p.Original = v
	default:
		// v3 inline base64: the value is the bytes, the params say so.
		if enc := strings.ToLower(f.Params.Get("ENCODING")); enc == "b" || enc == "base64" {
			if data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(v, "\r", ""), "\n", "")); err == nil {
				media := "image/jpeg"
				if t := strings.ToLower(f.Params.Get("TYPE")); t != "" {
					media = "image/" + t
				}
				p.Type = media
				p.Size = int64(len(data))
				return p, data
			}
		}
		p.Original = v
	}
	return p, nil
}

// decodeDataURL parses a base64 data: URI (the RFC 2397 form RFC 6350
// PHOTO uses for inline media).
func decodeDataURL(s string) (data []byte, media string, ok bool) {
	rest := s[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, "", false
	}
	header := rest[:comma]
	payload := rest[comma+1:]
	media = header
	isB64 := false
	if semi := strings.Index(header, ";"); semi >= 0 {
		media = header[:semi]
		isB64 = strings.EqualFold(header[semi+1:], "base64")
	}
	payload = strings.ReplaceAll(payload, "\r", "")
	payload = strings.ReplaceAll(payload, "\n", "")
	if isB64 {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", false
		}
		return decoded, strings.ToLower(media), true
	}
	return []byte(payload), strings.ToLower(media), true
}

// ContactToVCF renders one JSContact content object back to a vCard 4.0
// (RFC 6350). photoBytes are the blob the PhotoRef names (nil → the
// external URI, if any, is the remaining honest representation).
func ContactToVCF(c *Contact, photoBytes []byte) ([]byte, error) {
	out := vcard.Card{}
	add := func(name string, f *vcard.Field) { out.Add(name, f) }
	photoLine := ""

	add("VERSION", &vcard.Field{Value: "4.0"})
	if c.UID != "" {
		add("UID", &vcard.Field{Value: c.UID})
	}
	if c.Kind != "" && c.Kind != "individual" {
		add("KIND", &vcard.Field{Value: strings.ToLower(c.Kind)})
	}
	if c.Name != nil {
		addName(c.Name, add)
	}
	addEntries(add, "EMAIL", c.Emails, func(e JSEntry) (string, JSEntry) { return e.Address, e })
	addEntries(add, "TEL", c.Phones, func(e JSEntry) (string, JSEntry) { return e.Number, e })
	for _, key := range sortedMapKeys(c.Organizations) {
		o := c.Organizations[key]
		value := o.Name
		for _, u := range o.Units {
			value += ";" + u
		}
		add("ORG", restoredField(o.Ext, value))
	}
	for _, key := range sortedMapKeys(c.Titles) {
		t := c.Titles[key]
		add("TITLE", restoredField(t.Ext, t.Name))
	}
	for _, key := range sortedMapKeys(c.Notes) {
		add("NOTE", restoredField(c.Notes[key].Ext, c.Notes[key].Note))
	}
	for _, key := range sortedMapKeys(c.Addresses) {
		a := c.Addresses[key]
		value := strings.Join([]string{
			escapeValue(a.POBox), "", escapeList(a.Street), escapeValue(a.City),
			escapeValue(a.Region), escapeValue(a.PostalCode), escapeValue(a.Country),
		}, ";")
		f := restoredField(a.Ext, value)
		f.Params = mergeLabelType(f.Params, a.Label)
		add("ADR", f)
	}
	for _, key := range sortedMapKeys(c.Links) {
		l := c.Links[key]
		if l.Name == "logo" {
			add("LOGO", restoredField(l.Ext, l.URI))
		} else {
			add("URL", restoredField(l.Ext, l.URI))
		}
	}
	if c.Birthday != "" {
		add("BDAY", &vcard.Field{Value: c.Birthday})
	}
	for _, key := range sortedMapKeys(c.Members) {
		m := c.Members[key]
		uri := m.UID
		if looksLikeUUID(uri) {
			uri = "urn:uuid:" + uri
		}
		// MEMBER's default value type is URI (RFC 6350 §6.4.10), so no
		// VALUE parameter is needed — and adding one would make the
		// round-trip unstable for cards that never had it.
		add("MEMBER", restoredField(m.Ext, uri))
	}
	if c.Photo != nil {
		ph := &vcard.Field{}
		switch {
		case photoBytes != nil && isImageType(c.Photo.Type):
			ph.Value = "data:" + c.Photo.Type + ";base64," +
				base64.StdEncoding.EncodeToString(photoBytes)
		case c.Photo.Original != "":
			ph.Value = c.Photo.Original
		default:
			// The blob is gone and there is no URI: say nothing rather
			// than write an empty PHOTO (a server would reject it).
			ph = nil
		}
		if ph != nil {
			// PHOTO is URI-typed: its commas must not take the TEXT
			// escaping the library encoder applies to every value. The
			// line is emitted in the fold pass below, not through the
			// encoder (photoLine stays out of the card map).
			photoLine = "PHOTO:" + ph.Value
		}
	}
	// Ext entries re-emit after the modelled ones: grouped instances of
	// modelled properties (they must not collide with the primary
	// fields) and every property the model does not carry at all.
	for _, name := range sortedMapKeys(c.Ext) {
		for _, wire := range c.Ext[name] {
			add(name, decodeFieldWire(wire))
		}
	}

	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(out); err != nil {
		return nil, fmt.Errorf("convert: encode vCard: %w", err)
	}
	return foldVCard(buf.Bytes(), photoLine), nil
}

// foldVCard canonicalises the encoder output: it unfolds, re-emits every
// logical line, splices the (unescaped-URI) PHOTO line before the
// trailer, and folds at the RFC 6350 §3.2 limit of 75 octets, never
// splitting inside a UTF-8 sequence.
func foldVCard(encoded []byte, photoLine string) []byte {
	lines := unfoldVCardLines(encoded)
	if photoLine != "" {
		// Deterministic position: just before END:VCARD. vCard property
		// order is free (RFC 6350 §3), and END only needs to be last.
		for i, ln := range lines {
			if strings.HasPrefix(ln, "END:") {
				lines = append(lines[:i], append([]string{photoLine}, lines[i:]...)...)
				break
			}
		}
	}
	var b bytes.Buffer
	for _, ln := range lines {
		b.WriteString(foldLine(ln))
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// unfoldVCardLines joins continuation lines (CRLF + space, RFC 6350 §3.2)
// back into logical lines and trims the trailing END newline handling.
func unfoldVCardLines(encoded []byte) []string {
	var out []string
	for _, phys := range strings.Split(strings.ReplaceAll(string(encoded), "\r\n", "\n"), "\n") {
		if phys == "" {
			continue
		}
		if (phys[0] == ' ' || phys[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += phys[1:]
			continue
		}
		out = append(out, phys)
	}
	return out
}

// foldLine wraps one logical line at 74 octets (the limit is 75 counting
// the leading space of continuations) on UTF-8 boundaries.
func foldLine(s string) string {
	const limit = 74
	if len(s) <= limit {
		return s
	}
	var b strings.Builder
	b.WriteString(s[:firstBoundary(s, limit)])
	s = s[firstBoundary(s, limit):]
	for len(s) > 0 {
		n := firstBoundary(s, limit-1)
		b.WriteString("\r\n ")
		b.WriteString(s[:n])
		s = s[n:]
	}
	return b.String()
}

// firstBoundary returns the largest prefix length ≤ limit that ends on a
// UTF-8 rune boundary.
func firstBoundary(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	if limit == 0 {
		// One giant rune cannot happen in practice; emit whole to avoid
		// a zero-length fold loop.
		return len(s)
	}
	return limit
}

func addName(n *JSName, add func(string, *vcard.Field)) {
	var surname, given, additional, prefixes, suffixes []string
	for _, comp := range n.Components {
		switch comp.Kind {
		case "surname":
			surname = append(surname, comp.Value)
		case "given":
			given = append(given, comp.Value)
		case "additional":
			additional = append(additional, comp.Value)
		case "honorific-prefix":
			prefixes = append(prefixes, comp.Value)
		case "honorific-suffix":
			suffixes = append(suffixes, comp.Value)
		}
	}
	full := n.Full
	if full == "" {
		full = strings.Join(append(append([]string{}, given...), surname...), " ")
	}
	if full != "" {
		add("FN", &vcard.Field{Value: full})
	}
	if len(surname)+len(given)+len(additional)+len(prefixes)+len(suffixes) > 0 {
		add("N", &vcard.Field{Value: strings.Join([]string{
			escapeList(surname), escapeList(given),
			escapeList(additional), escapeList(prefixes),
			escapeList(suffixes),
		}, ";")})
	}
}

// escapeList joins structured list items, escaping each and the
// separators, the inverse of splitStructured on ",".
func escapeList(items []string) string {
	escaped := make([]string, 0, len(items))
	for _, it := range items {
		escaped = append(escaped, escapeValue(it))
	}
	return strings.Join(escaped, ",")
}

func addEntries(add func(string, *vcard.Field), name string, in map[string]JSEntry, pick func(JSEntry) (string, JSEntry)) {
	for _, key := range sortedMapKeys(in) {
		value, e := pick(in[key])
		if value == "" {
			continue
		}
		f := restoredField(e.Ext, value)
		// v4 spells preference as a PREF parameter; v3 used TYPE=pref.
		// When the preserved params already carry the v3 marker, adding
		// PREF too would make the round-trip unstable.
		if e.Pref != nil && !typeListHasPref(f.Params) {
			f.Params = setParam(f.Params, "PREF", strconv.Itoa(*e.Pref))
		}
		f.Params = mergeLabelType(f.Params, e.Label)
		add(name, f)
	}
}

// typeListHasPref reports the v3 pref marker in a TYPE parameter.
func typeListHasPref(p vcard.Params) bool {
	for _, v := range p["TYPE"] {
		for _, item := range strings.Split(strings.ToLower(v), ",") {
			if strings.TrimSpace(item) == "pref" {
				return true
			}
		}
	}
	return false
}

// mergeLabelType keeps a preserved TYPE list intact and adds the label
// only when it is missing: a card that came in as TYPE=WORK,VOICE must
// go back out the same way, not as TYPE=Work.
func mergeLabelType(p vcard.Params, label string) vcard.Params {
	if label == "" {
		return p
	}
	want := strings.ToLower(label)
	for _, v := range p["TYPE"] {
		for _, item := range strings.Split(strings.ToLower(v), ",") {
			if strings.TrimSpace(item) == want {
				return p
			}
		}
	}
	if p == nil {
		p = vcard.Params{}
	}
	p["TYPE"] = append(p["TYPE"], titleCaseLabel(label))
	return p
}

func restoredField(ext *VCExt, value string) *vcard.Field {
	f := &vcard.Field{Value: value}
	if ext != nil && len(ext.Params) > 0 {
		params := make(vcard.Params, len(ext.Params))
		for k, v := range ext.Params {
			params[k] = append([]string{}, v...)
		}
		f.Params = params
	}
	return f
}

func setParam(p vcard.Params, k, v string) vcard.Params {
	if p == nil {
		p = vcard.Params{}
	}
	p[k] = []string{v}
	return p
}

func titleCaseLabel(l string) string {
	if l == "" {
		return ""
	}
	r := []rune(l)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isImageType(t string) bool {
	switch t {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

func looksLikeUUID(s string) bool {
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return false
	}
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			continue
		}
		return false
	}
	return true
}

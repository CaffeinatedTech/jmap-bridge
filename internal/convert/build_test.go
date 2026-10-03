package convert

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// readBack parses the built bytes the way any other agent would, so the
// assertions are about the wire, not about our own struct.
func readBack(t *testing.T, raw []byte) (*mail.Message, string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse built message: %v", err)
	}
	dec := mime.WordDecoder{}
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	return msg, subject
}

func structureOfRec(t *testing.T, recJSON string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(recJSON), &out); err != nil {
		t.Fatalf("structure: %v", err)
	}
	return out
}

func TestBuildDraftTextOnlyRoundTrip(t *testing.T) {
	in := DraftInput{
		From:       []MailboxAddress{{Name: "Me", Email: "me@example.test"}},
		To:         []MailboxAddress{{Email: "you@example.test"}},
		Subject:    "Quarterly report",
		Parts:      []DraftPart{{Type: "text/plain", Text: "Numbers go up.\nSecond line."}},
		MessageID:  "<draft-1@example.test>",
		ReceivedAt: time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC),
	}
	d, err := BuildDraft(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	msg, subject := readBack(t, d.Raw)
	if subject != in.Subject {
		t.Errorf("subject = %q, want %q", subject, in.Subject)
	}
	if got := msg.Header.Get("Message-ID"); got != "<draft-1@example.test>" {
		t.Errorf("message-id = %q", got)
	}
	if got := msg.Header.Get("Date"); !strings.Contains(got, "2 Sep 2026") {
		t.Errorf("date = %q, want the received date", got)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Address != "me@example.test" || from.Name != "Me" {
		t.Errorf("from = %#v (err %v)", from, err)
	}

	// Body values keyed the way the structure numbers them.
	// A single-part message keeps its terminating line break in the value;
	// multipart parts do not (RFC 2046 §5.1.1). Hydration of the same
	// bytes behaves identically, so the cache stays consistent either way.
	if got := strings.TrimRight(d.Body.Values["1"], "\r\n"); got != "Numbers go up.\r\nSecond line." {
		t.Errorf("body value = %q", got)
	}
	if d.Rec.Preview == "" {
		t.Error("preview empty")
	}
	if d.Rec.HasAttachment || len(d.Body.Attachments) != 0 {
		t.Errorf("attachment flags set on a text-only draft")
	}
	if d.Rec.Size != int64(len(d.Raw)) {
		t.Errorf("size = %d, want %d", d.Rec.Size, len(d.Raw))
	}
	if len(d.Rec.MessageIDs) != 1 || d.Rec.MessageIDs[0] != "<draft-1@example.test>" {
		t.Errorf("message ids = %v", d.Rec.MessageIDs)
	}

	// Structure agrees with the values: one text/plain part, id "1".
	tree := structureOfRec(t, d.Rec.Structure)
	if tree["type"] != "text/plain" || tree["partId"] != "1" {
		t.Errorf("structure = %#v", tree)
	}
	wantSize := len("Numbers go up.\r\nSecond line.\r\n")
	if size, ok := tree["size"].(float64); !ok || int(size) != wantSize {
		t.Errorf("structure size = %v, want %d (CRLF-normalised body)", tree["size"], wantSize)
	}
}

func TestBuildDraftAlternativeAndAttachment(t *testing.T) {
	att := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 'P', 'K'}
	in := DraftInput{
		From:    []MailboxAddress{{Email: "me@example.test"}},
		Subject: "With everything",
		Parts: []DraftPart{
			{Type: "text/plain", Text: "plain body"},
			{Type: "text/html", Text: "<p>plain body</p>"},
			{Type: "application/zip", Name: "payload.zip", Disposition: "attachment", Data: att},
		},
		MessageID:  "<draft-2@example.test>",
		ReceivedAt: time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC),
	}
	d, err := BuildDraft(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	readBack(t, d.Raw)

	if !d.Rec.HasAttachment {
		t.Error("hasAttachment false with an attachment")
	}
	if len(d.Body.Attachments) != 1 {
		t.Fatalf("attachments = %#v", d.Body.Attachments)
	}
	got := d.Body.Attachments[0]
	if !bytes.Equal(got.Data, att) {
		t.Errorf("attachment bytes = %v, want %v", got.Data, att)
	}
	if got.Name != "payload.zip" || got.MediaType != "application/zip" {
		t.Errorf("attachment meta = name %q type %q", got.Name, got.MediaType)
	}

	// Every body value is reachable from the structure, and the tree
	// nests the way MIME was written: mixed{alternative{…}, attachment}.
	tree := structureOfRec(t, d.Rec.Structure)
	if tree["type"] != "multipart/mixed" {
		t.Fatalf("root = %v, want multipart/mixed", tree["type"])
	}
	subs := tree["subParts"].([]any)
	if len(subs) != 2 {
		t.Fatalf("subParts = %d, want alternative + attachment", len(subs))
	}
	alt := subs[0].(map[string]any)
	if alt["type"] != "multipart/alternative" {
		t.Errorf("first child = %v, want multipart/alternative", alt["type"])
	}
	attNode := subs[1].(map[string]any)
	if attNode["disposition"] != "attachment" || attNode["name"] != "payload.zip" {
		t.Errorf("attachment node = %#v", attNode)
	}
	// Structure ids and body values are the same key space.
	for _, id := range []string{"1.1", "1.2"} {
		if _, ok := d.Body.Values[id]; !ok {
			t.Errorf("no body value for part %q (values: %v)", id, d.Body.Values)
		}
	}
	if d.Body.Values["1.1"] != "plain body" || d.Body.Values["1.2"] != "<p>plain body</p>" {
		t.Errorf("body values = %v", d.Body.Values)
	}
}

func TestBuildDraftNonASCIIRoundTrip(t *testing.T) {
	in := DraftInput{
		From:       []MailboxAddress{{Name: "Jürgen Öztürk", Email: "juergen@example.test"}},
		To:         []MailboxAddress{{Name: "Ünicode Person", Email: "uni@example.test"}},
		Subject:    "Grüße aus München 🚀",
		Parts:      []DraftPart{{Type: "text/plain", Text: "Ein bisschen Text mit Umlauten: äöü"}},
		ReceivedAt: time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC),
	}
	d, err := BuildDraft(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	msg, subject := readBack(t, d.Raw)
	if subject != in.Subject {
		t.Errorf("subject = %q, want %q", subject, in.Subject)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		t.Fatalf("parse from: %v", err)
	}
	if from.Name != "Jürgen Öztürk" {
		t.Errorf("from name = %q", from.Name)
	}
	if got := d.Body.Values["1"]; got != "Ein bisschen Text mit Umlauten: äöü" {
		t.Errorf("body = %q", got)
	}
	if !strings.Contains(d.Rec.FromL, "juergen@example.test") {
		t.Errorf("from_l = %q", d.Rec.FromL)
	}
}

func TestBuildDraftEmptyBodyStillFormsAMessage(t *testing.T) {
	in := DraftInput{
		From:    []MailboxAddress{{Email: "me@example.test"}},
		Subject: "Nothing to say",
	}
	d, err := BuildDraft(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	readBack(t, d.Raw)
	tree := structureOfRec(t, d.Rec.Structure)
	if tree["type"] != "text/plain" || tree["partId"] != "1" {
		t.Errorf("structure = %#v, want a single text/plain part 1", tree)
	}
	if d.Rec.Subject != in.Subject {
		t.Errorf("subject = %q", d.Rec.Subject)
	}
}

func TestBuildDraftGeneratesMessageIDAndDate(t *testing.T) {
	d, err := BuildDraft(DraftInput{
		From:    []MailboxAddress{{Email: "me@example.test"}},
		Subject: "no ids given",
		Parts:   []DraftPart{{Text: "body"}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	msg, _ := readBack(t, d.Raw)
	if got := msg.Header.Get("Message-ID"); !strings.HasPrefix(got, "<") || !strings.Contains(got, "@") {
		t.Errorf("message-id = %q, want a generated <token@host>", got)
	}
	if msg.Header.Get("Date") == "" {
		t.Error("Date header missing (RFC 8621 §4.6: the server must set one)")
	}
	if len(d.Rec.MessageIDs) != 1 || d.Rec.MessageIDs[0] != msg.Header.Get("Message-ID") {
		t.Errorf("rec message ids = %v, header = %q", d.Rec.MessageIDs, msg.Header.Get("Message-ID"))
	}
	if d.Rec.SentAt == nil || d.Rec.SentAt.IsZero() {
		t.Error("sentAt not recorded")
	}
}

func TestBuildDraftLongAddressListFolds(t *testing.T) {
	var to []MailboxAddress
	for i := 0; i < 12; i++ {
		to = append(to, MailboxAddress{Name: "Recipient With A Long Name", Email: "recipient.with.a.long.name@example.test"})
	}
	d, err := BuildDraft(DraftInput{
		From:    []MailboxAddress{{Email: "me@example.test"}},
		To:      to,
		Subject: "many recipients",
		Parts:   []DraftPart{{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, line := range strings.Split(string(d.Raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line of %d octets exceeds RFC 5322 §2.2.3", len(line))
		}
	}
	msg, _ := readBack(t, d.Raw)
	list, err := mail.ParseAddressList(msg.Header.Get("To"))
	if err != nil {
		t.Fatalf("parse to list: %v", err)
	}
	if len(list) != len(to) {
		t.Errorf("parsed %d recipients, want %d", len(list), len(to))
	}
}

// TestStripBcc pins the delivery-time Bcc removal of RFC 8621 §7.5: the
// header and its folds go, everything else — including a line in the
// body that looks exactly like one — stays.
func TestStripBcc(t *testing.T) {
	msg := "From: me@example.test\r\n" +
		"Bcc: hidden@example.test\r\n" +
		" bcc-fold@example.test\r\n" +
		"Subject: hi\r\n" +
		"Received: by x\r\n" +
		"\r\n" +
		"Bcc: this is body text, not a header\r\n" +
		"\r\n.\r\n"
	want := "From: me@example.test\r\n" +
		"Subject: hi\r\n" +
		"Received: by x\r\n" +
		"\r\n" +
		"Bcc: this is body text, not a header\r\n" +
		"\r\n.\r\n"
	if got := string(StripBcc([]byte(msg))); got != want {
		t.Errorf("StripBcc =\n%q\nwant\n%q", got, want)
	}

	// Case is irrelevant (header names are case-insensitive), an absent
	// Bcc changes nothing, and a bare-LF message survives too.
	lower := "to: a@b.c\nbcc: hidden@x.y\nsubject: s\n\nbody\n"
	if got := string(StripBcc([]byte(lower))); got != "to: a@b.c\nsubject: s\n\nbody\n" {
		t.Errorf("lowercase StripBcc = %q", got)
	}
	none := "to: a@b.c\r\nsubject: s\r\n\r\nbody\r\n"
	if got := string(StripBcc([]byte(none))); got != none {
		t.Errorf("StripBcc changed a message without Bcc: %q", got)
	}
}

// TestStructureCarriesContentID pins RFC 8621 §4.1.4: an inline part's
// Content-ID travels as the body part's cid, with the angle brackets
// stripped.
func TestStructureCarriesContentID(t *testing.T) {
	raw := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=\"b\"\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><img src=\"cid:image1@example.test\"></html>\r\n" +
		"--b\r\n" +
		"Content-Type: image/jpeg\r\n" +
		"Content-ID: <image1@example.test>\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"/9j/4AAQ\r\n" +
		"--b--\r\n"

	treeJSON, err := structureOfRaw([]byte(raw))
	if err != nil {
		t.Fatalf("structureOfRaw: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(treeJSON), &tree); err != nil {
		t.Fatal(err)
	}
	image := findPartByType(tree, "image/jpeg")
	if image == nil {
		t.Fatalf("no image part in %s", treeJSON)
	}
	if got := image["cid"]; got != "image1@example.test" {
		t.Errorf("cid = %v, want image1@example.test", got)
	}
}

func findPartByType(part map[string]any, mediaType string) map[string]any {
	if t, _ := part["type"].(string); strings.Contains(t, mediaType) {
		return part
	}
	if subs, ok := part["subParts"].([]any); ok {
		for _, s := range subs {
			if child, ok := s.(map[string]any); ok {
				if found := findPartByType(child, mediaType); found != nil {
					return found
				}
			}
		}
	}
	return nil
}

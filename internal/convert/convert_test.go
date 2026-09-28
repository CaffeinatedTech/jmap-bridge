package convert

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenBody is the stable rendering of a BodyResult for goldens:
// attachment bytes travel base64 so the file stays text.
type goldenBody struct {
	Values      map[string]string `json:"values,omitempty"`
	Attachments []goldenAtt       `json:"attachments,omitempty"`
	Preview     string            `json:"preview,omitempty"`
	SoftError   string            `json:"softError,omitempty"`
}

type goldenAtt struct {
	PartID    string `json:"partId"`
	MediaType string `json:"mediaType"`
	Name      string `json:"name,omitempty"`
	Data      string `json:"data"`
}

func renderBody(res store.BodyResult, soft error) goldenBody {
	g := goldenBody{Values: res.Values, Preview: res.Preview}
	if soft != nil {
		g.SoftError = soft.Error()
	}
	for _, a := range res.Attachments {
		g.Attachments = append(g.Attachments, goldenAtt{
			PartID: a.PartID, MediaType: a.MediaType, Name: a.Name,
			Data: base64.StdEncoding.EncodeToString(a.Data),
		})
	}
	return g
}

// TestParseBodyGolden pins ParseBody against testdata/*.eml: partId
// numbering, charset/transfer decoding, attachment extraction, preview
// flattening, and how malformed input degrades (golden-pair rule,
// AGENTS.md).
func TestParseBodyGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".eml")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			res, soft := ParseBody(raw)
			got := renderBody(res, soft)
			goldenPath := filepath.Join("testdata", name+".body.json")
			if *update {
				out, _ := json.MarshalIndent(got, "", "  ")
				if err := os.WriteFile(goldenPath, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantRaw, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden (run with -update): %v", err)
			}
			var want goldenBody
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("bad golden: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				gj, _ := json.MarshalIndent(got, "", "  ")
				wj, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("body mismatch:\n got: %s\nwant: %s", gj, wj)
			}
		})
	}
}

// TestPartIDsAgreeBetweenPaths is the load-bearing invariant: the raw
// parse and the ENVELOPE/BODYSTRUCTURE path must number the same tree
// identically, or bodyValues keys miss their structure parts (FR-M.4).
func TestPartIDsAgreeBetweenPaths(t *testing.T) {
	// The mixed.eml tree as BODYSTRUCTURE would report it.
	root := &Part{Type: "MULTIPART", SubType: "mixed", Children: []*Part{
		{Type: "MULTIPART", SubType: "alternative", Children: []*Part{
			{Type: "text", SubType: "plain", Params: map[string]string{"charset": "utf-8"}, Size: 16},
			{Type: "text", SubType: "html", Params: map[string]string{"charset": "utf-8"}, Size: 28},
		}},
		{
			Type: "application", SubType: "pdf", Params: map[string]string{"name": "inv.pdf"},
			Disposition: "attachment", DispositionParams: map[string]string{"filename": "inv.pdf"}, Size: 12,
		},
	}}
	BuildStructure(root, "")
	structureIDs := map[string]string{}
	collectStructureIDs(root, structureIDs)

	raw, err := os.ReadFile(filepath.Join("testdata", "mixed.eml"))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := ParseBody(raw)
	rawIDs := map[string]string{}
	for pid := range res.Values {
		rawIDs[pid] = "value"
	}
	for _, a := range res.Attachments {
		rawIDs[a.PartID] = "attachment"
	}
	if !reflect.DeepEqual(keySet(structureIDs), keySet(rawIDs)) {
		t.Errorf("partIds disagree:\nstructure: %v\nraw:       %v", keySet(structureIDs), keySet(rawIDs))
	}
	if res.Attachments[0].Data == nil {
		t.Error("attachment bytes not extracted")
	}
	if !bytes.HasPrefix(res.Attachments[0].Data, []byte("%PDF-1.4")) {
		t.Errorf("attachment = %q, want a decoded %%PDF-1.4 prefix", res.Attachments[0].Data)
	}
}

func keySet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func collectStructureIDs(p *Part, into map[string]string) {
	if p.PartID != "" {
		into[p.PartID] = "part"
	}
	for _, c := range p.Children {
		collectStructureIDs(c, into)
	}
}

// TestSummary covers the ENVELOPE path: header JSON, lowercase query
// mirrors, threading inputs, date parsing and hasAttachment.
func TestSummary(t *testing.T) {
	env := Envelope{
		Date:       "Tue, 01 Sep 2026 12:00:00 +0000",
		Subject:    "Re: =?utf-8?Q?Gr=C3=BC=C3=9Fe?=",
		From:       []Address{{Name: "Müller", Mailbox: "muller", Host: "example.test"}},
		To:         []Address{{Mailbox: "me", Host: "example.test"}},
		MessageID:  "<mix-1@example.test>",
		References: " <first@example.test>\t<second@example.test> ",
	}
	root := &Part{Type: "MULTIPART", SubType: "mixed", Children: []*Part{
		{Type: "text", SubType: "plain", Params: map[string]string{"charset": "utf-8"}, Size: 10},
		{Type: "application", SubType: "pdf", Disposition: "attachment", Size: 99},
	}}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rec := Summary(env, root, at, 4096)

	if rec.Subject != "Re: Grüße" {
		t.Errorf("subject = %q (encoded words not decoded)", rec.Subject)
	}
	if rec.SubjectL != "re: grüße" {
		t.Errorf("subjectL = %q", rec.SubjectL)
	}
	if !strings.Contains(rec.FromL, "muller@example.test") {
		t.Errorf("fromL = %q", rec.FromL)
	}
	if !strings.Contains(rec.ToL, "me@example.test") {
		t.Errorf("toL = %q", rec.ToL)
	}
	if !rec.HasAttachment {
		t.Error("hasAttachment not derived from the structure")
	}
	if !reflect.DeepEqual(rec.MessageIDs, []string{"<mix-1@example.test>"}) {
		t.Errorf("messageIds = %v", rec.MessageIDs)
	}
	if !reflect.DeepEqual(rec.References, []string{"<first@example.test>", "<second@example.test>"}) {
		t.Errorf("references = %v", rec.References)
	}
	if rec.SentAt == nil || rec.SentAt.Day() != 1 {
		t.Errorf("sentAt = %v", rec.SentAt)
	}
	// Numbering: text "1", attachment "2"; containers carry no id.
	var text, attach string
	BuildStructure(root, "")
	collect(root, &text, &attach)
	if text != "1" || attach != "2" {
		t.Errorf("partIds text=%q attach=%q, want 1/2", text, attach)
	}
	var headers map[string]any
	if err := json.Unmarshal([]byte(rec.HeadersJSON), &headers); err != nil {
		t.Fatalf("headers JSON: %v", err)
	}
	if headers["subject"] != "Re: Grüße" {
		t.Errorf("headers.subject = %v", headers["subject"])
	}
}

func collect(p *Part, text, attach *string) {
	if p.PartID != "" && p.Type == "text" {
		*text = p.PartID
	}
	if p.PartID != "" && p.Type == "application" {
		*attach = p.PartID
	}
	for _, c := range p.Children {
		collect(c, text, attach)
	}
}

// TestPreviewFlattening pins the one-line, capped preview.
func TestPreviewFlattening(t *testing.T) {
	long := strings.Repeat("word ", 2000)
	got := flatPreview("  line one\r\n\tline   two  \n" + long)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("preview still has whitespace runs: %q", got[:40])
	}
	if !strings.HasPrefix(got, "line one line two word") {
		t.Errorf("preview head = %q", got[:40])
	}
	if len([]rune(got)) > previewLimit {
		t.Errorf("preview longer than %d runes", previewLimit)
	}
	if flatPreview("") != "" {
		t.Error("empty body must give empty preview")
	}
}

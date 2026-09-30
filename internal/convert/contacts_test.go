package convert

// Golden pairs for the vCard ⇄ JSContact half (AGENTS.md: conversion is
// golden-pair tested, including malformed/degenerate input). -update
// regenerates the .json side; the review step is reading the diff.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func updateGolden() bool { return os.Getenv("CONVERT_GOLDEN_UPDATE") == "1" }

func TestVCardGoldenPairs(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("testdata", "contacts", "*.vcf"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no contact fixtures found: %v", err)
	}
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".vcf")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := VCFToContact(raw)
			if err != nil {
				t.Fatalf("VCFToContact: %v", err)
			}
			gotJSON, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			goldenPath := filepath.Join("testdata", "contacts", name+".json")
			if updateGolden() {
				if err := os.WriteFile(goldenPath, append(gotJSON, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantRaw, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden (run with CONVERT_GOLDEN_UPDATE=1 and review): %v", err)
			}
			// Compare through the same pipeline both sides: the golden
			// is parsed into the model and re-encoded, so formatting
			// drift is out and semantic equality is in.
			var want Contact
			if err := json.Unmarshal(wantRaw, &want); err != nil {
				t.Fatalf("golden JSON invalid: %v", err)
			}
			wantJSON, err := json.MarshalIndent(&want, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", name, wantJSON, gotJSON)
			}

			// Stability: rebuild the vCard and convert again. The second
			// pass must produce the same JSContact — this is FR-P.12's
			// round-trip claim pinned as a test.
			vcf, err := ContactToVCF(got, got.inlinePhoto)
			if err != nil {
				t.Fatalf("ContactToVCF: %v", err)
			}
			back, err := VCFToContact(vcf)
			if err != nil {
				t.Fatalf("reconvert: %v\nvcard was:\n%s", err, vcf)
			}
			backJSON, _ := json.MarshalIndent(back, "", "  ")
			if string(backJSON) != string(gotJSON) {
				t.Errorf("round-trip drift for %s:\n--- first ---\n%s\n--- rebuilt ---\n%s", name, gotJSON, backJSON)
			}
		})
	}
}

func TestVCardGoldenRebuildContent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "contacts", "v4-full.vcf"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := VCFToContact(raw)
	if err != nil {
		t.Fatal(err)
	}
	vcf, err := ContactToVCF(c, c.inlinePhoto)
	if err != nil {
		t.Fatal(err)
	}
	s := string(vcf)
	for _, want := range []string{
		"BEGIN:VCARD", "VERSION:4.0", "END:VCARD",
		"UID:01985042-2212-7345-8000-ABCDEF012345",
		"N:King;Ada;;;",
		"PHOTO:data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==",
		"X-SOCIALPROFILE", // preserved extension
		"MEMBER",          // absent: this card has none — checked below
	} {
		if want == "MEMBER" {
			if strings.Contains(s, "MEMBER") {
				t.Errorf("rebuild carries a MEMBER it should not have:\n%s", s)
			}
			continue
		}
		if !strings.Contains(s, want) {
			t.Errorf("rebuild missing %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "ADR;") || !strings.Contains(s, "TYPE=Home") {
		t.Errorf("rebuild lost the ADR label:\n%s", s)
	}
	if !strings.Contains(s, "LANGUAGE=en-gb") {
		t.Errorf("rebuild lost preserved NOTE params:\n%s", s)
	}
}

func TestVCardDegenerate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		fail bool
	}{
		{"empty", "", true},
		{"no vcard", "just some text\n", true},
		{"header only", "BEGIN:VCARD\r\nVERSION:4.0\r\n", false},
		{"broken tail", "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Half\r\nGARBAGE NOT A LINE\r\nEND:VCARD\r\n", false},
		{"bare newline", "\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := VCFToContact([]byte(tc.in))
			if tc.fail {
				if err == nil {
					t.Fatalf("expected error, got card %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("degenerate input must salvage, not fail: %v", err)
			}
			if _, err := ContactToVCF(c, nil); err != nil {
				t.Fatalf("rebuild of degenerate card: %v", err)
			}
		})
	}
}

func TestMembersUseUIDReferences(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "contacts", "group.vcf"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := VCFToContact(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "group" {
		t.Fatalf("kind = %q, want group", c.Kind)
	}
	if len(c.Members) != 2 {
		t.Fatalf("members = %v", c.Members)
	}
	if c.Members["0"].UID != "01985042-2212-7345-8000-ABCDEF012345" {
		t.Errorf("member 0 uid = %q", c.Members["0"].UID)
	}
	vcf, err := ContactToVCF(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vcf), "MEMBER:urn:uuid:01985042-2212-7345-8000-ABCDEF012345") {
		t.Errorf("rebuilt group lost the urn:uuid form:\n%s", vcf)
	}
	if !strings.Contains(string(vcf), "KIND:group") {
		t.Errorf("rebuilt group lost KIND:\n%s", vcf)
	}
}

func TestPhotoV3InlineDecodes(t *testing.T) {
	in := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:photov3\r\nFN:P\r\n" +
		"PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRg==\r\nEND:VCARD\r\n"
	c, err := VCFToContact([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.Photo == nil || c.Photo.Type != "image/jpeg" || len(c.inlinePhoto) == 0 {
		t.Fatalf("photo not decoded: %+v", c.Photo)
	}
	vcf, err := ContactToVCF(c, c.inlinePhoto)
	if err != nil {
		t.Fatal(err)
	}
	back, err := VCFToContact(vcf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Photo.Type != "image/jpeg" || string(back.inlinePhoto) != string(c.inlinePhoto) {
		t.Errorf("v3 photo did not survive the v4 rebuild: %+v", back.Photo)
	}
}

package gmailapi

import "testing"

func TestKeywordsFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   map[string]bool
	}{
		{"read", []string{LabelInbox}, map[string]bool{KeywordSeen: true}},
		{"unread", []string{LabelInbox, LabelUnread}, map[string]bool{KeywordSeen: false}},
		{"full", []string{LabelUnread, LabelStarred, LabelImportant, LabelDraft}, map[string]bool{
			KeywordSeen: false, KeywordFlagged: true, KeywordImportant: true, KeywordDraft: true,
		}},
		{"empty", nil, map[string]bool{KeywordSeen: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := KeywordsFromLabels(tc.labels)
			if len(got) != len(tc.want) {
				t.Fatalf("keywords = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("keywords[%s] = %v, want %v (%v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestMapKeywords(t *testing.T) {
	m := MapKeywords([]string{KeywordSeen, KeywordFlagged}, []string{KeywordDraft})
	if len(m.Unsupported) != 0 {
		t.Fatalf("unexpected unsupported: %v", m.Unsupported)
	}
	// Adding $seen marks read → remove UNREAD; adding $flagged → STARRED;
	// removing $draft → remove DRAFT.
	if !equalSet(m.Add, []string{LabelStarred}) {
		t.Fatalf("add = %v, want [STARRED]", m.Add)
	}
	if !equalSet(m.Remove, []string{LabelUnread, LabelDraft}) {
		t.Fatalf("remove = %v, want [UNREAD DRAFT]", m.Remove)
	}
}

func TestMapKeywordsRefusesUnsupported(t *testing.T) {
	m := MapKeywords([]string{"$answered", "$deleted", "custom"}, nil)
	if len(m.Add) != 0 {
		t.Fatalf("unsupported keywords produced labels: %v", m.Add)
	}
	for _, kw := range []string{"$answered", "$deleted", "custom"} {
		if !contains(m.Unsupported, kw) {
			t.Fatalf("unsupported = %v, missing %q", m.Unsupported, kw)
		}
	}
}

func TestMapKeywordsRemovalWins(t *testing.T) {
	// A keyword in both sets is removed: removing $seen un-reads the
	// message, which is the inverse label add.
	m := MapKeywords([]string{KeywordSeen}, []string{KeywordSeen})
	if !equalSet(m.Add, []string{LabelUnread}) || len(m.Remove) != 0 {
		t.Fatalf("add/remove = %v/%v, want [UNREAD]/[]", m.Add, m.Remove)
	}
}

func TestDecodeSnippet(t *testing.T) {
	in := "Hello&#39;s &#8212; world\u200b\u200c\u00ad!"
	if got := DecodeSnippet(in); got != "Hello's — world!" {
		t.Fatalf("DecodeSnippet = %q", got)
	}
	if got := DecodeSnippet("  plain  "); got != "plain" {
		t.Fatalf("DecodeSnippet trim = %q", got)
	}
}

func TestThreadKeyStableAndPrefixed(t *testing.T) {
	a := ThreadKey("18c1f2a3b4")
	b := ThreadKey("18c1f2a3b4")
	c := ThreadKey("18c1f2a3b5")
	if a == "" || a[:2] != "g:" || len(a) != 42 {
		t.Fatalf("thread key shape = %q", a)
	}
	if a != b {
		t.Fatal("thread key not deterministic")
	}
	if a == c {
		t.Fatal("distinct thread ids collided")
	}
	if ThreadKey("") != "" {
		t.Fatal("empty thread id must yield empty key")
	}
}

func TestHeaderValueFoldsCase(t *testing.T) {
	headers := []Header{{Name: "From", Value: "a@x"}, {Name: "Message-ID", Value: "<id@x>"}}
	if got := HeaderValue(headers, "message-id"); got != "<id@x>" {
		t.Fatalf("HeaderValue = %q", got)
	}
	if got := HeaderValue(headers, "subject"); got != "" {
		t.Fatalf("missing header = %q", got)
	}
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !contains(got, w) {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

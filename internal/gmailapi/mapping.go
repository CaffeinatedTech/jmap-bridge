package gmailapi

import (
	"crypto/sha1" //nolint:gosec // thread lookup key, not security
	"encoding/hex"
	"html"
	"strings"
	"unicode"
)

// Gmail's well-known label ids. System labels are fixed strings; user labels
// carry opaque ids. The synthetic archive mailbox is not a real label and never
// appears here (D-API-11).
const (
	LabelInbox     = "INBOX"
	LabelSent      = "SENT"
	LabelDraft     = "DRAFT"
	LabelTrash     = "TRASH"
	LabelSpam      = "SPAM"
	LabelImportant = "IMPORTANT"
	LabelStarred   = "STARRED"
	LabelUnread    = "UNREAD"
	LabelChat      = "CHAT"
)

// JMAP keywords API mode can surface and write (D-API-5). Everything else is
// refused rather than silently dropped.
const (
	KeywordSeen      = "$seen"
	KeywordFlagged   = "$flagged"
	KeywordDraft     = "$draft"
	KeywordImportant = "$important"
)

// Header is a neutral MIME header pair, so the mapping helpers can be golden-pair
// tested without a google type in the signature.
type Header struct {
	Name  string
	Value string
}

// HeaderValue returns the first header with the given name (case-insensitive),
// or "" when absent. Gmail metadata headers preserve the sender's casing, so the
// match folds case rather than trusting exact spelling.
func HeaderValue(headers []Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// ThreadKey derives the store's thread registry key for a Gmail thread id. The
// API's threadId is an opaque hex string rather than IMAP's numeric X-GM-THRID,
// so it is hashed by its textual form; API-mode keys are internally consistent
// and never mix with IMAP keys for the same account (D-API-9). The "g:" prefix
// makes a Gmail grouping outrank header chains exactly as M4 does (FR-S.10).
func ThreadKey(threadID string) string {
	if threadID == "" {
		return ""
	}
	sum := sha1.Sum([]byte(threadID))
	return "g:" + hex.EncodeToString(sum[:])
}

// KeywordsFromLabels maps a Gmail label set to the JMAP keywords API mode
// supports. Gmail tracks read state by the presence of UNREAD, so $seen is the
// inverse; STARRED, IMPORTANT and DRAFT are direct presences. Labels that are
// really mailbox membership (INBOX, SENT, …) are not keywords and are ignored
// here.
func KeywordsFromLabels(labelIDs []string) map[string]bool {
	keywords := map[string]bool{}
	unread := false
	for _, id := range labelIDs {
		switch id {
		case LabelUnread:
			unread = true
		case LabelStarred:
			keywords[KeywordFlagged] = true
		case LabelImportant:
			keywords[KeywordImportant] = true
		case LabelDraft:
			keywords[KeywordDraft] = true
		}
	}
	keywords[KeywordSeen] = !unread
	return keywords
}

// LabelMutation is the Gmail-side effect of a JMAP keyword patch: labels to add
// and remove, plus any requested keyword the Gmail model cannot express. The
// caller refuses the whole write when Unsupported is non-empty (FR-M.8, D-API-5)
// — Gmail has no $answered, no $deleted keyword and no arbitrary keywords.
type LabelMutation struct {
	Add         []string
	Remove      []string
	Unsupported []string
}

// MapKeywords translates JMAP keyword add/remove sets into Gmail label
// add/remove sets. $seen inverts to UNREAD; $flagged, $draft and $important map
// straight across. A keyword present in both sets is treated as a removal
// (the last operation wins, matching patch order).
func MapKeywords(add, remove []string) LabelMutation {
	var m LabelMutation
	removed := map[string]bool{}
	for _, kw := range remove {
		removed[kw] = true
		if label, ok := keywordLabel(kw); ok {
			m.Remove = append(m.Remove, label)
		} else {
			m.Unsupported = append(m.Unsupported, kw)
		}
	}
	for _, kw := range add {
		if removed[kw] {
			continue
		}
		if label, ok := keywordLabel(kw); ok {
			m.Add = append(m.Add, label)
		} else {
			m.Unsupported = append(m.Unsupported, kw)
		}
	}
	return m
}

// keywordLabel maps one JMAP keyword to its Gmail label, reporting false for
// anything Gmail cannot store.
func keywordLabel(keyword string) (string, bool) {
	switch keyword {
	case KeywordSeen:
		return LabelUnread, true // $seen's Gmail form is the inverse label
	case KeywordFlagged:
		return LabelStarred, true
	case KeywordDraft:
		return LabelDraft, true
	case KeywordImportant:
		return LabelImportant, true
	default:
		return "", false
	}
}

// DecodeSnippet turns Gmail's snippet into JMAP preview text: HTML entities are
// decoded and the invisible padding Google inserts (Unicode format characters
// and soft hyphens) is stripped, so a client never renders zero-width noise.
func DecodeSnippet(raw string) string {
	decoded := html.UnescapeString(raw)
	cleaned := strings.Map(func(r rune) rune {
		if r == '\u00ad' || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, decoded)
	return strings.TrimSpace(cleaned)
}

// Package keyword maps between IMAP system flags and JMAP keywords
// (FR-M.8). The mapping is bijective: the five system flags take their
// JMAP `$` spellings, every other flag travels verbatim in both
// directions, and nothing is ever silently dropped.
package keyword

// imapToJMAP is the system-flag half of the bijection.
var imapToJMAP = map[string]string{
	`\Seen`:     "$seen",
	`\Answered`: "$answered",
	`\Flagged`:  "$flagged",
	`\Deleted`:  "$deleted",
	`\Draft`:    "$draft",
}

var jmapToIMAP = func() map[string]string {
	m := make(map[string]string, len(imapToJMAP))
	for imap, j := range imapToJMAP {
		m[j] = imap
	}
	return m
}()

// FromIMAP converts an IMAP FLAGS list into a JMAP keyword set.
// System flags become `$` keywords; custom keywords and server flags
// (Gmail's `\Important`, `$Forwarded`, …) travel verbatim (FR-M.8).
func FromIMAP(flags []string) map[string]bool {
	out := make(map[string]bool, len(flags))
	for _, f := range flags {
		if j, ok := imapToJMAP[f]; ok {
			out[j] = true
		} else {
			out[f] = true
		}
	}
	return out
}

// ToIMAP converts a JMAP keyword set into an IMAP FLAGS list. `null` /
// absent keywords simply do not appear; `false` values are ignored the
// same way (the caller decides what removes, FR-M.9, M2).
func ToIMAP(keywords map[string]bool) []string {
	out := make([]string, 0, len(keywords))
	for k, v := range keywords {
		if !v {
			continue
		}
		if f, ok := jmapToIMAP[k]; ok {
			out = append(out, f)
		} else {
			out = append(out, k)
		}
	}
	return out
}

// SystemFlag reports the IMAP system flag for a JMAP keyword, or ""
// when the keyword is not one of the five (FR-M.8: custom keywords are
// not system flags).
func SystemFlag(jmapKeyword string) string { return jmapToIMAP[jmapKeyword] }

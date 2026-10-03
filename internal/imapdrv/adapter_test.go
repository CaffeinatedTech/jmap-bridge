package imapdrv

import "testing"

// gmailLabelFor's mapping table, including the two system folders
// SPECIAL-USE does not cover. The implicit mailbox is decided by the
// caller (store's implicit flag) before gmailLabelFor is consulted, so
// the archive role has no row here.
func TestGmailLabelFor(t *testing.T) {
	cases := []struct{ role, path, want string }{
		{"inbox", "INBOX", `\Inbox`},
		{"sent", "[Gmail]/Sent Mail", `\Sent`},
		{"drafts", "[Gmail]/Drafts", `\Draft`},
		{"trash", "[Gmail]/Trash", `\Trash`},
		{"junk", "[Gmail]/Spam", `\Spam`},
		{"", "receipts", "receipts"},
		{"", "work/2026", "work/2026"},
		{"", "[Gmail]/Starred", `\Starred`},
		{"", "[Gmail]/Important", `\Important`},
	}
	for _, tc := range cases {
		if got := gmailLabelFor(tc.role, tc.path); got != tc.want {
			t.Errorf("gmailLabelFor(%q,%q) = %q, want %q", tc.role, tc.path, got, tc.want)
		}
	}
}

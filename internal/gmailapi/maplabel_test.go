package gmailapi

import (
	"testing"

	"google.golang.org/api/gmail/v1"
)

// TestMapLabelHidesCategories pins the GMAIL_API_PLAN §6.1 mapping: the
// Gmail tab labels are hidden state, not JMAP mailboxes, while system
// roles and user labels are still surfaced.
func TestMapLabelHidesCategories(t *testing.T) {
	for _, id := range []string{
		"CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS",
		"CATEGORY_UPDATES", "CATEGORY_FORUMS",
	} {
		if name, role, ok := mapLabel(&gmail.Label{Id: id, Name: id}); ok {
			t.Fatalf("%s exposed as mailbox %q (role %q)", id, name, role)
		}
	}
	if name, role, ok := mapLabel(&gmail.Label{Id: "Label_1", Name: "Work"}); !ok || name != "Work" || role != "" {
		t.Fatalf("user label hidden: name=%q role=%q ok=%v", name, role, ok)
	}
	if name, role, ok := mapLabel(&gmail.Label{Id: LabelInbox, Name: "INBOX"}); !ok || name != "INBOX" || role != "inbox" {
		t.Fatalf("inbox mapping wrong: name=%q role=%q ok=%v", name, role, ok)
	}
}

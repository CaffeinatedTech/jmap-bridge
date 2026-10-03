package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// This file is the M12 gate in-process (PLAN §12, GMAIL_API_PLAN §8.1):
// compose → send through the Gmail API → exactly one Sent copy, and an
// ambiguous send reconciled by Message-ID instead of duplicated. The
// "sink" is the fixture's own accepted-sends list, which is Gmail filing
// the message it relayed.

// gapiSubmissionEnv is the API-mode engine plus the account context a
// submitting client sees (submission capability + identity, FR-M.14).
type gapiSubmissionEnv struct {
	*gapiEnv
	acct   *jmapapi.Account
	h      *jmapapi.Handler
	drafts string
	sent   string
}

func newGapiSubmissionEnv(t *testing.T, fx *fixturegmail.Server) *gapiSubmissionEnv {
	t.Helper()
	env := newGmailAPIEnv(t, fx)
	acct := &jmapapi.Account{
		ID:      "gapi",
		Store:   env.st,
		Backend: env.eng,
		Capabilities: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
			jmapapi.SubmissionURN,
		},
		Identity: &jmapapi.Identity{
			ID:    jmapapi.IdentityID("gapi"),
			Name:  "Gapi Tester",
			Email: "me@example.test",
		},
	}
	return &gapiSubmissionEnv{
		gapiEnv: env,
		acct:    acct,
		h:       jmapapi.NewHandler(env.st),
		drafts:  env.mailboxID(t, "Drafts"),
		sent:    env.mailboxID(t, "Sent"),
	}
}

// compose creates a draft the way a composing client does (FR-M.11).
func (s *gapiSubmissionEnv) compose(t *testing.T, subject string) string {
	t.Helper()
	res := jmap(t, s.acct, s.h, "Email/set", map[string]any{
		"accountId": "gapi",
		"create": map[string]any{"draft": map[string]any{
			"mailboxIds": map[string]bool{s.drafts: true},
			"keywords":   map[string]bool{"$draft": true, "$seen": true},
			"from":       []map[string]string{{"email": "me@example.test"}},
			"to":         []map[string]string{{"email": "sink@example.test"}},
			"subject":    subject,
			"textBody":   []map[string]any{{"partId": "1", "type": "text/plain"}},
			"bodyValues": map[string]any{"1": map[string]any{"value": "the gate body text"}},
		}},
	})
	id := createdID(t, res, "draft")
	if id == "" {
		t.Fatalf("compose: %v (notCreated=%v)", res.Args, setErrors(t, res, "notCreated"))
	}
	return id
}

func (s *gapiSubmissionEnv) submissionCall(emailID string, patch map[string]any) []any {
	args := map[string]any{
		"accountId": "gapi",
		"create": map[string]any{"sub": map[string]any{
			"identityId": s.acct.Identity.ID,
			"emailId":    emailID,
		}},
	}
	if patch != nil {
		args["onSuccessUpdateEmail"] = map[string]any{"#sub": patch}
	}
	return []any{"EmailSubmission/set", args, "1"}
}

// TestGmailAPISubmissionComposeSendFilesSent is the M12 gate: one batched
// compose-and-send, Gmail filing exactly one Sent copy, the caller's patch
// retiring the draft without a second copy.
func TestGmailAPISubmissionComposeSendFilesSent(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	s := newGapiSubmissionEnv(t, fx)

	batch := jmapBatch(t, s.acct, s.h,
		s.composeCall("gapi gate"),
		s.submissionCall("#draft", map[string]any{
			"mailboxIds/" + s.drafts: nil,
			"mailboxIds/" + s.sent:   true,
			"keywords/$draft":        nil,
		}),
	)
	if len(batch) != 3 {
		t.Fatalf("responses = %d, want draft + submission + implicit Email/set: %+v", len(batch), batch)
	}
	draftID := createdID(t, batch[0], "draft")
	if draftID == "" {
		t.Fatalf("draft create failed: %v", batch[0].Args)
	}
	if errs := setErrors(t, batch[1], "notCreated"); len(errs) > 0 {
		t.Fatalf("submission rejected: %+v", errs)
	}
	created, _ := batch[1].Args["created"].(map[string]any)
	entry, _ := created["sub"].(map[string]any)
	if entry == nil || entry["undoStatus"] != "final" {
		t.Fatalf("created submission = %v, want undoStatus final", created)
	}
	if batch[2].Name != "Email/set" {
		t.Fatalf("third response = %q, want the implicit Email/set", batch[2].Name)
	}
	if errs := setErrors(t, batch[2], "notUpdated"); len(errs) > 0 {
		t.Fatalf("patch rejected: %+v", errs)
	}

	if got := len(fx.Sent()); got != 1 {
		t.Fatalf("Gmail accepted %d sends, want exactly 1", got)
	}
	if got := len(fx.DraftIDs()); got != 0 {
		t.Fatalf("fixture still holds %d drafts, want 0 (drafts.send consumed it)", got)
	}
	// Exactly one Sent copy in the cache, and it is the composed email.
	waitUntil(t, 5*time.Second, "sent copy in cache", func() bool {
		return len(gapiEmailsIn(t, s, s.sent)) == 1
	})
	e := gapiEmailByID(t, s, draftID)
	if e == nil {
		t.Fatal("the submitted draft vanished from the cache")
	}
	if !contains(e.MailboxIDs, s.sent) {
		t.Errorf("mailboxIds = %v, want it in Sent", e.MailboxIDs)
	}
	if contains(e.MailboxIDs, s.drafts) {
		t.Errorf("mailboxIds = %v, still in Drafts", e.MailboxIDs)
	}
	if e.Keywords["$draft"] {
		t.Errorf("keywords = %v, want $draft cleared", e.Keywords)
	}
	if !e.Keywords["$seen"] {
		t.Errorf("keywords = %v, want $seen", e.Keywords)
	}
	// The All Mail sync pass must not have minted a second copy.
	waitUntil(t, 5*time.Second, "sent membership stable", func() bool {
		return len(gapiEmailsIn(t, s, s.sent)) == 1
	})
}

// TestGmailAPISubmissionWithoutPatchStillFilesSent covers the patch-less
// shape: Gmail already filed Sent, so the bridge must record the move
// rather than leave the draft stale until the next sync pass.
func TestGmailAPISubmissionWithoutPatchStillFilesSent(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{})
	gapiSeedLabels(fx)
	s := newGapiSubmissionEnv(t, fx)
	draftID := s.compose(t, "no patch")

	res := jmap(t, s.acct, s.h, "EmailSubmission/set", map[string]any{
		"accountId": "gapi",
		"create": map[string]any{"sub": map[string]any{
			"identityId": s.acct.Identity.ID,
			"emailId":    draftID,
		}},
	})
	if errs := setErrors(t, res, "notCreated"); len(errs) > 0 {
		t.Fatalf("submission rejected: %+v", errs)
	}
	if got := len(fx.Sent()); got != 1 {
		t.Fatalf("Gmail accepted %d sends, want 1", got)
	}
	waitUntil(t, 5*time.Second, "sent copy recorded", func() bool {
		return len(gapiEmailsIn(t, s, s.sent)) == 1
	})
	e := gapiEmailByID(t, s, draftID)
	if e == nil || !contains(e.MailboxIDs, s.sent) || contains(e.MailboxIDs, s.drafts) {
		t.Fatalf("submitted draft = %+v, want it moved to Sent", e)
	}
}

// TestGmailAPISubmissionAmbiguousReconciles proves the M12 reconciliation
// clause: the fixture accepts the send then answers 500, and the bridge
// reports success with exactly one Sent copy rather than retrying (which
// would duplicate the mail).
func TestGmailAPISubmissionAmbiguousReconciles(t *testing.T) {
	fx := fixturegmail.Start(t, fixturegmail.Options{Tier: fixturegmail.TierAmbiguousSend})
	gapiSeedLabels(fx)
	s := newGapiSubmissionEnv(t, fx)
	draftID := s.compose(t, "ambiguous")

	res := jmap(t, s.acct, s.h, "EmailSubmission/set", map[string]any{
		"accountId": "gapi",
		"create": map[string]any{"sub": map[string]any{
			"identityId": s.acct.Identity.ID,
			"emailId":    draftID,
		}},
	})
	if errs := setErrors(t, res, "notCreated"); len(errs) > 0 {
		t.Fatalf("ambiguous send not reconciled: %+v", errs)
	}
	if got := len(fx.Sent()); got != 1 {
		t.Fatalf("Gmail accepted %d sends, want exactly 1 (no duplicate)", got)
	}
}

// composeCall is the Email/set create half of a batched compose+submit.
func (s *gapiSubmissionEnv) composeCall(subject string) []any {
	return []any{"Email/set", map[string]any{
		"accountId": "gapi",
		"create": map[string]any{"draft": map[string]any{
			"mailboxIds": map[string]bool{s.drafts: true},
			"keywords":   map[string]bool{"$draft": true, "$seen": true},
			"from":       []map[string]string{{"email": "me@example.test"}},
			"to":         []map[string]string{{"email": "sink@example.test"}},
			"subject":    subject,
			"textBody":   []map[string]any{{"partId": "1", "type": "text/plain"}},
			"bodyValues": map[string]any{"1": map[string]any{"value": "the gate body text"}},
		}},
	}, "0"}
}

// gapiEmailsIn returns the live Gmail-API-mode emails that are members of
// mailboxID. API mode also holds every message in the synthetic All Mail
// archive, so membership is a contains check, not equality.
func gapiEmailsIn(t *testing.T, s *gapiSubmissionEnv, mailboxID string) []*jmapapi.Email {
	t.Helper()
	all, _, _, err := s.st.EmailsByID(context.Background(), "gapi", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	var out []*jmapapi.Email
	for _, e := range all {
		if contains(e.MailboxIDs, mailboxID) {
			out = append(out, e)
		}
	}
	return out
}

// gapiEmailByID returns one live Gmail-API-mode email, nil when absent.
func gapiEmailByID(t *testing.T, s *gapiSubmissionEnv, id string) *jmapapi.Email {
	t.Helper()
	all, _, _, err := s.st.EmailsByID(context.Background(), "gapi", []string{id}, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

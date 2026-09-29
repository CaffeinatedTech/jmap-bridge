package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/submit"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturesmtp"
)

// The M3 gate, in-process (PLAN §12): compose → send → the message is
// in Sent *and* the submission sink received it, attachments byte-exact
// — with a second IMAP session as the independent witness, exactly the
// shape the M2 gate used for writes.

const (
	submitUser = "submitter"
	submitPass = "submit-pass"
)

// submitEnv is the gate's rig: fixture IMAP tier, SMTP sink, engine
// wired to both, and the account context a submitting client sees
// (submission capability + identity, FR-M.14).
type submitEnv struct {
	*testEnv
	t        *testing.T
	sink     *fixturesmtp.Server
	acct     *jmapapi.Account
	h        *jmapapi.Handler
	draftsID string
	sentID   string
}

func newSubmitEnv(t *testing.T, sinkOpts fixturesmtp.Options) *submitEnv {
	t.Helper()
	if sinkOpts.Username == "" {
		sinkOpts.Username = submitUser
		sinkOpts.Password = submitPass
	}
	var sink *fixturesmtp.Server
	env := newEnv(t, fixtureimap.TierQResync, func(c *Config) {
		sink = fixturesmtp.Start(t, sinkOpts)
		c.SMTP = &submit.Config{
			Host:     sink.Host(),
			Port:     sink.Port(),
			TLS:      "none",
			Auth:     "password",
			Username: sinkOpts.Username,
			Password: sinkOpts.Password,
		}
	})
	acct, h := testAccount(env)
	acct.Capabilities = append(append([]string{}, acct.Capabilities...), jmapapi.SubmissionURN)
	acct.Identity = &jmapapi.Identity{
		ID:    jmapapi.IdentityID("acct"),
		Name:  "Bridge Tester",
		Email: "me@example.test",
	}
	s := &submitEnv{testEnv: env, t: t, sink: sink, acct: acct, h: h}
	s.draftsID = s.createMailbox("Drafts")
	s.sentID = s.createMailbox("Sent")
	return s
}

func (s *submitEnv) createMailbox(name string) string {
	s.t.Helper()
	res := jmap(s.t, s.acct, s.h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"m": map[string]any{"name": name, "parentId": nil}},
	})
	id := createdID(s.t, res, "m")
	if id == "" {
		s.t.Fatalf("create %s: %v (notCreated=%v)", name, res.Args, setErrors(s.t, res, "notCreated"))
	}
	return id
}

// compose creates a draft the way a composing client does: content
// properties only, in Drafts, with $draft set (FR-M.11). extra
// overrides or adds properties of the create.
func (s *submitEnv) compose(extra map[string]any) string {
	s.t.Helper()
	obj := map[string]any{
		"mailboxIds": map[string]bool{s.draftsID: true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []map[string]string{{"email": "me@example.test"}},
		"to":         []map[string]string{{"email": "you@example.test"}},
		"subject":    "M3 gate draft",
		"textBody":   []map[string]any{{"partId": "1", "type": "text/plain"}},
		"bodyValues": map[string]any{"1": map[string]any{"value": "the gate body text"}},
	}
	for k, v := range extra {
		obj[k] = v
	}
	res := jmap(s.t, s.acct, s.h, "Email/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"draft": obj},
	})
	id := createdID(s.t, res, "draft")
	if id == "" {
		s.t.Fatalf("compose: %v (notCreated=%v)", res.Args, setErrors(s.t, res, "notCreated"))
	}
	return id
}

// submissionCall is the EmailSubmission/set a composing client sends.
func (s *submitEnv) submissionCall(emailID string, patch map[string]any) []any {
	args := map[string]any{
		"accountId": "acct",
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

func (s *submitEnv) rawOf(emailID string) []byte {
	s.t.Helper()
	ctx := context.Background()
	blobID, err := s.st.RawBlobID(ctx, "acct", emailID)
	if err != nil || blobID == "" {
		s.t.Fatalf("raw blob of %s: %q %v", emailID, blobID, err)
	}
	raw, _, err := s.st.ReadBlob(ctx, "acct", blobID)
	if err != nil {
		s.t.Fatalf("read raw blob: %v", err)
	}
	return raw
}

// emailsIn returns every live email whose only mailbox is id.
func emailsIn(t *testing.T, env *testEnv, mailboxID string) []*jmapapi.Email {
	t.Helper()
	all, _, _, err := env.st.EmailsByID(context.Background(), "acct", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	var out []*jmapapi.Email
	for _, e := range all {
		if len(e.MailboxIDs) == 1 && e.MailboxIDs[0] == mailboxID {
			out = append(out, e)
		}
	}
	return out
}

// TestSubmissionComposeSendFilesSent is the M3 gate: one batched
// compose-and-send, the sink holding the message byte for byte, the
// second IMAP session seeing it in Sent with \Seen — and the caller's
// onSuccessUpdateEmail patch doing the filing, so Sent ends with exactly
// one copy (RFC 8621 §7.5, PLAN §7.2).
func TestSubmissionComposeSendFilesSent(t *testing.T) {
	s := newSubmitEnv(t, fixturesmtp.Options{})
	ctx := context.Background()

	// Upload an attachment (FR-M.16's store half; the HTTP endpoint is
	// pinned in httpapi): binary bytes, CRLF, NUL — nothing a text path
	// would survive.
	payload := []byte{0x00, 0x01, 0x02, '\r', '\n', 0xff, 0xfe, 'e', 'n', 'd'}
	blobID, err := s.st.PutBlob(ctx, "acct", "application/octet-stream", payload)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	draftCreate := []any{"Email/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"draft": map[string]any{
			"mailboxIds":  map[string]bool{s.draftsID: true},
			"keywords":    map[string]bool{"$draft": true, "$seen": true},
			"from":        []map[string]string{{"email": "me@example.test"}},
			"to":          []map[string]string{{"email": "you@example.test"}},
			"cc":          []map[string]string{{"email": "cc@example.test"}},
			"bcc":         []map[string]string{{"email": "bcc@example.test"}},
			"subject":     "M3 gate send",
			"textBody":    []map[string]any{{"partId": "1", "type": "text/plain"}},
			"bodyValues":  map[string]any{"1": map[string]any{"value": "gate body"}},
			"attachments": []map[string]any{{"blobId": blobID, "type": "application/octet-stream", "name": "gate.bin"}},
		}},
	}, "0"}
	submission := s.submissionCall("#draft", map[string]any{
		"mailboxIds/" + s.draftsID: nil,
		"mailboxIds/" + s.sentID:   true,
		"keywords/$draft":          nil,
	})

	batch := jmapBatch(t, s.acct, s.h, draftCreate, submission)
	if len(batch) != 3 {
		t.Fatalf("responses = %d, want draft + submission + implicit Email/set: %+v", len(batch), batch)
	}

	// --- the draft ---
	draftID := createdID(t, batch[0], "draft")
	if draftID == "" {
		t.Fatalf("draft create failed: %v", batch[0].Args)
	}

	// --- the submission ---
	if batch[1].Name != "EmailSubmission/set" {
		t.Fatalf("second response = %q, want EmailSubmission/set", batch[1].Name)
	}
	if errs := setErrors(t, batch[1], "notCreated"); len(errs) > 0 {
		t.Fatalf("submission rejected: %+v", errs)
	}
	created, _ := batch[1].Args["created"].(map[string]any)
	entry, _ := created["sub"].(map[string]any)
	if entry == nil {
		t.Fatalf("created = %v", batch[1].Args["created"])
	}
	if undo, _ := entry["undoStatus"].(string); undo != "final" {
		t.Errorf("undoStatus = %v, want final (FR-M.15: it is already relayed)", entry["undoStatus"])
	}
	if got, _ := entry["emailId"].(string); got != draftID {
		t.Errorf("emailId = %q, want the draft %q", got, draftID)
	}
	if got, _ := entry["identityId"].(string); got != s.acct.Identity.ID {
		t.Errorf("identityId = %q, want %q", got, s.acct.Identity.ID)
	}
	if _, ok := entry["id"].(string); !ok {
		t.Errorf("submission has no id: %v", entry)
	}

	// --- the implicit Email/set (RFC 8621 §7.5) ---
	implicit := batch[2]
	if implicit.Name != "Email/set" {
		t.Fatalf("third response = %q, want the implicit Email/set", implicit.Name)
	}
	if implicit.CallID != batch[1].CallID {
		t.Errorf("implicit call id = %q, want the submission's %q (Stalwart's shape, PLAN §2.1)",
			implicit.CallID, batch[1].CallID)
	}
	updated := updatedIDs(t, implicit)
	if len(updated) != 1 || updated[0] != draftID {
		t.Errorf("implicit updated = %v, want [%s]", updated, draftID)
	}
	if errs := setErrors(t, implicit, "notUpdated"); len(errs) > 0 {
		t.Errorf("patch rejected: %+v", errs)
	}

	// --- the sink has it, byte for byte ---
	msgs := s.sink.Messages()
	if len(msgs) != 1 {
		t.Fatalf("sink holds %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.From != "me@example.test" {
		t.Errorf("MAIL FROM = %q, want the identity's address", msg.From)
	}
	wantRcpt := []string{"you@example.test", "cc@example.test", "bcc@example.test"}
	if strings.Join(msg.Recipients, ",") != strings.Join(wantRcpt, ",") {
		t.Errorf("RCPT TO = %v, want %v (To, Cc and Bcc all go on the envelope)", msg.Recipients, wantRcpt)
	}
	if bytes.Contains(msg.Data, []byte("Bcc:")) {
		t.Error("delivered message still carries Bcc (RFC 8621 §7.5: strip it during delivery)")
	}
	want := convert.StripBcc(s.rawOf(draftID))
	if !bytes.Equal(msg.Data, want) {
		t.Errorf("delivered bytes differ from the submitted draft:\n got %d bytes\nwant %d",
			len(msg.Data), len(want))
	}
	// The attachment survives the whole trip: uploaded blob → MIME part
	// → SMTP → the sink's parser, unchanged (the M3 gate's byte-exact
	// clause).
	body, err := convert.ParseBody(msg.Data)
	if err != nil {
		t.Fatalf("parse delivered message: %v", err)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("delivered attachments = %d, want 1", len(body.Attachments))
	}
	if !bytes.Equal(body.Attachments[0].Data, payload) {
		t.Errorf("attachment bytes = %x, want %x", body.Attachments[0].Data, payload)
	}

	// --- the second IMAP client sees one message in Sent ---
	waitUntil(t, 10*time.Second, "sent copy visible to the second client", func() bool {
		return len(uidsVia(t, s.testEnv, "Sent")) == 1 && len(uidsVia(t, s.testEnv, "Drafts")) == 0
	})
	sentUID := uidsVia(t, s.testEnv, "Sent")[0]
	flags := flagsVia(t, s.testEnv, "Sent", sentUID)
	if !hasFlagName(flags, `\Seen`) {
		t.Errorf("sent flags = %v, want \\Seen (PLAN §7.2)", flags)
	}
	if hasFlagName(flags, `\Draft`) {
		t.Errorf("sent flags = %v, must not still be \\Draft", flags)
	}

	// --- and the cache agrees: one copy, in Sent, no longer a draft ---
	e := emailByID(t, s.testEnv, draftID)
	if e == nil {
		t.Fatal("the submitted draft vanished from the cache")
	}
	if len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != s.sentID {
		t.Errorf("mailboxIds = %v, want the Sent mailbox only", e.MailboxIDs)
	}
	if e.Keywords["$draft"] {
		t.Errorf("keywords = %v, want $draft cleared by the patch", e.Keywords)
	}
	if !e.Keywords["$seen"] {
		t.Errorf("keywords = %v, want $seen kept", e.Keywords)
	}
}

// TestSubmissionWithoutPatchAppendsSentCopy covers the other filing
// shape: a client that says nothing about where the message belongs
// gets the bridge's own Sent copy (PLAN §7.2), filed separately from the
// draft so retiring the draft cannot take it away.
func TestSubmissionWithoutPatchAppendsSentCopy(t *testing.T) {
	s := newSubmitEnv(t, fixturesmtp.Options{})
	draftID := s.compose(nil)

	res := jmap(t, s.acct, s.h, "EmailSubmission/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"sub": map[string]any{
			"identityId": s.acct.Identity.ID,
			"emailId":    draftID,
		}},
	})
	if errs := setErrors(t, res, "notCreated"); len(errs) > 0 {
		t.Fatalf("submission rejected: %+v", errs)
	}
	if n := len(s.sink.Messages()); n != 1 {
		t.Fatalf("sink holds %d messages, want 1", n)
	}

	waitUntil(t, 10*time.Second, "sent copy filed", func() bool {
		return len(emailsIn(t, s.testEnv, s.sentID)) == 1
	})
	if got := len(uidsVia(t, s.testEnv, "Sent")); got != 1 {
		t.Errorf("second session sees %d messages in Sent, want 1", got)
	}
	// The draft the client still owns is untouched.
	e := emailByID(t, s.testEnv, draftID)
	if len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != s.draftsID {
		t.Errorf("draft mailboxIds = %v, want it left in Drafts", e.MailboxIDs)
	}
	if !e.Keywords["$draft"] {
		t.Errorf("draft keywords = %v, want $draft kept (only the client retires a draft)", e.Keywords)
	}
	// The Sent record is a separate Email sharing the submitted bytes,
	// so Email/get answers blobId for it without storing them twice.
	sent := emailsIn(t, s.testEnv, s.sentID)[0]
	if sent.ID == draftID {
		t.Error("the Sent copy reused the draft's id; a client destroying the draft would lose it")
	}
	draftBlob, _ := s.st.RawBlobID(context.Background(), "acct", draftID)
	sentBlob, _ := s.st.RawBlobID(context.Background(), "acct", sent.ID)
	if draftBlob == "" || draftBlob != sentBlob {
		t.Errorf("raw blobs = draft %q / sent %q, want the same cached bytes", draftBlob, sentBlob)
	}
	if !bytes.Equal(s.rawOf(sent.ID), s.rawOf(draftID)) {
		t.Error("Sent copy bytes differ from the submitted message")
	}
}

// TestSubmissionSMTPFailureFilesNothing pins FR-M.15's failure half: the
// SMTP reply is the SetError description, nothing lands in Sent, no
// patch runs, and there is no implicit Email/set to run it in.
func TestSubmissionSMTPFailureFilesNothing(t *testing.T) {
	s := newSubmitEnv(t, fixturesmtp.Options{RejectRCPT: "550 5.1.1 no such recipient"})
	draftID := s.compose(nil)

	batch := jmapBatch(t, s.acct, s.h, s.submissionCall(draftID, map[string]any{
		"mailboxIds/" + s.draftsID: nil,
		"mailboxIds/" + s.sentID:   true,
		"keywords/$draft":          nil,
	}))
	if len(batch) != 1 {
		t.Fatalf("responses = %d, want only the submission (no implicit Email/set for a failed create)", len(batch))
	}
	errs := setErrors(t, batch[0], "notCreated")
	se, ok := errs["sub"]
	if !ok {
		t.Fatalf("no notCreated: %v", batch[0].Args)
	}
	if se.Type != "serverFail" {
		t.Errorf("SetError type = %q, want serverFail", se.Type)
	}
	if se.Description == nil || !strings.Contains(*se.Description, "550") ||
		!strings.Contains(*se.Description, "no such recipient") {
		t.Errorf("description = %v, want the SMTP reply", se.Description)
	}
	if n := len(s.sink.Messages()); n != 0 {
		t.Errorf("sink holds %d messages, want none", n)
	}
	if got := len(uidsVia(t, s.testEnv, "Sent")); got != 0 {
		t.Errorf("Sent holds %d messages, want none for a refused submission", got)
	}
	e := emailByID(t, s.testEnv, draftID)
	if len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != s.draftsID || !e.Keywords["$draft"] {
		t.Errorf("draft after a failed send = mailboxes %v keywords %v, want it untouched",
			e.MailboxIDs, e.Keywords)
	}
}

// TestSubmissionArgumentValidation checks the refusals RFC 8621 §7.5
// defines, all of them before a byte leaves for SMTP.
func TestSubmissionArgumentValidation(t *testing.T) {
	s := newSubmitEnv(t, fixturesmtp.Options{})
	draftID := s.compose(nil)

	for _, tc := range []struct {
		name  string
		args  map[string]any
		want  string
		props []string
	}{
		{
			name: "onSuccessDestroyEmail is unsupported",
			args: map[string]any{
				"accountId":             "acct",
				"create":                map[string]any{"sub": map[string]any{"identityId": s.acct.Identity.ID, "emailId": draftID}},
				"onSuccessDestroyEmail": []string{"#sub"},
			},
			want: "invalidProperties",
		},
		{
			name: "unknown emailId",
			args: map[string]any{
				"accountId": "acct",
				"create":    map[string]any{"sub": map[string]any{"identityId": s.acct.Identity.ID, "emailId": "nope"}},
			},
			want: "invalidProperties",
		},
		{
			name: "unknown identity",
			args: map[string]any{
				"accountId": "acct",
				"create":    map[string]any{"sub": map[string]any{"identityId": "nope", "emailId": draftID}},
			},
			want: "invalidProperties",
		},
		{
			name: "delayed send is not supported",
			args: map[string]any{
				"accountId": "acct",
				"create": map[string]any{"sub": map[string]any{
					"identityId": s.acct.Identity.ID, "emailId": draftID,
					"sendAt": "2030-01-01T00:00:00Z",
				}},
			},
			want: "invalidProperties",
		},
		{
			name: "missing identityId",
			args: map[string]any{
				"accountId": "acct",
				"create":    map[string]any{"sub": map[string]any{"emailId": draftID}},
			},
			want: "invalidProperties",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := jmapTry(s.acct, s.h, "EmailSubmission/set", tc.args)
			if err == nil {
				if errs := setErrors(t, res, "notCreated"); len(errs) > 0 {
					if errs["sub"].Type != tc.want {
						t.Fatalf("notCreated = %+v, want %s", errs["sub"], tc.want)
					}
				} else {
					t.Fatalf("accepted: %v", res.Args)
				}
				return
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %s", err, tc.want)
			}
		})
	}
	if n := len(s.sink.Messages()); n != 0 {
		t.Errorf("sink holds %d messages; a rejected submission must never reach SMTP", n)
	}
}

// TestIdentityGetServesTheConfiguredIdentity pins FR-M.14: one stable
// identity from the account's address and display name, mayDelete false,
// and nothing at all for an account that cannot send.
func TestIdentityGetServesTheConfiguredIdentity(t *testing.T) {
	s := newSubmitEnv(t, fixturesmtp.Options{})
	res := jmap(t, s.acct, s.h, "Identity/get", map[string]any{"accountId": "acct"})
	list, _ := res.Args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want exactly one identity", res.Args["list"])
	}
	obj, _ := list[0].(map[string]any)
	if obj["email"] != "me@example.test" || obj["name"] != "Bridge Tester" {
		t.Errorf("identity = %v", obj)
	}
	if obj["mayDelete"] != false {
		t.Errorf("mayDelete = %v, want false (FR-M.14)", obj["mayDelete"])
	}
	if id, _ := obj["id"].(string); id != jmapapi.IdentityID("acct") {
		t.Errorf("id = %v, want the stable derivation", obj["id"])
	}
	if state, _ := res.Args["state"].(string); state == "" {
		t.Error("Identity/get returned no state")
	}
	// A second call answers the same id: it is derived, never minted.
	again := jmap(t, s.acct, s.h, "Identity/get", map[string]any{"accountId": "acct"})
	againList, _ := again.Args["list"].([]any)
	againObj, _ := againList[0].(map[string]any)
	if againObj["id"] != obj["id"] {
		t.Errorf("identity id is not stable: %v then %v", obj["id"], againObj["id"])
	}
}

// TestSubmissionNeedsTheCapability pins the gate of FR-M.14/FR-J.5: an
// account with no SMTP behind it neither offers the capability in
// `using` nor answers the two methods.
func TestSubmissionNeedsTheCapability(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env) // mailCaps, no identity: no SMTP configured

	if _, err := jmapTry(acct, h, "Identity/get", map[string]any{"accountId": "acct"}); err == nil ||
		!strings.Contains(err.Error(), "accountNotSupportedByMethod") {
		t.Errorf("Identity/get without SMTP = %v, want accountNotSupportedByMethod", err)
	}
	if _, err := jmapTry(acct, h, "EmailSubmission/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"sub": map[string]any{"identityId": "x", "emailId": "y"}},
	}); err == nil || !strings.Contains(err.Error(), "accountNotSupportedByMethod") {
		t.Errorf("EmailSubmission/set without SMTP = %v, want accountNotSupportedByMethod", err)
	}

	// Naming the capability in `using` is unknown for this account —
	// exactly what a client that ignored the session deserves (FR-J.5).
	body, err := json.Marshal(map[string]any{
		"using":       []string{jmapapi.SubmissionURN},
		"methodCalls": []any{[]any{"Identity/get", map[string]any{"accountId": "acct"}, "c1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if status != 400 || !strings.Contains(string(raw), "unknownCapability") {
		t.Errorf("using=[submission] → HTTP %d %s, want 400 unknownCapability", status, raw)
	}
}

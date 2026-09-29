package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/submit"
	bridgesync "github.com/CaffeinatedTech/jmap-bridge/internal/sync"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixturesmtp"
)

// The M3 live gate (PLAN §12): compose → send → the message is in Sent
// *and* the submission sink received it, attachments byte-exact — driven
// through the real JMAP handlers against the real IMAP server, with an
// in-process SMTP sink standing in for the MTA so no message ever
// leaves this machine. Skips unless JMAP_BRIDGE_TEST_IMAP_* is set
// (AGENTS.md).

const m3Subject = "M3 gate send"

// m3Batch runs several method calls in one request — the shape a
// composing client sends (RFC 8620 §3.4) — and returns every response.
func m3Batch(t *testing.T, acct *jmapapi.Account, h *jmapapi.Handler, calls ...[]any) []map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"using": acct.Capabilities, "methodCalls": calls})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if status != 200 {
		t.Fatalf("batch: HTTP %d: %s", status, raw)
	}
	var wire struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	out2 := make([]map[string]any, 0, len(wire.MethodResponses))
	for _, triple := range wire.MethodResponses {
		var entry map[string]any
		if err := json.Unmarshal(triple[1], &entry); err != nil {
			t.Fatalf("decode response args: %v", err)
		}
		var name string
		_ = json.Unmarshal(triple[0], &name)
		entry["__name"] = name
		out2 = append(out2, entry)
	}
	return out2
}

// TestLiveSendComposeFilesSent is the M3 gate, live side.
func TestLiveSendComposeFilesSent(t *testing.T) {
	cfg := liveIMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	sink := fixturesmtp.Start(t, fixturesmtp.Options{
		Username: "m3-gate", Password: "m3-gate-pass",
	})
	verifier := readConn(t, cfg)

	// Fresh store + engine + JMAP handler against the real account; the
	// engine gets the sink as its submission server.
	changes := make(chan struct{}, 16)
	st, err := store.Open(ctx, store.Options{
		DataDir: t.TempDir(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Publish: func(string) {
			select {
			case changes <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := bridgesync.New(bridgesync.Config{
		Account: "livetest", IMAP: cfg,
		SMTP: &submit.Config{
			Host: sink.Host(), Port: sink.Port(), TLS: "none",
			Auth: "password", Username: "m3-gate", Password: "m3-gate-pass",
		},
		Interval: time.Hour, BatchSize: 200, PrefetchWindow: 0, Concurrency: 2,
	}, st, quiet)
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	go eng.Run(engCtx)

	acct := &jmapapi.Account{
		ID: "livetest", Store: st, Backend: eng,
		Capabilities: []string{
			"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail",
			"urn:ietf:params:jmap:submission",
		},
		Identity: &jmapapi.Identity{
			ID:    jmapapi.IdentityID("livetest"),
			Name:  "M3 gate",
			Email: "gate@example.test",
		},
	}
	h := jmapapi.NewHandler(st)

	// Discovery must know the account's folders before anything can be
	// filed into them.
	waitFor(t, 90*time.Second, "folders discovered", func() bool {
		return liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == "inbox" }) != nil
	})
	drafts := ensureRoleFolder(t, ctx, st, acct, h, "drafts", "Drafts")
	sent := ensureRoleFolder(t, ctx, st, acct, h, "sent", "Sent")
	draftsPath := mailboxPathOf(t, ctx, st, drafts.ID)
	sentPath := mailboxPathOf(t, ctx, st, sent.ID)

	// Idempotent setup: an earlier run's message must not satisfy a
	// "the message is here" assertion by accident (live rules: clean up
	// after yourself).
	expungeBySubject(t, verifier, draftsPath, m3Subject)
	expungeBySubject(t, verifier, sentPath, m3Subject)
	t.Cleanup(func() {
		expungeBySubject(t, verifier, draftsPath, m3Subject)
		expungeBySubject(t, verifier, sentPath, m3Subject)
	})

	// --- compose: upload an attachment, create the draft ---
	payload := []byte{0x00, 0x01, '\r', '\n', 0xff, 0xfe, 'm', '3'}
	blobID, err := st.PutBlob(ctx, "livetest", "application/octet-stream", payload)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	batch := m3Batch(t, acct, h,
		[]any{"Email/set", map[string]any{
			"accountId": "livetest",
			"create": map[string]any{"draft": map[string]any{
				"mailboxIds":  map[string]bool{drafts.ID: true},
				"keywords":    map[string]bool{"$draft": true, "$seen": true},
				"from":        []map[string]string{{"email": "gate@example.test"}},
				"to":          []map[string]string{{"email": "sink@example.test"}},
				"bcc":         []map[string]string{{"email": "hidden@example.test"}},
				"subject":     m3Subject,
				"textBody":    []map[string]any{{"partId": "1", "type": "text/plain"}},
				"bodyValues":  map[string]any{"1": map[string]any{"value": "M3 gate body"}},
				"attachments": []map[string]any{{"blobId": blobID, "type": "application/octet-stream", "name": "gate.bin"}},
			}},
		}, "0"},
		[]any{"EmailSubmission/set", map[string]any{
			"accountId": "livetest",
			"create": map[string]any{"sub": map[string]any{
				"identityId": acct.Identity.ID,
				"emailId":    "#draft",
			}},
			"onSuccessUpdateEmail": map[string]any{"#sub": map[string]any{
				"mailboxIds/" + drafts.ID: nil,
				"mailboxIds/" + sent.ID:   true,
				"keywords/$draft":         nil,
			}},
		}, "1"},
	)
	if len(batch) < 2 || batch[1]["__name"] != "EmailSubmission/set" {
		t.Fatalf("responses = %d (%v), want a draft then the submission: %v", len(batch), names(batch), batch)
	}
	if errs := setErrEntries(t, batch[0], "notCreated"); len(errs) > 0 {
		t.Fatalf("draft create rejected: %s", describeSetErrors(errs))
	}
	draftID := createdIDOf(t, batch[0], "draft")
	if errs := setErrEntries(t, batch[1], "notCreated"); len(errs) > 0 {
		t.Fatalf("submission rejected: %s", describeSetErrors(errs))
	}
	if len(batch) != 3 {
		t.Fatalf("responses = %d (%v), want the implicit Email/set as the third (RFC 8621 §7.5): %v",
			len(batch), names(batch), batch)
	}
	created, _ := batch[1]["created"].(map[string]any)
	entry, _ := created["sub"].(map[string]any)
	if undo, _ := entry["undoStatus"].(string); undo != "final" {
		t.Errorf("undoStatus = %v, want final", entry["undoStatus"])
	}
	if batch[2]["__name"] != "Email/set" {
		t.Fatalf("third response = %v, want the implicit Email/set (RFC 8621 §7.5)", batch[2]["__name"])
	}
	if updated := stringList(batch[2]["updated"]); len(updated) != 1 || updated[0] != draftID {
		t.Errorf("implicit updated = %v, want [%s]", updated, draftID)
	}
	t.Log("submission accepted and its patch applied")

	// --- the sink received it ---
	msgs := sink.Messages()
	if len(msgs) != 1 {
		t.Fatalf("sink holds %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.From != "gate@example.test" {
		t.Errorf("MAIL FROM = %q", msg.From)
	}
	if strings.Join(msg.Recipients, ",") != "sink@example.test,hidden@example.test" {
		t.Errorf("RCPT TO = %v, want To and Bcc on the envelope", msg.Recipients)
	}
	if bytes.Contains(msg.Data, []byte("Bcc:")) {
		t.Error("delivered message still carries Bcc (RFC 8621 §7.5)")
	}
	rawBlob, err := st.RawBlobID(ctx, "livetest", draftID)
	if err != nil || rawBlob == "" {
		t.Fatalf("draft raw blob: %q %v", rawBlob, err)
	}
	draftRaw, _, err := st.ReadBlob(ctx, "livetest", rawBlob)
	if err != nil {
		t.Fatalf("read draft raw: %v", err)
	}
	if want := convert.StripBcc(draftRaw); !bytes.Equal(msg.Data, want) {
		t.Errorf("delivered bytes = %d, want %d (byte-exact)", len(msg.Data), len(want))
	}
	body, err := convert.ParseBody(msg.Data)
	if err != nil {
		t.Fatalf("parse delivered message: %v", err)
	}
	if len(body.Attachments) != 1 || !bytes.Equal(body.Attachments[0].Data, payload) {
		t.Fatalf("attachments = %d bytes, want the uploaded payload byte-exact", len(body.Attachments))
	}
	t.Log("sink received the message with the attachment byte-exact")

	// --- an independent IMAP session sees it in Sent, not Drafts ---
	waitFor(t, 30*time.Second, "sent copy visible to the second client", func() bool {
		_, inSent := uidWithSubject(t, verifier, sentPath, m3Subject)
		_, inDrafts := uidWithSubject(t, verifier, draftsPath, m3Subject)
		return inSent && !inDrafts
	})
	uid, _ := uidWithSubject(t, verifier, sentPath, m3Subject)
	flags := flagsAt(t, verifier, sentPath, uid)
	if !flagHas(flags, `\Seen`) {
		t.Errorf("sent flags = %v, want \\Seen", flags)
	}
	if flagHas(flags, `\Draft`) {
		t.Errorf("sent flags = %v, must not still be \\Draft", flags)
	}

	// --- and the cache agrees ---
	e := liveEmail(t, ctx, st, draftID)
	if e == nil {
		t.Fatal("the submitted draft vanished from the cache")
	}
	if len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != sent.ID {
		t.Errorf("mailboxIds = %v, want the Sent mailbox only", e.MailboxIDs)
	}
	if e.Keywords["$draft"] {
		t.Errorf("keywords = %v, want $draft cleared", e.Keywords)
	}
	t.Log("M3 live gate: compose → send → Sent, sink delivered, attachments byte-exact")
}

// ensureRoleFolder resolves the mailbox with role, creating name when
// the account has none (roles come from the well-known name on the
// refresh that follows CREATE).
func ensureRoleFolder(t *testing.T, ctx context.Context, st *store.Store,
	acct *jmapapi.Account, h *jmapapi.Handler, role, name string,
) *jmapapi.Mailbox {
	t.Helper()
	if mb := liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == role }); mb != nil {
		return mb
	}
	liveSet(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "livetest",
		"create":    map[string]any{"m": map[string]any{"name": name, "parentId": nil}},
	})
	waitFor(t, 60*time.Second, name+" role", func() bool {
		return liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == role }) != nil
	})
	mb := liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == role })
	if mb == nil {
		t.Fatalf("no mailbox with role %q after creating %s", role, name)
	}
	return mb
}

func mailboxPathOf(t *testing.T, ctx context.Context, st *store.Store, id string) string {
	t.Helper()
	path, err := st.MailboxPath(ctx, "livetest", id)
	if err != nil {
		t.Fatalf("mailbox path: %v", err)
	}
	return path
}

// setErrEntries decodes one of notCreated/notUpdated/notDestroyed.
func setErrEntries(t *testing.T, res map[string]any, field string) map[string]jmapapi.SetError {
	t.Helper()
	raw, ok := res[field]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal %s: %v", field, err)
	}
	var out map[string]jmapapi.SetError
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	return out
}

// describeSetErrors renders a /set failure map with its descriptions
// dereferenced — the pointer form prints as a memory address.
func describeSetErrors(errs map[string]jmapapi.SetError) string {
	var b strings.Builder
	for handle, se := range errs {
		fmt.Fprintf(&b, "%s: %s", handle, se.Type)
		if se.Description != nil {
			fmt.Fprintf(&b, " — %s", *se.Description)
		}
		b.WriteString("; ")
	}
	return b.String()
}

func names(batch []map[string]any) []string {
	out := make([]string, 0, len(batch))
	for _, e := range batch {
		name, _ := e["__name"].(string)
		out = append(out, name)
	}
	return out
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

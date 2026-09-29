package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

// These tests run the whole write path the way production does — JMAP
// handler, real SQLite store, engine backend, fixture IMAP server — and
// verify every mutation through a *second* IMAP session, which is the
// M2 gate's "visible from another client" half in-process (PLAN §12).

// mailCaps is the capability set testAccount offers: core and mail,
// with submission joining once an M3 test configures SMTP (FR-J.5).
var mailCaps = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}

type jmapResult struct {
	Name   string
	CallID string
	Args   map[string]any
	Raw    json.RawMessage
}

// jmap runs one method call and returns its response. A method-level
// error fails the test unless the caller asks for it with wantError.
func jmap(t *testing.T, acct *jmapapi.Account, h *jmapapi.Handler, method string, args any) jmapResult {
	t.Helper()
	res, err := jmapTry(acct, h, method, args)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func jmapTry(acct *jmapapi.Account, h *jmapapi.Handler, method string, args any) (jmapResult, error) {
	out, err := dispatchBatch(acct, h, []any{method, args, "c1"})
	if err != nil {
		return jmapResult{}, err
	}
	if len(out) != 1 {
		return jmapResult{}, &dispatchErr{status: 200, body: fmt.Sprintf("got %d responses, want 1", len(out))}
	}
	if out[0].Name == "error" {
		return jmapResult{}, &dispatchErr{status: 200, body: string(out[0].Raw)}
	}
	return out[0], nil
}

// jmapBatch runs several method calls in one request — the shape a
// composing client sends: create the draft and submit it together, so
// the submission can name the draft "#draft" (RFC 8620 §3.4, §3.7).
// Responses come back in order, including any implicit call the server
// appended (RFC 8621 §7.5). A method-level error is a response here,
// not a Go error: a batch is not atomic.
func jmapBatch(t *testing.T, acct *jmapapi.Account, h *jmapapi.Handler, calls ...[]any) []jmapResult {
	t.Helper()
	out, err := dispatchBatch(acct, h, calls...)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	return out
}

// dispatchBatch marshals the calls, dispatches them, and decodes every
// response triple. `using` is derived from the account's own
// capabilities — exactly what a client that read the session sends.
func dispatchBatch(acct *jmapapi.Account, h *jmapapi.Handler, calls ...[]any) ([]jmapResult, error) {
	body, err := json.Marshal(map[string]any{
		"using":       acct.Capabilities,
		"methodCalls": calls,
	})
	if err != nil {
		return nil, err
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &dispatchErr{status: status, body: string(raw)}
	}
	var wire struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	results := make([]jmapResult, 0, len(wire.MethodResponses))
	for _, triple := range wire.MethodResponses {
		if len(triple) != 3 {
			return nil, &dispatchErr{status: status, body: string(raw)}
		}
		var name, callID string
		_ = json.Unmarshal(triple[0], &name)
		_ = json.Unmarshal(triple[2], &callID)
		res := jmapResult{Name: name, CallID: callID, Raw: triple[1]}
		if name != "error" {
			if err := json.Unmarshal(triple[1], &res.Args); err != nil {
				return nil, err
			}
		} else {
			_ = json.Unmarshal(triple[1], &res.Args)
		}
		results = append(results, res)
	}
	return results, nil
}

type dispatchErr struct {
	status int
	body   string
}

func (e *dispatchErr) Error() string {
	return "method error (HTTP " + strconv.Itoa(e.status) + "): " + e.body
}

// setErrors reads one of notCreated/notUpdated/notDestroyed.
func setErrors(t *testing.T, res jmapResult, key string) map[string]jmapapi.SetError {
	t.Helper()
	raw, ok := res.Args[key]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	var out map[string]jmapapi.SetError
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return out
}

func idList(t *testing.T, res jmapResult, key string) []string {
	t.Helper()
	raw, ok := res.Args[key]
	if !ok || raw == nil {
		return nil
	}
	b, _ := json.Marshal(raw)
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return out
}

// testAccount builds the per-request account context production builds.
func testAccount(env *testEnv) (*jmapapi.Account, *jmapapi.Handler) {
	return &jmapapi.Account{
			ID: "acct", Store: env.st, Backend: env.eng, Capabilities: mailCaps,
		},
		jmapapi.NewHandler(env.st)
}

// secondConn opens an independent IMAP session — the gate's "second
// client" (M2 requires every write to be visible there).
func secondConn(t *testing.T, env *testEnv) *imapdrv.Conn {
	t.Helper()
	host, port := splitAddr(t, env.fx.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := imapdrv.Dial(ctx, imapdrv.Config{
		Host: host, Port: port, Username: "test", Password: "test-pass",
	})
	if err != nil {
		t.Fatalf("second session dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// flagsVia reads one message's flags through the second session.
func flagsVia(t *testing.T, env *testEnv, folder string, uid uint32) []string {
	t.Helper()
	conn := secondConn(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Examine(ctx, folder, nil); err != nil {
		t.Fatalf("second session examine %s: %v", folder, err)
	}
	all, err := conn.AllFlags(ctx)
	if err != nil {
		t.Fatalf("second session flags: %v", err)
	}
	for _, ch := range all {
		if ch.UID == uid {
			return ch.Flags
		}
	}
	t.Fatalf("uid %d not found in %s (second session sees %d messages)", uid, folder, len(all))
	return nil
}

// uidsVia lists a folder through the second session.
func uidsVia(t *testing.T, env *testEnv, folder string) []uint32 {
	t.Helper()
	conn := secondConn(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Examine(ctx, folder, nil); err != nil {
		t.Fatalf("second session examine %s: %v", folder, err)
	}
	uids, err := conn.UIDs(ctx)
	if err != nil {
		t.Fatalf("second session uids: %v", err)
	}
	return uids
}

func hasFlagName(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// mailboxID finds a mailbox by role or name after discovery.
func mailboxBy(t *testing.T, env *testEnv, match func(*jmapapi.Mailbox) bool) *jmapapi.Mailbox {
	t.Helper()
	mbs, _, err := env.st.Mailboxes(context.Background(), "acct")
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	for _, mb := range mbs {
		if match(mb) {
			return mb
		}
	}
	return nil
}

func emailByID(t *testing.T, env *testEnv, id string) *jmapapi.Email {
	t.Helper()
	emails, _, notFound, err := env.st.EmailsByID(context.Background(), "acct", []string{id}, false)
	if err != nil {
		t.Fatalf("email get: %v", err)
	}
	if len(notFound) > 0 || len(emails) == 0 {
		return nil
	}
	return emails[0]
}

func seedInbox(t *testing.T, env *testEnv, subject, msgid string) (uid uint32, emailID string) {
	t.Helper()
	uid, _ = env.fx.Append("INBOX", raw(subject, msgid, "body of "+subject))
	waitUntil(t, 10*time.Second, "seed backfilled", func() bool {
		for _, e := range env.emails(t) {
			if e.Subject == subject {
				return true
			}
		}
		return false
	})
	for _, e := range env.emails(t) {
		if e.Subject == subject {
			return uid, e.ID
		}
	}
	t.Fatalf("seeded message %q not in store", subject)
	return 0, ""
}

// --- Email/set: keyword round trip (FR-M.9) ---

func TestEmailSetStarIsVisibleToSecondClient(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)
	uid, id := seedInbox(t, env, "star me", "<star@example.test>")

	old := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"keywords/$flagged": true}},
	})
	if got := idList(t, old, "updated"); len(got) != 1 || got[0] != id {
		t.Fatalf("updated = %v, want [%s]", got, id)
	}
	oldState, _ := old.Args["oldState"].(string)
	newState, _ := old.Args["newState"].(string)
	if newState == "" || newState == oldState {
		t.Errorf("newState %q did not advance past oldState %q (read-your-writes, FR-M.13)", newState, oldState)
	}

	// The cache moved...
	if e := emailByID(t, env, id); e == nil || !e.Keywords["$flagged"] {
		t.Fatalf("store keywords = %v, want $flagged", e)
	}
	// ...and so did the server: a second, independent session sees it.
	if flags := flagsVia(t, env, "INBOX", uid); !hasFlagName(flags, `\Flagged`) {
		t.Errorf("second session flags = %v, want \\Flagged", flags)
	}
	// /changes replays it for anyone holding the old state.
	cs, err := env.st.Changes(context.Background(), "acct", "Email", oldState)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != id {
		t.Errorf("changes updated = %v, want [%s]", cs.Updated, id)
	}

	// Undo (jmap-tui's star-undo is exactly this patch).
	undo := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"keywords/$flagged": nil}},
	})
	if got := idList(t, undo, "updated"); len(got) != 1 {
		t.Errorf("undo updated = %v", got)
	}
	if e := emailByID(t, env, id); e.Keywords["$flagged"] {
		t.Error("store still has $flagged after undo")
	}
	if flags := flagsVia(t, env, "INBOX", uid); hasFlagName(flags, `\Flagged`) {
		t.Errorf("second session flags = %v, want the flag gone", flags)
	}
}

// --- Email/set: membership (FR-M.9) ---

func TestEmailSetMoveCopyUndoRoundTrip(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)
	uid, id := seedInbox(t, env, "move me", "<move@example.test>")

	// Create the destination through Mailbox/set (FR-M.12) — also the
	// proof that role detection refresh works: "Archive" earns the role.
	created := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"arch": map[string]any{"name": "Archive", "parentId": nil}},
	})
	handle, _ := created.Args["created"].(map[string]any)
	entry, _ := handle["arch"].(map[string]any)
	archiveID, _ := entry["id"].(string)
	if archiveID == "" {
		t.Fatalf("Mailbox/set create failed: %v", created.Args)
	}
	arch := mailboxBy(t, env, func(mb *jmapapi.Mailbox) bool { return mb.ID == archiveID })
	if arch == nil || arch.Role != "archive" {
		t.Fatalf("created mailbox = %#v, want role archive after refresh", arch)
	}

	// Move: add Archive, remove Inbox in one patch.
	move := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update": map[string]any{id: map[string]any{
			"mailboxIds/" + archiveID:       true,
			"mailboxIds/" + inboxID(t, env): nil,
		}},
	})
	if errs := setErrors(t, move, "notUpdated"); len(errs) > 0 {
		t.Fatalf("move rejected: %+v", errs)
	}
	if e := emailByID(t, env, id); len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != archiveID {
		t.Fatalf("mailboxIds after move = %v, want [%s]", e.MailboxIDs, archiveID)
	}
	// Second client: present in Archive, gone from INBOX.
	if got := uidsVia(t, env, "Archive"); len(got) != 1 {
		t.Errorf("Archive uids via second session = %v, want the message", got)
	}
	if got := uidsVia(t, env, "INBOX"); len(got) != 0 {
		t.Errorf("INBOX uids via second session = %v, want empty", got)
	}
	_ = uid

	// Undo the move: back to Inbox only.
	undo := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update": map[string]any{id: map[string]any{
			"mailboxIds/" + inboxID(t, env): true,
			"mailboxIds/" + archiveID:       nil,
		}},
	})
	if errs := setErrors(t, undo, "notUpdated"); len(errs) > 0 {
		t.Fatalf("move undo rejected: %+v", errs)
	}
	if e := emailByID(t, env, id); len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != inboxID(t, env) {
		t.Fatalf("mailboxIds after undo = %v", e.MailboxIDs)
	}
	if got := uidsVia(t, env, "INBOX"); len(got) != 1 {
		t.Errorf("INBOX uids via second session = %v, want the message back", got)
	}
	if got := uidsVia(t, env, "Archive"); len(got) != 0 {
		t.Errorf("Archive uids via second session = %v, want empty", got)
	}

	// Copy (add without removing), then drop the extra membership.
	copyRes := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"mailboxIds/" + archiveID: true}},
	})
	if errs := setErrors(t, copyRes, "notUpdated"); len(errs) > 0 {
		t.Fatalf("copy rejected: %+v", errs)
	}
	if e := emailByID(t, env, id); len(e.MailboxIDs) != 2 {
		t.Fatalf("mailboxIds after copy = %v, want both", e.MailboxIDs)
	}
	if got := uidsVia(t, env, "Archive"); len(got) != 1 {
		t.Errorf("Archive uids via second session = %v, want one copy", got)
	}
	unCopy := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"mailboxIds/" + archiveID: nil}},
	})
	if errs := setErrors(t, unCopy, "notUpdated"); len(errs) > 0 {
		t.Fatalf("copy undo rejected: %+v", errs)
	}
	if got := uidsVia(t, env, "Archive"); len(got) != 0 {
		t.Errorf("Archive uids after copy undo = %v, want empty", got)
	}
}

func inboxID(t *testing.T, env *testEnv) string {
	t.Helper()
	mb := mailboxBy(t, env, func(m *jmapapi.Mailbox) bool { return m.Role == "inbox" })
	if mb == nil {
		t.Fatal("no inbox mailbox")
	}
	return mb.ID
}

// --- Email/set: destroy (FR-M.10) ---

func TestEmailSetDestroyExpungesAndTombstones(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)
	_, id := seedInbox(t, env, "destroy me", "<destroy@example.test>")

	res := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"destroy":   []string{id},
	})
	if got := idList(t, res, "destroyed"); len(got) != 1 || got[0] != id {
		t.Fatalf("destroyed = %v, want [%s]", got, id)
	}
	// Gone from the server (second client) and tombstoned locally.
	if got := uidsVia(t, env, "INBOX"); len(got) != 0 {
		t.Errorf("INBOX uids via second session = %v, want empty", got)
	}
	if e := emailByID(t, env, id); e != nil {
		t.Errorf("email still live after destroy: %v", e.Subject)
	}
	cs, err := env.st.Changes(context.Background(), "acct", "Email", "0")
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	found := false
	for _, d := range cs.Destroyed {
		if d == id {
			found = true
		}
	}
	if !found {
		t.Errorf("changes destroyed = %v, want %s", cs.Destroyed, id)
	}

	// An id the cache never saw is notFound (RFC 8621 §4.6 suite);
	// destroying the tombstoned one again stays a no-op success.
	unknown := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct", "destroy": []string{"nonexistent-email-xyz"},
	})
	if errs := setErrors(t, unknown, "notDestroyed"); errs["nonexistent-email-xyz"].Type != "notFound" {
		t.Errorf("destroy unknown = %+v, want notFound", errs)
	}
	again := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct", "destroy": []string{id},
	})
	if errs := setErrors(t, again, "notDestroyed"); len(errs) > 0 {
		t.Errorf("re-destroy of a tombstone = %+v, want no-op success (FR-M.10)", errs)
	}
}

// --- failure semantics (FR-M.13) ---

func TestEmailSetFailuresLeaveStateUnchanged(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)
	_, id := seedInbox(t, env, "keep me", "<keep@example.test>")
	before, _ := env.st.EmailStateString(context.Background(), "acct")

	// Unknown email id → notFound.
	res := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{"nonexistent-email-xyz": map[string]any{"keywords/$seen": true}},
	})
	if errs := setErrors(t, res, "notUpdated"); errs["nonexistent-email-xyz"].Type != "notFound" {
		t.Errorf("update unknown id = %+v, want notFound", errs)
	}

	// A patch that would leave the email in no mailbox is refused, and
	// the cache must not move (RFC 8621 §4.1).
	empty := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"mailboxIds/" + inboxID(t, env): nil}},
	})
	if errs := setErrors(t, empty, "notUpdated"); errs[id].Type != "invalidProperties" {
		t.Errorf("emptying patch = %+v, want invalidProperties", errs)
	}

	// An unknown mailbox id in a patch is invalidProperties too.
	unknownMB := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"mailboxIds/no-such-mailbox": true}},
	})
	if errs := setErrors(t, unknownMB, "notUpdated"); errs[id].Type != "invalidProperties" {
		t.Errorf("unknown mailbox patch = %+v, want invalidProperties", errs)
	}

	// Immutable content on update → invalidProperties, never applied.
	immutable := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{id: map[string]any{"subject": "rewritten"}},
	})
	if errs := setErrors(t, immutable, "notUpdated"); errs[id].Type != "invalidProperties" {
		t.Errorf("immutable patch = %+v, want invalidProperties", errs)
	}

	// Nothing above may have moved the state or the message.
	after, _ := env.st.EmailStateString(context.Background(), "acct")
	if after != before {
		t.Errorf("email state moved %s → %s after failed members", before, after)
	}
	e := emailByID(t, env, id)
	if e == nil || len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != inboxID(t, env) {
		t.Errorf("email after failed members = %#v", e)
	}
}

// --- Mailbox/set (FR-M.12) ---

func TestMailboxSetCreateRenameDelete(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)

	// Top-level create.
	top := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"a": map[string]any{"name": "Projects", "parentId": nil}},
	})
	if errs := setErrors(t, top, "notCreated"); len(errs) > 0 {
		t.Fatalf("create top-level: %+v", errs)
	}
	topID := createdID(t, top, "a")
	if topID == "" {
		t.Fatal("no id returned")
	}
	if oldState, _ := top.Args["oldState"].(string); oldState == "" {
		t.Error("oldState missing")
	}
	if newState, _ := top.Args["newState"].(string); newState == "" || newState == mustString(t, top.Args["oldState"]) {
		t.Errorf("newState = %v, oldState = %v; want an advance", newState, top.Args["oldState"])
	}

	// Child create, verified through Mailbox/get (the suite's check).
	child := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"b": map[string]any{
			"name": "2026", "parentId": topID,
		}},
	})
	if errs := setErrors(t, child, "notCreated"); len(errs) > 0 {
		t.Fatalf("create child: %+v", errs)
	}
	childID := createdID(t, child, "b")
	got := jmap(t, acct, h, "Mailbox/get", map[string]any{
		"accountId": "acct", "ids": []string{childID},
	})
	if parent := mailboxProp(t, got, 0, "parentId"); parent != topID {
		t.Errorf("child parentId = %v, want %s", parent, topID)
	}

	// Duplicate name under the same parent → alreadyExists.
	dup := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"c": map[string]any{"name": "Projects", "parentId": nil}},
	})
	if errs := setErrors(t, dup, "notCreated"); errs["c"].Type != "alreadyExists" {
		t.Errorf("duplicate create = %+v, want alreadyExists", errs)
	}

	// A property the bridge derives from the server is refused loudly.
	derived := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"d": map[string]any{"name": "Nope", "sortOrder": 10}},
	})
	if errs := setErrors(t, derived, "notCreated"); errs["d"].Type != "invalidProperties" {
		t.Errorf("sortOrder create = %+v, want invalidProperties", errs)
	}

	// Rename keeps the id and follows the child's path.
	rename := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{topID: map[string]any{"name": "Work"}},
	})
	if errs := setErrors(t, rename, "notUpdated"); len(errs) > 0 {
		t.Fatalf("rename: %+v", errs)
	}
	gotTop := jmap(t, acct, h, "Mailbox/get", map[string]any{
		"accountId": "acct", "ids": []string{topID},
	})
	if name := mailboxProp(t, gotTop, 0, "name"); name != "Work" {
		t.Errorf("renamed name = %v, want Work", name)
	}
	// The child's id survives its parent's rename (RFC 8621 §5.1), and
	// its reported path is whatever LIST says — some servers rename the
	// subtree with the parent, some move only the node, and the cache
	// follows the server either way (golden rule 1).
	gotChild := jmap(t, acct, h, "Mailbox/get", map[string]any{
		"accountId": "acct", "ids": []string{childID},
	})
	childName, _ := mailboxProp(t, gotChild, 0, "name").(string)
	lastName := ""
	for _, n := range folderNames(t, env) {
		if strings.HasSuffix(n, "2026") {
			lastName = n
		}
	}
	if lastName == "" {
		t.Fatalf("second session lost the child folder: %v", folderNames(t, env))
	}
	if childName != lastName {
		t.Errorf("child path = %q, server has %q", childName, lastName)
	}

	// A mailbox with children cannot be destroyed (RFC 8621 §2.5).
	par := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "create": map[string]any{"p": map[string]any{"name": "Parent", "parentId": nil}},
	})
	parentID := createdID(t, par, "p")
	kid := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "create": map[string]any{"k": map[string]any{"name": "Child", "parentId": parentID}},
	})
	kidID := createdID(t, kid, "k")
	hasChild := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "destroy": []string{parentID},
	})
	if errs := setErrors(t, hasChild, "notDestroyed"); errs[parentID].Type != "mailboxHasChild" {
		t.Errorf("destroy with children = %+v, want mailboxHasChild", errs)
	}
	// The child and the parent both still exist.
	gotPair := jmap(t, acct, h, "Mailbox/get", map[string]any{
		"accountId": "acct", "ids": []string{parentID, kidID},
	})
	if n := listLen(t, gotPair); n != 2 {
		t.Errorf("parent/child get after refused destroy returned %d objects: %v", n, gotPair.Args)
	}

	// Empty child destroys cleanly, and the second session stops seeing
	// it (CREATE/RENAME/DELETE are server-side).
	delChild := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "destroy": []string{kidID},
	})
	if got := idList(t, delChild, "destroyed"); len(got) != 1 || got[0] != kidID {
		t.Fatalf("destroy child = %+v", delChild.Args)
	}
	for _, n := range folderNames(t, env) {
		if n == "Parent/Child" {
			t.Errorf("second session still lists %q", n)
		}
	}

	// Unknown ids are notFound on update and destroy.
	unknownUpd := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"update":    map[string]any{"nonexistent-mailbox-xyz": map[string]any{"name": "x"}},
	})
	if errs := setErrors(t, unknownUpd, "notUpdated"); errs["nonexistent-mailbox-xyz"].Type != "notFound" {
		t.Errorf("update unknown mailbox = %+v, want notFound", errs)
	}
	unknownDel := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "destroy": []string{"nonexistent-mailbox-xyz"},
	})
	if errs := setErrors(t, unknownDel, "notDestroyed"); errs["nonexistent-mailbox-xyz"].Type != "notFound" {
		t.Errorf("destroy unknown mailbox = %+v, want notFound", errs)
	}

	// Finally, a non-empty mailbox refuses to die until the caller says
	// it may remove the mail (RFC 8621 §2.5).
	seedInbox(t, env, "occupant", "<occupant@example.test>")
	empty := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"e": map[string]any{"name": "Occupied", "parentId": nil}},
	})
	occupiedID := createdID(t, empty, "e")
	jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"m": map[string]any{
			"mailboxIds":    map[string]bool{occupiedID: true},
			"from":          []map[string]string{{"email": "me@example.test"}},
			"to":            []map[string]string{{"email": "you@example.test"}},
			"subject":       "here",
			"bodyStructure": map[string]any{"type": "text/plain", "partId": "1"},
			"bodyValues":    map[string]any{"1": map[string]any{"value": "body"}},
		}},
	})
	hasEmail := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "destroy": []string{occupiedID},
	})
	if errs := setErrors(t, hasEmail, "notDestroyed"); errs[occupiedID].Type != "mailboxHasEmail" {
		t.Errorf("destroy non-empty = %+v, want mailboxHasEmail", errs)
	}
	withMail := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct", "destroy": []string{occupiedID},
		"onDestroyRemoveEmails": true,
	})
	if got := idList(t, withMail, "destroyed"); len(got) != 1 {
		t.Errorf("destroy with onDestroyRemoveEmails = %v, want the mailbox", withMail.Args)
	}
	if names := folderNames(t, env); contains(names, "Occupied") {
		t.Errorf("second session still lists Occupied: %v", names)
	}
}

// --- Email/set create: drafts (FR-M.11) ---

func TestEmailSetCreateDraftRoundTrip(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)

	// A Drafts mailbox (role comes from the well-known name on refresh).
	mb := jmap(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "acct",
		"create":    map[string]any{"d": map[string]any{"name": "Drafts", "parentId": nil}},
	})
	draftsID := createdID(t, mb, "d")
	if draftsID == "" {
		t.Fatalf("drafts create failed: %v", mb.Args)
	}
	if got := mailboxBy(t, env, func(m *jmapapi.Mailbox) bool { return m.ID == draftsID }); got.Role != "drafts" {
		t.Fatalf("role = %q, want drafts", got.Role)
	}

	res := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"new": map[string]any{
			"mailboxIds": map[string]bool{draftsID: true},
			"keywords":   map[string]bool{"$draft": true, "$seen": true},
			"from":       []map[string]string{{"name": "Me", "email": "me@example.test"}},
			"to":         []map[string]string{{"name": "You", "email": "you@example.test"}},
			"subject":    "A draft about bridges",
			"textBody":   []map[string]any{{"partId": "1", "type": "text/plain"}},
			"bodyValues": map[string]any{"1": map[string]any{"value": "draft body text"}},
			"receivedAt": "2026-09-02T09:30:00Z",
		}},
	})
	if errs := setErrors(t, res, "notCreated"); len(errs) > 0 {
		t.Fatalf("create draft: %+v", errs)
	}
	created, _ := res.Args["created"].(map[string]any)
	entry, _ := created["new"].(map[string]any)
	id, _ := entry["id"].(string)
	threadID, _ := entry["threadId"].(string)
	size, _ := entry["size"].(float64)
	if id == "" {
		t.Fatalf("created = %v, want an id", res.Args["created"])
	}
	if threadID == "" || size <= 0 {
		t.Errorf("created object = %v, want threadId and size (RFC 8621 §4.6)", entry)
	}
	if blobID, _ := entry["blobId"].(string); blobID == "" {
		t.Errorf("created object has no blobId: %v", entry)
	}

	// The message is on the server with \Draft, from a second session.
	got := uidsVia(t, env, "Drafts")
	if len(got) != 1 {
		t.Fatalf("Drafts uids via second session = %v, want the draft", got)
	}
	if flags := flagsVia(t, env, "Drafts", got[0]); !hasFlagName(flags, `\Draft`) || !hasFlagName(flags, `\Seen`) {
		t.Errorf("draft flags = %v, want \\Draft and \\Seen", flags)
	}

	// Reading it back gives the body the client wrote, hydrated.
	emails, _, notFound, err := env.st.EmailsByID(context.Background(), "acct", []string{id}, true)
	if err != nil || len(notFound) > 0 {
		t.Fatalf("Email/get: %v %v", notFound, err)
	}
	e := emails[0]
	if e.Subject != "A draft about bridges" {
		t.Errorf("subject = %q", e.Subject)
	}
	if got := strings.TrimRight(e.BodyValues["1"], "\r\n"); got != "draft body text" {
		t.Errorf("bodyValues = %q, want the written text", got)
	}
	if !e.Keywords["$draft"] {
		t.Errorf("keywords = %v, want $draft", e.Keywords)
	}
	if len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != draftsID {
		t.Errorf("mailboxIds = %v, want the drafts mailbox", e.MailboxIDs)
	}

	// Create violations are rejected, not reshaped (RFC 8621 §4.6).
	bad := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"bad": map[string]any{
			"mailboxIds": map[string]bool{draftsID: true},
			"headers":    map[string]any{"subject": "nope"},
		}},
	})
	if errs := setErrors(t, bad, "notCreated"); errs["bad"].Type != "invalidProperties" {
		t.Errorf("create with headers = %+v, want invalidProperties", errs)
	}
	missingBlob := jmap(t, acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"create": map[string]any{"att": map[string]any{
			"mailboxIds":  map[string]bool{draftsID: true},
			"attachments": []map[string]any{{"blobId": "no-such-blob", "type": "text/plain"}},
		}},
	})
	if errs := setErrors(t, missingBlob, "notCreated"); errs["att"].Type != "blobNotFound" {
		t.Errorf("create with unknown blob = %+v, want blobNotFound", errs)
	}
}

// --- state strings / ifInState (RFC 8620 §5.3) ---

func TestEmailSetIfInStateMismatch(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	acct, h := testAccount(env)
	_, id := seedInbox(t, env, "ifinstate", "<ifinstate@example.test>")

	_, merr := jmapTry(acct, h, "Email/set", map[string]any{
		"accountId": "acct",
		"ifInState": "999999",
		"update":    map[string]any{id: map[string]any{"keywords/$seen": true}},
	})
	if merr == nil {
		t.Fatal("ifInState mismatch accepted; want a stateMismatch error invocation")
	}
	if !strings.Contains(merr.Error(), "stateMismatch") {
		t.Errorf("error = %v, want stateMismatch", merr)
	}
	// The rejected request must not have applied.
	if e := emailByID(t, env, id); e.Keywords["$seen"] {
		t.Error("update applied despite stateMismatch")
	}
}

func folderNames(t *testing.T, env *testEnv) []string {
	t.Helper()
	conn := secondConn(t, env)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := conn.ListFolders(ctx)
	if err != nil {
		t.Fatalf("second session list: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, f := range list {
		out = append(out, f.Name)
	}
	return out
}

// listLen counts the objects a /get returned.
func listLen(t *testing.T, res jmapResult) int {
	t.Helper()
	list, _ := res.Args["list"].([]any)
	return len(list)
}

func createdID(t *testing.T, res jmapResult, handle string) string {
	t.Helper()
	created, _ := res.Args["created"].(map[string]any)
	entry, _ := created[handle].(map[string]any)
	id, _ := entry["id"].(string)
	return id
}

func mailboxProp(t *testing.T, res jmapResult, index int, prop string) any {
	t.Helper()
	list, _ := res.Args["list"].([]any)
	if index >= len(list) {
		t.Fatalf("list too short: %v", res.Args)
	}
	obj, _ := list[index].(map[string]any)
	return obj[prop]
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	return s
}

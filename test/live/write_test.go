package live

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/go-imap/imapclient"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	bridgesync "github.com/CaffeinatedTech/jmap-bridge/internal/sync"
)

// The M2 live gate, bridge side: every JMAP mutation must be visible to
// an independent IMAP session — star, move, copy, destroy, mailbox
// admin and a draft create — against the real server (PLAN §12 M2).
// Skips unless JMAP_BRIDGE_TEST_IMAP_* is set (AGENTS.md).

// tempFolder is this test's workspace (live rules of engagement: work
// inside designated folders, clean up after). The subjects are fixed so
// a rerun can find and remove anything an earlier run left behind.
const (
	tempFolder     = "jmap-bridge-m2"
	m2SeedSubject  = "M2 gate target"
	m2DraftSubject = "M2 draft"
)

// readConn opens a fresh, independent IMAP session for verification —
// it shares nothing with the engine under test.
func readConn(t *testing.T, cfg imapdrv.Config) *imapdrv.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := imapdrv.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("verification session dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func flagsAt(t *testing.T, conn *imapdrv.Conn, folder string, uid uint32) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := conn.Examine(ctx, folder, nil); err != nil {
		t.Fatalf("examine %s: %v", folder, err)
	}
	all, err := conn.AllFlags(ctx)
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	for _, ch := range all {
		if ch.UID == uid {
			return ch.Flags
		}
	}
	t.Fatalf("uid %d not in %s (session sees %d messages)", uid, folder, len(all))
	return nil
}

func folderNamesVia(t *testing.T, conn *imapdrv.Conn) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := conn.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]bool{}
	for _, f := range list {
		out[f.Name] = true
	}
	return out
}

// uidWithSubject finds a message by subject through the verifying
// session — subjects, not uids, because uids are reassigned by every
// MOVE and the gate account holds messages from earlier runs.
func uidWithSubject(t *testing.T, conn *imapdrv.Conn, folder, subject string) (uint32, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := conn.Examine(ctx, folder, nil); err != nil {
		t.Fatalf("examine %s: %v", folder, err)
	}
	uids, err := conn.UIDs(ctx)
	if err != nil {
		t.Fatalf("uids: %v", err)
	}
	if len(uids) == 0 {
		return 0, false
	}
	msgs, err := conn.FetchHeaders(ctx, uids)
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	for _, m := range msgs {
		if m.Envelope.Subject == subject {
			return m.UID, true
		}
	}
	return 0, false
}

// expungeBySubject removes one message the test owns, wherever an
// earlier run left it (live rules: clean up after yourself).
func expungeBySubject(t *testing.T, conn *imapdrv.Conn, folder, subject string) {
	t.Helper()
	uid, ok := uidWithSubject(t, conn, folder, subject)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conn.ExpungeUIDs(ctx, folder, []uint32{uid}); err != nil {
		t.Logf("expunge %s/%d: %v", folder, uid, err)
	}
}

func flagHas(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// liveSet runs one JMAP method call against the in-process handler and
// returns the decoded /set response (failing the test on a method-level
// error).
func liveSet(t *testing.T, acct *jmapapi.Account, h *jmapapi.Handler, method string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{[]any{method, args, "c1"}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	status, out := h.Dispatch(context.Background(), acct, body)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if status != 200 {
		t.Fatalf("%s: HTTP %d: %s", method, status, raw)
	}
	var wire struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.MethodResponses) == 0 {
		t.Fatalf("%s: bad response: %s", method, raw)
	}
	var name string
	_ = json.Unmarshal(wire.MethodResponses[0][0], &name)
	if name == "error" {
		t.Fatalf("%s: method error: %s", method, wire.MethodResponses[0][1])
	}
	var m map[string]any
	if err := json.Unmarshal(wire.MethodResponses[0][1], &m); err != nil {
		t.Fatalf("%s: decode args: %v", method, err)
	}
	return m
}

// setErr decodes one notCreated/notUpdated/notDestroyed entry.
func setErr(t *testing.T, res map[string]any, key, field string) *jmapapi.SetError {
	t.Helper()
	raw, ok := res[field]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal %s: %v", field, err)
	}
	var m map[string]jmapapi.SetError
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	se, ok := m[key]
	if !ok {
		return nil
	}
	return &se
}

func createdIDOf(t *testing.T, res map[string]any, handle string) string {
	t.Helper()
	created, _ := res["created"].(map[string]any)
	entry, _ := created[handle].(map[string]any)
	id, _ := entry["id"].(string)
	if id == "" {
		t.Fatalf("create %q failed: %v", handle, res)
	}
	return id
}

func liveMailboxID(t *testing.T, ctx context.Context, st *store.Store, match func(*jmapapi.Mailbox) bool) *jmapapi.Mailbox {
	t.Helper()
	mbs, _, err := st.Mailboxes(ctx, "livetest")
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

func liveEmail(t *testing.T, ctx context.Context, st *store.Store, id string) *jmapapi.Email {
	t.Helper()
	emails, _, notFound, err := st.EmailsByID(ctx, "livetest", []string{id}, false)
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	if len(notFound) > 0 || len(emails) == 0 {
		return nil
	}
	return emails[0]
}

// TestLiveWriteTriageRoundTrips is the M2 gate, bridge side.
func TestLiveWriteTriageRoundTrips(t *testing.T) {
	cfg := liveIMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	admin := adminSession(t, cfg)
	verifier := readConn(t, cfg)

	// Workspace: the move target and a drafts mailbox (roles come from
	// the well-known names on the refresh that follows each CREATE).
	deleteQuietly(t, ctx, admin, tempFolder)
	deleteQuietly(t, ctx, admin, tempFolder+"-renamed")
	if err := admin.Create(tempFolder, nil).Wait(ctx); err != nil {
		t.Fatalf("create %s: %v", tempFolder, err)
	}
	if err := admin.Create("Drafts", nil).Wait(ctx); err != nil && !strings.Contains(err.Error(), "exists") {
		t.Fatalf("create Drafts: %v", err)
	}
	// Idempotent setup: a previous run's leftovers must not satisfy a
	// "message is here" assertion by accident.
	expungeBySubject(t, verifier, "INBOX", m2SeedSubject)
	expungeBySubject(t, verifier, tempFolder, m2SeedSubject)
	seedUID := appendLive(t, ctx, admin, "INBOX", liveMessage(m2SeedSubject, "<m2-gate@example.test>"))
	t.Cleanup(func() {
		// Whatever the run did, the seed and the draft go; uids have
		// moved too often to trust, so match by subject.
		expungeBySubject(t, verifier, "INBOX", m2SeedSubject)
		expungeBySubject(t, verifier, tempFolder, m2SeedSubject)
		expungeBySubject(t, verifier, "Drafts", m2DraftSubject)
		deleteQuietly(t, ctx, admin, tempFolder)
		deleteQuietly(t, ctx, admin, tempFolder+"-renamed")
	})

	// Fresh store + engine + JMAP handler against the real account.
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

	eng := bridgesync.New(bridgesync.Config{
		Account: "livetest", NewBackend: liveBackend(cfg),
		Interval: time.Hour, BatchSize: 200, PrefetchWindow: 0, Concurrency: 2,
	}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	go eng.Run(engCtx)

	acct := &jmapapi.Account{
		ID: "livetest", Store: st, Backend: eng,
		Capabilities: []string{
			"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail",
		},
	}
	h := jmapapi.NewHandler(st)

	// Backfill: the seeded message and the two folders are known.
	waitFor(t, 90*time.Second, "backfill", func() bool {
		e := liveEmailWithSubject(t, ctx, st, "M2 gate target")
		return e != nil && len(e.MailboxIDs) == 1
	})
	seed := liveEmailWithSubject(t, ctx, st, "M2 gate target")
	inbox := liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == "inbox" })
	target := liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Path == tempFolder })
	drafts := liveMailboxID(t, ctx, st, func(m *jmapapi.Mailbox) bool { return m.Role == "drafts" })
	if inbox == nil || target == nil || drafts == nil {
		t.Fatalf("mailboxes: inbox=%v target=%v drafts=%v", inbox, target, drafts)
	}

	// --- star: visible to the second session within the write's wake ---
	res := liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"update":    map[string]any{seed.ID: map[string]any{"keywords/$flagged": true}},
	})
	if se := setErr(t, res, seed.ID, "notUpdated"); se != nil {
		t.Fatalf("star rejected: %+v", se)
	}
	if flags := flagsAt(t, verifier, "INBOX", seedUID); !flagHas(flags, `\Flagged`) {
		t.Errorf("second client flags = %v, want \\Flagged", flags)
	}
	if e := liveEmail(t, ctx, st, seed.ID); !e.Keywords["$flagged"] {
		t.Errorf("store keywords = %v, want $flagged", e.Keywords)
	}
	t.Log("star visible to the second IMAP client")

	// --- move: add target, remove inbox, in one patch ---
	res = liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"update": map[string]any{seed.ID: map[string]any{
			"mailboxIds/" + target.ID: true,
			"mailboxIds/" + inbox.ID:  nil,
		}},
	})
	if se := setErr(t, res, seed.ID, "notUpdated"); se != nil {
		t.Fatalf("move rejected: %+v", se)
	}
	waitFor(t, 10*time.Second, "move visible to the second client", func() bool {
		_, inDest := uidWithSubject(t, verifier, tempFolder, m2SeedSubject)
		_, inInbox := uidWithSubject(t, verifier, "INBOX", m2SeedSubject)
		return inDest && !inInbox
	})
	if e := liveEmail(t, ctx, st, seed.ID); len(e.MailboxIDs) != 1 || e.MailboxIDs[0] != target.ID {
		t.Errorf("store mailboxIds = %v, want the target only", e.MailboxIDs)
	}
	t.Log("move visible to the second IMAP client")

	// --- undo (back to the inbox), then copy ---
	res = liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"update": map[string]any{seed.ID: map[string]any{
			"mailboxIds/" + inbox.ID:  true,
			"mailboxIds/" + target.ID: nil,
		}},
	})
	if se := setErr(t, res, seed.ID, "notUpdated"); se != nil {
		t.Fatalf("move undo rejected: %+v", se)
	}
	waitFor(t, 10*time.Second, "undo visible to the second client", func() bool {
		_, inDest := uidWithSubject(t, verifier, tempFolder, m2SeedSubject)
		_, inInbox := uidWithSubject(t, verifier, "INBOX", m2SeedSubject)
		return inInbox && !inDest
	})
	res = liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"update":    map[string]any{seed.ID: map[string]any{"mailboxIds/" + target.ID: true}},
	})
	if se := setErr(t, res, seed.ID, "notUpdated"); se != nil {
		t.Fatalf("copy rejected: %+v", se)
	}
	waitFor(t, 10*time.Second, "copy visible to the second client", func() bool {
		_, inDest := uidWithSubject(t, verifier, tempFolder, m2SeedSubject)
		_, inInbox := uidWithSubject(t, verifier, "INBOX", m2SeedSubject)
		return inDest && inInbox
	})
	liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"update":    map[string]any{seed.ID: map[string]any{"mailboxIds/" + target.ID: nil}},
	})
	waitFor(t, 10*time.Second, "copy undo visible to the second client", func() bool {
		_, inDest := uidWithSubject(t, verifier, tempFolder, m2SeedSubject)
		_, inInbox := uidWithSubject(t, verifier, "INBOX", m2SeedSubject)
		return !inDest && inInbox
	})
	t.Log("move/undo/copy round-trips visible to the second IMAP client")

	// --- Mailbox/set: create, rename, delete, all server-side ---
	created := liveSet(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "livetest",
		"create":    map[string]any{"t": map[string]any{"name": tempFolder + "-new", "parentId": nil}},
	})
	newID := createdIDOf(t, created, "t")
	waitFor(t, 10*time.Second, "created mailbox listed by the second client", func() bool {
		return folderNamesVia(t, verifier)[tempFolder+"-new"]
	})
	renamed := liveSet(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "livetest",
		"update":    map[string]any{newID: map[string]any{"name": tempFolder + "-renamed"}},
	})
	if se := setErr(t, renamed, newID, "notUpdated"); se != nil {
		t.Fatalf("rename rejected: %+v", se)
	}
	waitFor(t, 10*time.Second, "renamed mailbox listed by the second client", func() bool {
		names := folderNamesVia(t, verifier)
		return names[tempFolder+"-renamed"] && !names[tempFolder+"-new"]
	})
	// A non-empty mailbox refuses to die; an empty one deletes cleanly.
	destroyed := liveSet(t, acct, h, "Mailbox/set", map[string]any{
		"accountId": "livetest", "destroy": []string{newID},
	})
	if se := setErr(t, destroyed, newID, "notDestroyed"); se != nil {
		t.Fatalf("delete rejected: %+v", se)
	}
	waitFor(t, 10*time.Second, "deleted mailbox gone from the second client", func() bool {
		return !folderNamesVia(t, verifier)[tempFolder+"-renamed"]
	})
	t.Log("Mailbox/set create/rename/delete round-trips visible to the second IMAP client")

	// --- draft create: APPEND lands with \Draft ---
	created = liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest",
		"create": map[string]any{"d": map[string]any{
			"mailboxIds": map[string]bool{drafts.ID: true},
			"keywords":   map[string]bool{"$draft": true, "$seen": true},
			"from":       []map[string]string{{"name": "Gate", "email": "gate@example.test"}},
			"to":         []map[string]string{{"name": "Gate", "email": "gate@example.test"}},
			"subject":    m2DraftSubject,
			"textBody":   []map[string]any{{"partId": "1", "type": "text/plain"}},
			"bodyValues": map[string]any{"1": map[string]any{"value": "draft written by the M2 gate"}},
		}},
	})
	draftID := createdIDOf(t, created, "d")
	waitFor(t, 10*time.Second, "draft visible to the second client", func() bool {
		_, ok := uidWithSubject(t, verifier, "Drafts", m2DraftSubject)
		return ok
	})
	draftUID, _ := uidWithSubject(t, verifier, "Drafts", m2DraftSubject)
	if flags := flagsAt(t, verifier, "Drafts", draftUID); !flagHas(flags, `\Draft`) {
		t.Errorf("draft flags = %v, want \\Draft", flags)
	}
	// Reading it back gives the body we wrote (hydrated without a
	// second fetch: the build already parsed its own bytes).
	waitFor(t, 20*time.Second, "draft body readable", func() bool {
		e := liveEmail(t, ctx, st, draftID)
		if e == nil {
			return false
		}
		_, _, _, err := st.EmailsByID(ctx, "livetest", []string{draftID}, true)
		if err != nil {
			return false
		}
		e = liveEmail(t, ctx, st, draftID)
		return strings.Contains(e.BodyValues["1"], "M2 gate")
	})
	t.Log("draft create visible to the second IMAP client with \\Draft")

	// --- destroy: gone from the server, tombstoned in /changes ---
	driveState, _ := st.EmailStateString(ctx, "livetest")
	liveSet(t, acct, h, "Email/set", map[string]any{
		"accountId": "livetest", "destroy": []string{draftID},
	})
	waitFor(t, 10*time.Second, "destroyed draft gone from the second client", func() bool {
		_, ok := uidWithSubject(t, verifier, "Drafts", m2DraftSubject)
		return !ok
	})
	if e := liveEmail(t, ctx, st, draftID); e != nil {
		t.Errorf("destroyed draft still live in the store")
	}
	cs, err := st.Changes(ctx, "livetest", "Email", driveState)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	found := false
	for _, id := range cs.Destroyed {
		if id == draftID {
			found = true
		}
	}
	if !found {
		t.Errorf("destroy not replayed by /changes: %v", cs.Destroyed)
	}
	t.Log("destroy expunged on the server and tombstoned locally")
}

func liveEmailWithSubject(t *testing.T, ctx context.Context, st *store.Store, subject string) *jmapapi.Email {
	t.Helper()
	emails, _, _, err := st.EmailsByID(ctx, "livetest", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	for _, e := range emails {
		if e.Subject == subject {
			return e
		}
	}
	return nil
}

// deleteQuietly removes a folder if it exists; used for idempotent
// setup and cleanup (servers refuse to delete a non-empty folder, which
// is fine: the test only ever leaves it empty).
func deleteQuietly(t *testing.T, ctx context.Context, c *imapclient.Client, name string) {
	t.Helper()
	_ = c.Delete(name, nil).Wait(ctx)
}

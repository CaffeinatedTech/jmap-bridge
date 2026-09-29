package imapdrv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

// The write tests always verify through a *second* session (another
// driver connection), so what they prove is what a foreign IMAP client
// sees — the same shape as the M2 gate, at the driver level.

func TestWriteStoreFlagsVisibleToSecondSession(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	uid := mustAppend(t, s, "INBOX", rawMsg("flags", "<flags@example.test>", "body"))

	c := dialFixture(t, s)
	ctx := context.Background()
	if err := c.StoreFlags(ctx, "INBOX", []uint32{uid}, []string{`\Flagged`, `$label1`}, nil); err != nil {
		t.Fatalf("store add: %v", err)
	}
	if flags := flagsAt(t, s, "INBOX", uid); !hasFlag(flags, `\Flagged`) || !hasFlag(flags, `$label1`) {
		t.Errorf("second session sees %v, want \\Flagged and $label1", flags)
	}

	if err := c.StoreFlags(ctx, "INBOX", []uint32{uid}, nil, []string{`\Flagged`, `$label1`}); err != nil {
		t.Fatalf("store remove: %v", err)
	}
	if flags := flagsAt(t, s, "INBOX", uid); hasFlag(flags, `\Flagged`) || hasFlag(flags, `$label1`) {
		t.Errorf("second session still sees %v after removal", flags)
	}
}

func TestWriteCopyMoveAndExpunge(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	uid := mustAppend(t, s, "INBOX", rawMsg("move me", "<move@example.test>", "body"))
	c := dialFixture(t, s)
	ctx := context.Background()

	if err := c.CreateMailbox(ctx, "Archive"); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err := c.CopyUIDs(ctx, "INBOX", "Archive", []uint32{uid})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if len(res.DestUIDs) != 1 || res.DestUIDs[uid] == 0 {
		t.Errorf("copy mapping = %v, want the source uid mapped to a fresh one", res.DestUIDs)
	}
	if c.SupportsUIDPlus() && res.DestUIDValidity == 0 {
		t.Error("copy reported no destination uidvalidity")
	}
	if got := uidsAt(t, s, "Archive"); len(got) != 1 {
		t.Errorf("Archive uids = %v, want one copy", got)
	}

	// Expunge the copy: the original must survive (UIDPLUS precision).
	if err := c.ExpungeUIDs(ctx, "Archive", []uint32{res.DestUIDs[uid]}); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if got := uidsAt(t, s, "Archive"); len(got) != 0 {
		t.Errorf("Archive uids = %v, want empty after expunge", got)
	}
	if got := uidsAt(t, s, "INBOX"); len(got) != 1 {
		t.Errorf("INBOX uids = %v, want the original untouched", got)
	}

	// Move: source copy leaves, destination gains exactly one.
	moved, err := c.MoveUIDs(ctx, "INBOX", "Archive", []uint32{uid})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if len(moved.DestUIDs) == 0 {
		t.Error("move returned no COPYUID mapping")
	}
	if got := uidsAt(t, s, "INBOX"); len(got) != 0 {
		t.Errorf("INBOX uids = %v, want empty after move", got)
	}
	if got := uidsAt(t, s, "Archive"); len(got) != 1 {
		t.Errorf("Archive uids = %v, want exactly one message", got)
	}
}

func TestWriteAppendReportsUIDWhenUIDPLUS(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	c := dialFixture(t, s)
	ctx := context.Background()

	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	uid, uv, err := c.AppendMessage(ctx, "INBOX",
		rawMsg("appended", "<appended@example.test>", "body"),
		[]string{`\Draft`, `\Seen`}, &when)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := uidsAt(t, s, "INBOX")
	if len(got) != 1 {
		t.Fatalf("INBOX uids = %v, want the appended message", got)
	}
	if c.SupportsUIDPlus() {
		if uid != got[0] {
			t.Errorf("append uid = %d, server assigned %d", uid, got[0])
		}
		if uv == 0 {
			t.Error("append uidvalidity is zero")
		}
	} else if uid != 0 {
		t.Errorf("server without UIDPLUS reported uid %d", uid)
	}
	if flags := flagsAt(t, s, "INBOX", got[0]); !hasFlag(flags, `\Draft`) || !hasFlag(flags, `\Seen`) {
		t.Errorf("appended flags = %v, want \\Draft and \\Seen", flags)
	}
}

func TestWriteMailboxAdminAndDelim(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	c := dialFixture(t, s)
	ctx := context.Background()

	if err := c.CreateMailbox(ctx, "Projects"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.CreateMailbox(ctx, "Projects/2026"); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := c.RenameMailbox(ctx, "Projects/2026", "Projects/2027"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	delim, err := c.HierarchyDelim(ctx)
	if err != nil {
		t.Fatalf("hierarchy delim: %v", err)
	}
	if delim == 0 {
		t.Error("hierarchy delimiter is zero")
	}
	if err := c.DeleteMailbox(ctx, "Projects/2027"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteMailbox(ctx, "Projects"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}

	// A refusal is a refusal: the error must classify as server-side so
	// the API layer can quote it (FR-M.13).
	err = c.DeleteMailbox(ctx, "no-such-folder")
	if err == nil {
		t.Fatal("deleting a missing folder succeeded")
	}
	if !ServerRejected(err) {
		t.Errorf("err = %v, want a server NO/BAD classification", err)
	}
	if RejectText(err) == "" {
		t.Errorf("no refusal text on %v", err)
	}
}

// --- helpers: a second, independent session over the same server ---

func secondSession(t *testing.T, s *fixtureimap.Server) *Conn {
	t.Helper()
	return dialFixture(t, s)
}

func uidsAt(t *testing.T, s *fixtureimap.Server, folder ...string) []uint32 {
	t.Helper()
	name := "INBOX"
	if len(folder) > 0 {
		name = folder[0]
	}
	c := secondSession(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Examine(ctx, name, nil); err != nil {
		t.Fatalf("examine %s: %v", name, err)
	}
	uids, err := c.UIDs(ctx)
	if err != nil {
		t.Fatalf("uids %s: %v", name, err)
	}
	return uids
}

func flagsAt(t *testing.T, s *fixtureimap.Server, folder string, uid uint32) []string {
	t.Helper()
	c := secondSession(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Examine(ctx, folder, nil); err != nil {
		t.Fatalf("examine %s: %v", folder, err)
	}
	all, err := c.AllFlags(ctx)
	if err != nil {
		t.Fatalf("all flags: %v", err)
	}
	for _, ch := range all {
		if ch.UID == uid {
			return ch.Flags
		}
	}
	t.Fatalf("uid %d not found in %s (%d messages)", uid, folder, len(all))
	return nil
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

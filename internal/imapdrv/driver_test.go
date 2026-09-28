package imapdrv

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
	"github.com/kiliant/go-imap"
)

func dialFixture(t *testing.T, s *fixtureimap.Server) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port, err := strconv.Atoi(portOf(s.Addr()))
	if err != nil {
		t.Fatalf("port of %s: %v", s.Addr(), err)
	}
	c, err := Dial(ctx, Config{Host: hostOf(s.Addr()), Port: port, Username: "test", Password: "test-pass"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func rawMsg(subject, msgid, body string) []byte {
	return []byte("From: sender@example.test\r\nTo: me@example.test\r\nSubject: " + subject +
		"\r\nMessage-ID: " + msgid + "\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func TestDialTierDiscoveryAndStatus(t *testing.T) {
	cases := []struct {
		tier fixtureimap.Tier
		want Tier
	}{
		{fixtureimap.TierQResync, TierQResync},
		{fixtureimap.TierCondStore, TierCondStore},
		{fixtureimap.TierBare, TierBaseline},
	}
	for _, tc := range cases {
		s := fixtureimap.Start(t, fixtureimap.Options{Tier: tc.tier})
		c := dialFixture(t, s)
		if c.Tier() != tc.want {
			t.Errorf("fixture tier %d: detected %s, want %s", tc.tier, c.Tier(), tc.want)
		}
		ctx := context.Background()
		folders, err := c.ListFolders(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(folders) == 0 {
			t.Fatal("no folders")
		}
		foundInbox := false
		for _, f := range folders {
			if f.Name == "INBOX" && f.Role == "inbox" {
				foundInbox = true
			}
		}
		if !foundInbox {
			t.Errorf("INBOX/inbox role missing from %v", folders)
		}
		if prefix, delim, err := c.Namespace(ctx); err != nil {
			t.Errorf("namespace: %v", err)
		} else {
			t.Logf("namespace prefix=%q delim=%q", prefix, string(delim))
		}
		st, err := c.Status(ctx, "INBOX")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.UIDValidity == 0 {
			t.Error("status uidvalidity is zero")
		}
	}
}

func TestBackfillHeadersAndBody(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	body := "The quick brown fox."
	raw := rawMsg("Hello", "<h1@example.test>", body)
	uid := mustAppend(t, s, "INBOX", raw)

	c := dialFixture(t, s)
	ctx := context.Background()
	if _, err := c.Examine(ctx, "INBOX", nil); err != nil {
		t.Fatalf("examine: %v", err)
	}
	msgs, err := c.FetchHeaders(ctx, []uint32{uid})
	if err != nil {
		t.Fatalf("fetch headers: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Envelope.Subject != "Hello" || m.Envelope.MessageID != "<h1@example.test>" {
		t.Errorf("envelope = %+v", m.Envelope)
	}
	if m.Size <= 0 || m.InternalDate.IsZero() {
		t.Errorf("size/date missing: %+v", m)
	}
	if m.Structure == nil || m.Structure.Media() != "text/plain" {
		t.Errorf("structure = %+v", m.Structure)
	}

	got, err := c.FetchBody(ctx, uid)
	if err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	if !strings.Contains(string(got), body) {
		t.Errorf("body = %q, want it to contain %q", got, body)
	}

	previews, err := c.FetchPreviews(ctx, []uint32{uid})
	if err != nil {
		t.Fatalf("previews: %v", err)
	}
	if previews[uid] == "" {
		t.Errorf("preview for uid %d is empty (PREVIEW cap=%v)", uid, c.SupportsPREVIEW())
	}
}

func TestQResyncAnchorReplaysChanges(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	uid1 := mustAppend(t, s, "INBOX", rawMsg("one", "<one@example.test>", "one"))
	uid2 := mustAppend(t, s, "INBOX", rawMsg("two", "<two@example.test>", "two"))

	c := dialFixture(t, s)
	ctx := context.Background()
	sel, err := c.Examine(ctx, "INBOX", nil)
	if err != nil {
		t.Fatalf("examine: %v", err)
	}
	anchor := &Anchor{UIDValidity: sel.Status.UIDValidity, ModSeq: sel.Status.HighestModSeq}
	if anchor.ModSeq == 0 {
		t.Fatal("server reported no HIGHESTMODSEQ")
	}

	// Foreign client flips a flag and expunges another message.
	s.SetFlag("INBOX", uid1, imap.FlagSeen, true)
	s.Expunge("INBOX", uid2)

	sel2, err := c.Examine(ctx, "INBOX", anchor)
	if err != nil {
		t.Fatalf("re-examine: %v", err)
	}
	if sel2.ResyncRejected {
		t.Fatal("server rejected a valid anchor")
	}
	var flagged bool
	for _, ch := range sel2.FlagChanges {
		if ch.UID == uid1 {
			for _, f := range ch.Flags {
				if f == `\Seen` {
					flagged = true
				}
			}
		}
	}
	if !flagged {
		t.Errorf("flag change for uid %d not replayed: %+v", uid1, sel2.FlagChanges)
	}
	foundVanished := false
	for _, u := range sel2.Vanished {
		if u == uid2 {
			foundVanished = true
		}
	}
	if !foundVanished {
		t.Errorf("vanished = %v, want [%d]", sel2.Vanished, uid2)
	}
}

func TestCondStoreChangesSince(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierCondStore})
	uid := mustAppend(t, s, "INBOX", rawMsg("m", "<m@example.test>", "body"))

	c := dialFixture(t, s)
	ctx := context.Background()
	sel, err := c.Examine(ctx, "INBOX", nil)
	if err != nil {
		t.Fatalf("examine: %v", err)
	}
	if c.Tier() != TierCondStore {
		t.Fatalf("tier = %s", c.Tier())
	}
	changed, _, err := c.ChangesSince(ctx, sel.Status.HighestModSeq)
	if err != nil {
		t.Fatalf("changes since: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("clean mailbox reported changes: %+v", changed)
	}

	s.SetFlag("INBOX", uid, imap.FlagFlagged, true)
	changed, _, err = c.ChangesSince(ctx, sel.Status.HighestModSeq)
	if err != nil {
		t.Fatalf("changes since (2): %v", err)
	}
	if len(changed) != 1 || changed[0].UID != uid {
		t.Fatalf("changed = %+v, want the flagged uid", changed)
	}
	var flagged bool
	for _, f := range changed[0].Flags {
		if f == `\Flagged` {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("flags = %v", changed[0].Flags)
	}
}

func TestBaselineAllFlagsAndUIDs(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierBare})
	uid := mustAppend(t, s, "INBOX", rawMsg("m", "<m@example.test>", "body"))

	c := dialFixture(t, s)
	ctx := context.Background()
	if _, err := c.Examine(ctx, "INBOX", nil); err != nil {
		t.Fatalf("examine: %v", err)
	}
	if c.Tier() != TierBaseline {
		t.Fatalf("tier = %s, want baseline", c.Tier())
	}
	uids, err := c.UIDs(ctx)
	if err != nil {
		t.Fatalf("uids: %v", err)
	}
	if len(uids) != 1 || uids[0] != uid {
		t.Fatalf("uids = %v, want [%d]", uids, uid)
	}
	all, err := c.AllFlags(ctx)
	if err != nil {
		t.Fatalf("all flags: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("all flags = %+v", all)
	}

	s.SetFlag("INBOX", uid, imap.FlagSeen, true)
	all, err = c.AllFlags(ctx)
	if err != nil {
		t.Fatalf("all flags (2): %v", err)
	}
	if len(all) != 1 || len(all[0].Flags) == 0 {
		t.Errorf("flag refetch missed the change: %+v", all)
	}
}

func TestIdleSeesForeignChange(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	uid := mustAppend(t, s, "INBOX", rawMsg("m", "<m@example.test>", "body"))

	c := dialFixture(t, s)
	ctx := context.Background()
	if _, err := c.Examine(ctx, "INBOX", nil); err != nil {
		t.Fatalf("examine: %v", err)
	}
	idle, err := c.StartIdle()
	if err != nil {
		t.Fatalf("start idle: %v", err)
	}
	defer func() { _ = c.EndIdle(idle) }()

	// The foreign client acts while we idle.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.SetFlag("INBOX", uid, imap.FlagSeen, true)
	}()

	select {
	case <-c.Notes():
		// the gate's ≤2s path, at the driver level
	case err := <-idle.IdleDone():
		t.Fatalf("idle ended before the change: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no change notification within 2s")
	}
}

func mustAppend(t *testing.T, s *fixtureimap.Server, folder string, raw []byte) uint32 {
	t.Helper()
	uid, _ := s.Append(folder, raw)
	return uid
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[i+1:]
	}
	return "143"
}

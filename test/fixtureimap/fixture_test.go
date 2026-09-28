package fixtureimap

import (
	"context"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// probe opens a fresh client session (a third "other" client).
func probe(t *testing.T, s *Server) *imapclient.Client {
	t.Helper()
	ctx := context.Background()
	c, err := imapclient.Dial(ctx, s.Addr(), &imapclient.Options{AllowInsecureAuth: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Login(ctx, s.user, "test-pass", nil); err != nil {
		t.Fatalf("login: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTierCapabilities(t *testing.T) {
	cases := []struct {
		tier    Tier
		qresync bool
		cond    bool
	}{
		{TierQResync, true, true},
		{TierCondStore, false, true},
		{TierBare, false, false},
	}
	for _, tc := range cases {
		s := Start(t, Options{Tier: tc.tier})
		c := probe(t, s)
		if err := c.Capability(context.Background(), nil); err != nil {
			t.Fatalf("capability: %v", err)
		}
		caps := c.Capabilities()
		if got := caps["QRESYNC"]; got != tc.qresync {
			t.Errorf("tier %d: QRESYNC = %v, want %v", tc.tier, got, tc.qresync)
		}
		if got := caps["CONDSTORE"]; got != tc.cond {
			t.Errorf("tier %d: CONDSTORE = %v, want %v", tc.tier, got, tc.cond)
		}
	}
}

func TestAdminAppendAndFlipFlag(t *testing.T) {
	s := Start(t, Options{Tier: TierQResync})
	raw := []byte("From: a@example.test\r\nTo: b@example.test\r\nSubject: one\r\n\r\nbody\r\n")
	uid, _ := s.Append("INBOX", raw, imap.FlagSeen)
	if uid == 0 {
		t.Fatal("append returned uid 0")
	}
	s.SetFlag("INBOX", uid, imap.FlagFlagged, true)
	s.SetFlag("INBOX", uid, imap.FlagSeen, false)

	c := probe(t, s)
	if _, err := c.Select("INBOX", nil).Wait(context.Background()); err != nil {
		t.Fatalf("select: %v", err)
	}
	cmd := c.FetchUID(imap.UIDSetNum(imap.UID(uid)), nil, imap.FetchItemFlags, imap.FetchItemUID)
	got, err := cmd.Next(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	flags, ok := got.Items[imap.FetchDataKey("FLAGS")]
	if !ok || len(flags) == 0 {
		t.Fatalf("no FLAGS in %v", got.Items)
	}
	set := map[imap.Flag]bool{}
	for _, f := range flags[0].(imap.FetchDataFlags) {
		set[f] = true
	}
	if set[imap.FlagFlagged] != true || set[imap.FlagSeen] != false {
		t.Errorf("flags = %v, want \\Flagged only", set)
	}
}

func TestUIDValidityChangesOnRecreate(t *testing.T) {
	s := Start(t, Options{Tier: TierQResync})
	s.CreateFolder("Projects")
	raw := []byte("Subject: x\r\n\r\nbody\r\n")
	first, uv1 := s.Append("Projects", raw)
	s.DeleteFolder("Projects")
	s.CreateFolder("Projects")
	second, uv2 := s.Append("Projects", raw)
	if uv1 == uv2 {
		t.Errorf("uidvalidity after recreate = %d, want a fresh value (first uid=%d, second uid=%d)", uv2, first, second)
	}
	if second != 1 {
		t.Errorf("uid in recreated folder = %d, want 1", second)
	}
}

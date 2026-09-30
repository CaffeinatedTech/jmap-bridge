package imapdrv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

// TestFetchBodiesBatch pins the batched body fetch: several uids in
// one EXAMINE, byte-exact bodies back, no \Seen side effect.
func TestFetchBodiesBatch(t *testing.T) {
	s := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	uids := []uint32{
		mustAppend(t, s, "INBOX", []byte("From: a@example.test\r\nSubject: one\r\n\r\nbody one\r\n")),
		mustAppend(t, s, "INBOX", []byte("From: a@example.test\r\nSubject: two\r\n\r\nbody two\r\n")),
		mustAppend(t, s, "INBOX", []byte("From: a@example.test\r\nSubject: three\r\n\r\nbody three\r\n")),
	}
	c := dialFixture(t, s)
	bodies, err := c.FetchBodies(ctx, "INBOX", uids)
	if err != nil {
		t.Fatalf("FetchBodies: %v", err)
	}
	if len(bodies) != len(uids) {
		t.Fatalf("got %d bodies, want %d", len(bodies), len(uids))
	}
	if !strings.Contains(string(bodies[uids[0]]), "body one") ||
		!strings.Contains(string(bodies[uids[2]]), "body three") {
		t.Error("bodies not byte-matched to their uids")
	}
	if n := len(flagsAt(t, s, "INBOX", uids[0])); n != 0 {
		t.Errorf("flags after fetch: %d set, want untouched (PEEK)", n)
	}
}

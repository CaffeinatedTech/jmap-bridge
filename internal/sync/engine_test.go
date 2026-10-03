package sync

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CaffeinatedTech/go-imap"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/test/fixtureimap"
)

type testEnv struct {
	fx      *fixtureimap.Server
	st      *store.Store
	eng     *Engine
	cancel  context.CancelFunc
	changes chan struct{}
}

// newEnv starts a fixture (tier), a store, and an engine bound to it.
// Prefetch is off unless the test turns it on; the poll interval is
// long so anything that arrives fast provably came from IDLE.
func newEnv(t *testing.T, tier fixtureimap.Tier, tweak func(*Config)) *testEnv {
	t.Helper()
	fx := fixtureimap.Start(t, fixtureimap.Options{Tier: tier, Users: map[string]string{"test": "test-pass"}})

	changes := make(chan struct{}, 64)
	st, err := store.Open(context.Background(), store.Options{
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
	t.Cleanup(func() { _ = st.Close() })

	host, port := splitAddr(t, fx.Addr())
	cfg := Config{
		Account: "acct",
		IMAP: imapdrv.Config{
			Host: host, Port: port, Username: "test", Password: "test-pass",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Interval:       time.Hour, // poll is not the delivery path under test
		BatchSize:      100,
		PrefetchWindow: 0,
		Concurrency:    2,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	eng := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	env := &testEnv{fx: fx, st: st, eng: eng, changes: changes}
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	go eng.Run(ctx)
	t.Cleanup(cancel)
	return env
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		t.Fatalf("bad addr %q", addr)
	}
	return addr[:i], atoiOrFail(t, addr[i+1:])
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func raw(subject, msgid, body string) []byte {
	return []byte("From: sender@example.test\r\nTo: me@example.test\r\nSubject: " + subject +
		"\r\nMessage-ID: " + msgid + "\r\nDate: Tue, 01 Sep 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func (env *testEnv) emails(t *testing.T) []*jmapapi.Email {
	t.Helper()
	list, _, notFound, err := env.st.EmailsByID(context.Background(), "acct", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	if len(notFound) > 0 {
		t.Fatalf("unexpected notFound: %v", notFound)
	}
	return list
}

// TestBackfillThenForeignFlagWithinTwoSeconds is the engine half of the
// M1 gate: headers backfill without bodies, then a flag flipped by
// "another IMAP client" reaches the store within the FR-S.7 budget —
// over the IDLE path, not the poll (the poll interval is one hour).
func TestBackfillThenForeignFlagWithinTwoSeconds(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	uid1, _ := env.fx.Append("INBOX", raw("One", "<one@example.test>", "first body"))
	_, _ = env.fx.Append("INBOX", raw("Re: One", "<two@example.test>", "reply body"))
	_, _ = env.fx.Append("INBOX", raw("Two", "<three@example.test>", "second body"))
	env.fx.CreateFolder("Sent", imap.MailboxAttrSent)
	_, _ = env.fx.Append("Sent", raw("Out", "<out@example.test>", "sent body"))

	waitUntil(t, 10*time.Second, "backfill", func() bool {
		mbs, _, err := env.st.Mailboxes(context.Background(), "acct")
		if err != nil || len(mbs) < 2 {
			return false
		}
		return len(env.emails(t)) == 4
	})
	mbs, _, _ := env.st.Mailboxes(context.Background(), "acct")
	var inbox *jmapapi.Mailbox
	for _, mb := range mbs {
		if mb.Role == "inbox" {
			inbox = mb
		}
	}
	if inbox.TotalEmails != 3 || inbox.UnreadEmails != 3 {
		t.Errorf("inbox counts = %d/%d, want 3/3", inbox.TotalEmails, inbox.UnreadEmails)
	}
	if inbox.TotalThreads != 2 { // "One" + reply share a thread
		t.Errorf("inbox threads = %d, want 2", inbox.TotalThreads)
	}
	// Bodies stayed lazy during backfill (golden rule 2).
	for _, e := range env.emails(t) {
		if e.Preview == "" {
			t.Errorf("email %s has no preview after backfill", e.Subject)
		}
	}

	// Wait until the idle connection is actually IDLE, then flip a flag
	// from the foreign client and measure the FR-S.7 budget.
	waitUntil(t, 10*time.Second, "idle watching", func() bool { return env.eng.idleWatching.Load() })
	drain(env.changes)
	t0 := time.Now()
	env.fx.SetFlag("INBOX", uid1, imap.FlagSeen, true)

	waitUntil(t, 2*time.Second, "flag visible", func() bool {
		select {
		case <-env.changes:
		default:
		}
		for _, e := range env.emails(t) {
			if e.Keywords["$seen"] && len(e.MailboxIDs) == 1 && e.MailboxIDs[0] == inbox.ID {
				return true
			}
		}
		return false
	})
	elapsed := time.Since(t0)
	if elapsed > 2*time.Second {
		t.Errorf("foreign flag took %v, budget 2s", elapsed)
	}
	t.Logf("foreign flag visible after %v", elapsed)
}

// TestBaselineTierForeignFlag proves the ladder's bottom rung: without
// CONDSTORE/QRESYNC the baseline pass still notices flips (FR-S.5).
func TestBaselineTierForeignFlag(t *testing.T) {
	env := newEnv(t, fixtureimap.TierBare, nil)
	uid, _ := env.fx.Append("INBOX", raw("M", "<m@example.test>", "body"))
	waitUntil(t, 10*time.Second, "backfill", func() bool { return len(env.emails(t)) == 1 })
	waitUntil(t, 10*time.Second, "idle watching", func() bool { return env.eng.idleWatching.Load() })

	drain(env.changes)
	env.fx.SetFlag("INBOX", uid, imap.FlagFlagged, true)
	waitUntil(t, 2*time.Second, "flag visible", func() bool {
		select {
		case <-env.changes:
		default:
		}
		for _, e := range env.emails(t) {
			if e.Keywords["$flagged"] {
				return true
			}
		}
		return false
	})
}

// TestRestartResumesBackfill proves the FR-S.3 cursor: a new engine on
// the same database continues where the old one stopped instead of
// re-downloading.
func TestRestartResumesBackfill(t *testing.T) {
	fx := fixtureimap.Start(t, fixtureimap.Options{Tier: fixtureimap.TierQResync})
	dir := t.TempDir()
	changes := make(chan struct{}, 64)
	open := func() *store.Store {
		st, err := store.Open(context.Background(), store.Options{
			DataDir: dir,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			Publish: func(string) {},
		})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		return st
	}
	_ = changes
	for i := 1; i <= 3; i++ {
		_, _ = fx.Append("INBOX", raw("seed "+string(rune('0'+i)), "<m"+string(rune('0'+i))+"@example.test>", "body"))
	}
	host, port := splitAddr(t, fx.Addr())
	cfg := Config{
		Account: "acct", Interval: time.Hour, BatchSize: 1,
		IMAP: imapdrv.Config{
			Host: host, Port: port, Username: "test", Password: "test-pass",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}

	// First run: stop after one batch of the three-message backfill.
	st1 := open()
	eng1 := New(cfg, st1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx1, cancel1 := context.WithCancel(context.Background())
	go eng1.Run(ctx1)
	waitUntil(t, 5*time.Second, "cursor past zero", func() bool {
		fs, err := st1.FolderSync(context.Background(), "acct", "INBOX")
		return err == nil && fs.BackfillUID >= 1
	})
	cancel1()
	eng1.disconnect()
	_ = st1.Close()

	// Two more messages arrive while we are down.
	_, _ = fx.Append("INBOX", raw("d", "<d1@example.test>", "while down"))
	_, _ = fx.Append("INBOX", raw("e", "<e1@example.test>", "while down"))

	// Second run resumes and picks up everything, exactly once.
	st2 := open()
	eng2 := New(cfg, st2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx2, cancel2 := context.WithCancel(context.Background())
	go eng2.Run(ctx2)
	defer cancel2()
	waitUntil(t, 10*time.Second, "full mailbox", func() bool {
		return len(emailList(t, st2)) == 5
	})
	seen := map[string]int{}
	for _, e := range emailList(t, st2) {
		seen[e.Subject]++
	}
	for subj, n := range seen {
		if n != 1 {
			t.Errorf("subject %q appears %d times (backfill re-ran)", subj, n)
		}
	}
	eng2.disconnect()
	_ = st2.Close()
}

func emailList(t *testing.T, st *store.Store) []*jmapapi.Email {
	t.Helper()
	list, _, _, err := st.EmailsByID(context.Background(), "acct", nil, false)
	if err != nil {
		t.Fatalf("emails: %v", err)
	}
	return list
}

// TestUIDValidityChangeMintsFreshIDs walks the FR-S.6 drill through
// the whole engine: same folder name, same message, new UIDVALIDITY →
// destroy+create, never a recycled id.
func TestUIDValidityChangeMintsFreshIDs(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	env.fx.CreateFolder("Projects")
	_, _ = env.fx.Append("Projects", raw("Doc", "<doc@example.test>", "content"))

	waitUntil(t, 10*time.Second, "projects backfilled", func() bool {
		return hasSubject(t, env.st, "Doc")
	})
	oldID := ""
	for _, e := range emailList(t, env.st) {
		if e.Subject == "Doc" {
			oldID = e.ID
		}
	}
	if oldID == "" {
		t.Fatal("Doc not backfilled")
	}
	state, _ := env.st.EmailStateString(context.Background(), "acct")

	// Drop and recreate the folder with the same message between passes.
	env.fx.DeleteFolder("Projects")
	env.fx.CreateFolder("Projects")
	_, _ = env.fx.Append("Projects", raw("Doc", "<doc@example.test>", "content"))

	env.eng.requestPass("")
	waitUntil(t, 10*time.Second, "fresh id minted", func() bool {
		for _, e := range emailList(t, env.st) {
			if e.Subject == "Doc" && e.ID != oldID {
				return true
			}
		}
		return false
	})
	// FR-S.6 invariant: exactly one live Doc, and the pre-reset id is
	// never recycled — the old id is tombstoned, a fresh one minted.
	liveDocs := 0
	for _, e := range emailList(t, env.st) {
		if e.Subject != "Doc" {
			continue
		}
		liveDocs++
		if e.ID == oldID {
			t.Errorf("old id %s is live after a UIDVALIDITY reset: ids must never be recycled", oldID)
		}
	}
	if liveDocs != 1 {
		t.Errorf("live Doc count = %d, want 1", liveDocs)
	}
	cs, err := env.st.Changes(context.Background(), "acct", "Email", state)
	if err != nil {
		t.Fatal(err)
	}
	var createdFresh bool
	for _, id := range cs.Created {
		if id != oldID {
			createdFresh = true
		}
	}
	destroyed := false
	for _, id := range cs.Destroyed {
		if id == oldID {
			destroyed = true
		}
	}
	if !destroyed || !createdFresh {
		t.Errorf("changes since reset: created=%v destroyed=%v (old id %s)", cs.Created, cs.Destroyed, oldID)
	}
}

// TestHydrationSingleFlightAndBodies: Email/get with body properties
// hydrates through the engine, concurrent readers share one fetch
// (FR-S.8), and a second read is served from cache.
func TestHydrationSingleFlightAndBodies(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	msg := "From: a@example.test\r\nTo: b@example.test\r\nSubject: Hydrate me\r\n" +
		"Message-ID: <h@example.test>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nThe body text.\r\n"
	_, _ = env.fx.Append("INBOX", []byte(msg))
	waitUntil(t, 10*time.Second, "backfill", func() bool { return len(env.emails(t)) == 1 })

	var id string
	for _, e := range env.emails(t) {
		id = e.ID
	}
	// Two concurrent readers: one fetch (FR-S.8).
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			emails, _, _, err := env.st.EmailsByID(context.Background(), "acct", []string{id}, true)
			if err != nil || len(emails) != 1 {
				return
			}
			bodies[i] = emails[0].BodyValues["1"]
		}(i)
	}
	wg.Wait()
	if bodies[0] != "The body text.\r\n" || bodies[1] != "The body text.\r\n" {
		t.Errorf("bodies = %q / %q", bodies[0], bodies[1])
	}
	if n := env.eng.hydrateFetches.Load(); n != 1 {
		t.Errorf("body fetched %d times, want 1 (single-flight)", n)
	}
	// Cached: a third read must not fetch again.
	emails, _, _, err := env.st.EmailsByID(context.Background(), "acct", []string{id}, true)
	if err != nil || len(emails) != 1 || emails[0].BodyValues["1"] == "" {
		t.Fatalf("cached read failed: %v %#v", err, emails)
	}
	if n := env.eng.hydrateFetches.Load(); n != 1 {
		t.Errorf("body fetched %d times after cache hit", n)
	}
}

// TestPrefetchHydratesRecentBody: FR-S.9's background window.
func TestPrefetchHydratesRecentBody(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, func(c *Config) {
		c.PrefetchWindow = 48 * time.Hour
		c.Concurrency = 1
	})
	msg := "Subject: Prefetch\r\nMessage-ID: <p@example.test>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain\r\n\r\nprefetched body\r\n"
	_, _ = env.fx.Append("INBOX", []byte("From: a@example.test\r\nTo: b@example.test\r\n"+msg))
	waitUntil(t, 10*time.Second, "backfill", func() bool { return len(env.emails(t)) == 1 })
	id := env.emails(t)[0].ID
	waitUntil(t, 10*time.Second, "prefetched body", func() bool {
		ok, err := env.st.IsHydrated(context.Background(), "acct", id)
		return err == nil && ok
	})
}

// TestPreviewFilledOnFirstRead covers PLAN §5's lazy preview: the
// backfill leaves preview empty only until the first summary read.
func TestPreviewFilledOnFirstRead(t *testing.T) {
	env := newEnv(t, fixtureimap.TierQResync, nil)
	_, _ = env.fx.Append("INBOX", raw("Preview me", "<pv@example.test>", "previewable text"))
	waitUntil(t, 10*time.Second, "backfill", func() bool { return len(env.emails(t)) == 1 })
	id := env.emails(t)[0].ID
	emails, _, _, err := env.st.EmailsByID(context.Background(), "acct", []string{id}, false)
	if err != nil || len(emails) != 1 {
		t.Fatalf("read: %v", err)
	}
	if emails[0].Preview == "" {
		t.Errorf("preview empty after read (PREVIEW cap path or partial path must fill it)")
	}
}

func hasSubject(t *testing.T, st *store.Store, subject string) bool {
	t.Helper()
	for _, e := range emailList(t, st) {
		if e.Subject == subject {
			return true
		}
	}
	return false
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

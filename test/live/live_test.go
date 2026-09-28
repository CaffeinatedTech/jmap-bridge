// Package live holds the agent-run live tests: they read credentials
// from JMAP_BRIDGE_TEST_* (AGENTS.md), skip when the environment is
// unset, and never print or persist what they read. Every mutation
// happens inside the designated jmap-bridge-test folder and is cleaned
// up afterwards (live rules of engagement).
package live

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	bridgesync "github.com/CaffeinatedTech/jmap-bridge/internal/sync"
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// testFolder is the only mailbox these tests touch (live rules of
// engagement).
const testFolder = "jmap-bridge-test"

// liveIMAP reads the IMAP credentials or skips.
func liveIMAP(t *testing.T) imapdrv.Config {
	t.Helper()
	host := os.Getenv("JMAP_BRIDGE_TEST_IMAP_HOST")
	if host == "" {
		t.Skip("JMAP_BRIDGE_TEST_IMAP_HOST unset; skipping live test")
	}
	port, err := strconv.Atoi(envOr("JMAP_BRIDGE_TEST_IMAP_PORT", "993"))
	if err != nil {
		t.Fatalf("JMAP_BRIDGE_TEST_IMAP_PORT: %v", err)
	}
	user := os.Getenv("JMAP_BRIDGE_TEST_IMAP_USERNAME")
	pass := os.Getenv("JMAP_BRIDGE_TEST_IMAP_PASSWORD")
	if user == "" || pass == "" {
		t.Skip("JMAP_BRIDGE_TEST_IMAP_USERNAME/PASSWORD unset; skipping live test")
	}
	return imapdrv.Config{
		Host:     host,
		Port:     port,
		TLS:      envOr("JMAP_BRIDGE_TEST_IMAP_TLS", "true") == "true",
		Username: user,
		Password: pass,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// adminSession opens a second, independent IMAP session — the "other
// IMAP client" of the M1 gate (FR-S.7).
func adminSession(t *testing.T, cfg imapdrv.Config) *imapclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := &imapclient.Options{AllowInsecureAuth: !cfg.TLS}
	var (
		c   *imapclient.Client
		err error
	)
	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)
	if cfg.TLS {
		c, err = imapclient.DialTLS(ctx, addr, opts)
	} else {
		c, err = imapclient.Dial(ctx, addr, opts)
	}
	if err != nil {
		t.Fatalf("admin dial: %v", err)
	}
	if err := c.Login(ctx, cfg.Username, cfg.Password, nil); err != nil {
		t.Fatalf("admin login: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestLiveSyncAndForeignFlag is the M1 gate on the bridge side: a real
// account syncs read-only into SQLite (FR-S.1, FR-S.3), and a flag
// flipped by a second IMAP session is visible through the store inside
// the FR-S.7 budget of two seconds (measured while IDLE is healthy).
func TestLiveSyncAndForeignFlag(t *testing.T) {
	cfg := liveIMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	admin := adminSession(t, cfg)

	// Designated test folder: create if absent (live rules), and start
	// from a clean slate every run.
	ensureFolder(t, ctx, admin)
	seed := liveMessage("jmap-bridge live gate", "<bridge-live-gate@example.test>")
	uid := appendLive(t, ctx, admin, testFolder, seed)
	t.Cleanup(func() { cleanupFolder(t, cfg) })

	// Fresh store + engine against the real account.
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

	engineCfg := bridgesync.Config{
		Account:        "livetest",
		IMAP:           cfg,
		Interval:       time.Hour, // IDLE is the delivery path under test
		BatchSize:      200,
		PrefetchWindow: 48 * time.Hour,
		Concurrency:    2,
	}
	eng := bridgesync.New(engineCfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	go eng.Run(engCtx)

	// Backfill: the folder's mail is readable without touching bodies.
	waitFor(t, 90*time.Second, "backfill of the test folder", func() bool {
		return folderHasSubject(t, st, "jmap-bridge live gate")
	})
	mbs, _, err := st.Mailboxes(ctx, "livetest")
	if err != nil {
		t.Fatal(err)
	}
	var testMB *jmapapi.Mailbox
	for _, mb := range mbs {
		if mb.Name == testFolder {
			testMB = mb
		}
	}
	if testMB == nil {
		t.Fatalf("folder %q missing from store (mailboxes: %d)", testFolder, len(mbs))
	}
	if testMB.TotalEmails < 1 {
		t.Fatalf("folder counts = %d, want the seeded message", testMB.TotalEmails)
	}
	t.Logf("backfilled: folder=%s total=%d unread=%d", testMB.Name, testMB.TotalEmails, testMB.UnreadEmails)

	// IDLE must be up before we measure (the poll interval is 1h).
	waitFor(t, 30*time.Second, "idle watching", func() bool { return eng.IdleWatching() })

	// The gate: flip \Seen from the second session, measure to store.
	drain(changes)
	t0 := time.Now()
	setFlag(t, ctx, admin, testFolder, uid, imap.FlagSeen, true)
	waitForBudget(t, 2*time.Second, "foreign flag visible", func() bool {
		select {
		case <-changes:
		default:
		}
		return folderHasKeyword(t, st, testFolder, "$seen")
	})
	t.Logf("foreign flag visible after %v (budget 2s)", time.Since(t0).Round(time.Millisecond))

	// Unflip for a clean re-run, then verify hydration (FR-S.8) fetches
	// the body on demand from the real server.
	setFlag(t, ctx, admin, testFolder, uid, imap.FlagSeen, false)
	emails, _, notFound, err := st.EmailsByID(ctx, "livetest", nil, true)
	if err != nil {
		t.Fatalf("Email/get with bodies: %v", err)
	}
	if len(notFound) > 0 {
		t.Fatalf("notFound: %v", notFound)
	}
	var got *jmapapi.Email
	for _, e := range emails {
		if e.Subject == "jmap-bridge live gate" {
			got = e
		}
	}
	if got == nil {
		t.Fatal("seeded email not returned by Email/get")
	}
	if !strings.Contains(got.BodyValues["1"], "live gate body") {
		t.Errorf("hydrated body = %q, want the seeded text", got.BodyValues["1"])
	}
	if got.Preview == "" {
		t.Error("preview empty after hydration")
	}
}

// --- helpers over the admin session (no test hooks, protocol only) ---

func ensureFolder(t *testing.T, ctx context.Context, c *imapclient.Client) {
	t.Helper()
	// Delete leftovers from a previous run so uidvalidity and counts
	// start known; ignore failure (folder may not exist).
	_ = c.Delete(testFolder, nil).Wait(ctx)
	if err := c.Create(testFolder, nil).Wait(ctx); err != nil {
		t.Fatalf("create %s: %v", testFolder, err)
	}
}

func cleanupFolder(t *testing.T, cfg imapdrv.Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := imapclient.Dial(ctx, cfg.Host+":"+strconv.Itoa(cfg.Port),
		&imapclient.Options{AllowInsecureAuth: !cfg.TLS})
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(ctx, cfg.Username, cfg.Password, nil); err != nil {
		return
	}
	_ = c.Delete(testFolder, nil).Wait(ctx)
}

func liveMessage(subject, msgid string) []byte {
	return []byte("From: jmap-bridge live <bridge@example.test>\r\n" +
		"To: jmap-bridge live <bridge@example.test>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: " + msgid + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"live gate body — seeded by the jmap-bridge M1 test.\r\n")
}

func appendLive(t *testing.T, ctx context.Context, c *imapclient.Client, folder string, raw []byte) uint32 {
	t.Helper()
	data, err := c.Append(ctx, folder, nil, int64(len(raw)), strings.NewReader(string(raw))).Wait(ctx)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return uint32(data.UID)
}

func setFlag(t *testing.T, ctx context.Context, c *imapclient.Client, folder string, uid uint32, flag imap.Flag, on bool) {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(ctx); err != nil {
		t.Fatalf("select %s: %v", folder, err)
	}
	op := imapclient.StoreFlagsAdd
	if !on {
		op = imapclient.StoreFlagsRemove
	}
	if err := c.StoreUID(imap.UIDSetNum(imap.UID(uid)), []imap.Flag{flag},
		&imapclient.StoreOptions{Op: op}).Wait(ctx); err != nil {
		t.Fatalf("store flag: %v", err)
	}
}

// waitFor polls cond until it holds or budget expires.
func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForBudget polls cond until it holds; a timeout is the FR-S.7
// gate failing, so the message carries the budget.
func waitForBudget(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s not observed within the %v budget", what, budget)
}

// drain empties the change signal so the next wait starts clean.
func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func folderHasSubject(t *testing.T, st *store.Store, subject string) bool {
	t.Helper()
	emails, _, _, err := st.EmailsByID(context.Background(), "livetest", nil, false)
	if err != nil {
		return false
	}
	for _, e := range emails {
		if e.Subject == subject {
			return true
		}
	}
	return false
}

func folderHasKeyword(t *testing.T, st *store.Store, folder, keyword string) bool {
	t.Helper()
	mbs, _, err := st.Mailboxes(context.Background(), "livetest")
	if err != nil {
		return false
	}
	var folderID string
	for _, mb := range mbs {
		if mb.Name == folder {
			folderID = mb.ID
		}
	}
	if folderID == "" {
		return false
	}
	emails, _, _, err := st.EmailsByID(context.Background(), "livetest", nil, false)
	if err != nil {
		return false
	}
	for _, e := range emails {
		if !contains(e.MailboxIDs, folderID) {
			continue
		}
		if e.Keywords[keyword] {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

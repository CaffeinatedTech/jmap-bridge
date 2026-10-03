package live

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/go-imap"
	"github.com/CaffeinatedTech/go-imap/imapclient"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	bridgesync "github.com/CaffeinatedTech/jmap-bridge/internal/sync"
)

// The M5 soak (FR-X, NFR-1, NFR-8) runs against the local dev Dovecot:
// it seeds a synthetic 100k-message corpus, syncs it through the real
// engine, and measures the binding numbers — warm Email/query p95,
// hydration rate, idle RSS — plus goroutine stability across engine
// restarts and cold-start responsiveness. Credentials come from
// JMAP_BRIDGE_TEST_* exactly like the other live tests; scale comes
// from JMAP_BRIDGE_SOAK_* so a smaller race-detector pass can reuse
// the same code. Nothing here touches a real provider.

const soakFolder = "jmap-bridge-test/soak"

func soakInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// soakMessage builds one synthetic corpus message: a unique Message-ID
// and body token per message, the shared "quota alpha beta" tokens the
// text queries sweep for, and internal dates spread over a year so
// date filters and thread exemplars have spread.
func soakMessage(i int) string {
	date := time.Now().Add(-time.Duration(i*87) * time.Hour).UTC().Format(time.RFC1123Z)
	return fmt.Sprintf("From: soak sender <soak%06d@soak.test>\r\n"+
		"To: soak target <target@soak.test>\r\n"+
		"Subject: soak fixture %06d alpha beta\r\n"+
		"Message-ID: <soak-%06d@soak.test>\r\n"+
		"Date: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
		"Soak body %06d with searchable terms: quota alpha beta. Unique token zebra%06d. "+
		"Padding follows so the message has a realistic shape: %s\r\n",
		i, i, i, date, i, i, strings.Repeat("filler ", 12))
}

// seedSoak brings the corpus folder up to count via IMAP APPEND,
// idempotently: existing messages stay, the difference is appended.
func seedSoak(t *testing.T, ctx context.Context, cfg struct {
	Host, Port, Username, Password string
}, count int,
) {
	t.Helper()
	c, err := imapclient.Dial(ctx, cfg.Host+":"+cfg.Port,
		&imapclient.Options{AllowInsecureAuth: true})
	if err != nil {
		t.Fatalf("soak seed dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(ctx, cfg.Username, cfg.Password, nil); err != nil {
		t.Fatalf("soak seed login: %v", err)
	}
	_ = c.Create("jmap-bridge-test", nil).Wait(ctx)
	_ = c.Create(soakFolder, nil).Wait(ctx)
	st, err := c.Status(soakFolder, &imapclient.StatusOptions{
		Items: []imap.StatusItem{imap.StatusItemMessages},
	}).Wait(ctx)
	if err != nil {
		t.Fatalf("soak seed status: %v", err)
	}
	have := int(st.NumMessages)
	if have >= count {
		t.Logf("soak corpus already holds %d messages — reusing", have)
		return
	}
	start := time.Now()
	for i := have; i < count; i++ {
		msg := soakMessage(i)
		if _, err := c.Append(ctx, soakFolder, nil, int64(len(msg)),
			strings.NewReader(msg)).Wait(ctx); err != nil {
			t.Fatalf("soak append %d: %v", i, err)
		}
		if (i-have)%10000 == 9999 {
			t.Logf("soak seed: %d/%d (%.0f/s)", i+1-have, count-have,
				float64(i+1-have)/time.Since(start).Seconds())
		}
	}
	t.Logf("soak seed: appended %d messages in %v", count-have, time.Since(start))
}

func vmRSS() (int64, error) {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, err := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("VmRSS not found")
}

// percentile returns the p-th sample of a sorted duration slice.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted) - 1) * p / 100
	return sorted[i]
}

// The race detector instruments every memory access: the identical
// workload runs ~10-40× slower and its shadow memory dominates RSS.
// NFR-1/NFR-8 are binding and demonstrated by the plain soak, so under
// -race the thresholds below widen to cover instrumentation while
// staying tight enough to catch a genuine stall or leak. The -race
// run's own gate is the detector itself: it must report no data race.
const (
	// soakRaceScale multiplies wall-clock budgets under -race.
	soakRaceScale = 40
	// soakHydrationFloor is the slowest acceptable body rate; under
	// -race only a pathological stall should trip it.
	soakHydrationFloor     = 15
	soakHydrationFloorRace = 2
	// soakRSSBudget bounds idle RSS (NFR-8); under -race the
	// detector's shadow memory raises the floor.
	soakRSSBudget     = 150 << 20
	soakRSSBudgetRace = 1500 << 20
)

// soakBudget scales a wall-clock budget for the race detector.
func soakBudget(d time.Duration) time.Duration {
	if raceEnabled {
		return d * soakRaceScale
	}
	return d
}

// soakHydrationRate returns the minimum acceptable hydration rate.
func soakHydrationRate() float64 {
	if raceEnabled {
		return soakHydrationFloorRace
	}
	return soakHydrationFloor
}

// soakRSS returns the idle-RSS ceiling for this build.
func soakRSS() int64 {
	if raceEnabled {
		return soakRSSBudgetRace
	}
	return soakRSSBudget
}

// TestLiveSoak100k is the M5 soak gate. Long by design: it seeds and
// syncs a full corpus before measuring. Run it directly:
//
//	JMAP_BRIDGE_TEST_IMAP_HOST=127.0.0.1 JMAP_BRIDGE_TEST_IMAP_PORT=143 \
//	JMAP_BRIDGE_TEST_IMAP_TLS=false \
//	JMAP_BRIDGE_TEST_IMAP_USERNAME=jmaptest JMAP_BRIDGE_TEST_IMAP_PASSWORD=… \
//	  go test ./test/live/ -run TestLiveSoak100k -v -timeout 2h
func TestLiveSoak100k(t *testing.T) {
	cfg := liveIMAP(t) // skips when the env is unset
	count := soakInt("JMAP_BRIDGE_SOAK_COUNT", 100000)
	hydrateN := soakInt("JMAP_BRIDGE_SOAK_HYDRATE", 10000)
	dataDir := envOr("JMAP_BRIDGE_SOAK_DIR", filepath.Join(os.TempDir(), "jmap-bridge-soak"))
	if os.Getenv("JMAP_BRIDGE_SOAK_FRESH") == "1" {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Fatalf("fresh soak dir: %v", err)
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("soak dir: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	seedHost := struct{ Host, Port, Username, Password string }{
		cfg.Host, strconv.Itoa(cfg.Port), cfg.Username, cfg.Password,
	}
	seedSoak(t, ctx, seedHost, count)

	st, err := store.Open(ctx, store.Options{DataDir: dataDir, Logger: quiet})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// --- engine sync: headers backfill, bodies stay lazy ---
	runEngine := func() (cancel context.CancelFunc) {
		eng := bridgesync.New(bridgesync.Config{
			Account:        "soak",
			NewBackend:     liveBackend(cfg),
			Interval:       time.Hour,
			BatchSize:      500,
			PrefetchWindow: 0, // bodies only via explicit hydration: NFR-8 stays measurable
			Concurrency:    4,
			SearchBackfill: true,
		}, st, quiet)
		ectx, ecancel := context.WithCancel(context.Background())
		go eng.Run(ectx)
		return ecancel
	}
	cancel := runEngine()
	defer cancel()

	var soakID string
	waitForBudget(t, 45*time.Minute, "soak corpus fully synced", func() bool {
		mbs, _, err := st.Mailboxes(ctx, "soak")
		if err != nil {
			return false
		}
		for _, m := range mbs {
			if m.Path == soakFolder && m.TotalEmails == count {
				soakID = m.ID
				return true
			}
			if m.Path == soakFolder {
				soakID = m.ID
			}
		}
		return false
	})
	if soakID == "" {
		t.Fatal("soak mailbox not discovered")
	}
	t.Logf("corpus of %d messages synced", count)

	// --- NFR-1: warm Email/query over the 100k folder ---
	shapes := []struct {
		name string
		q    jmapapi.EmailQuery
	}{
		{"browse", jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: soakID}, Limit: 50,
		}},
		{"collapse", jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: soakID}, Limit: 50, CollapseThreads: true,
		}},
		{"text", jmapapi.EmailQuery{
			Filter: jmapapi.EmailFilter{InMailbox: soakID, Text: "quota alpha beta"},
			Limit:  50, CollapseThreads: true,
		}},
	}
	const iters = 60
	for _, shape := range shapes {
		var ds []time.Duration
		var total int
		for i := 0; i < iters; i++ {
			t0 := time.Now()
			_, _, tot, _, err := st.QueryEmails(ctx, "soak", shape.q)
			if err != nil {
				t.Fatalf("%s query: %v", shape.name, err)
			}
			total = tot
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
		p50, p95 := percentile(ds, 50), percentile(ds, 95)
		t.Logf("NFR-1 %s query: total=%d p50=%v p95=%v", shape.name, total, p50, p95)
		if budget := soakBudget(150 * time.Millisecond); p95 > budget {
			t.Errorf("%s query p95 = %v, want < %v (NFR-1)", shape.name, p95, budget)
		}
	}

	// Session → first mailbox list, warm (NFR-1).
	t0 := time.Now()
	if _, _, err := st.Mailboxes(ctx, "soak"); err != nil {
		t.Fatal(err)
	}
	if budget := soakBudget(2 * time.Second); time.Since(t0) > budget {
		t.Errorf("warm mailbox list took %v, want < %v (NFR-1)", time.Since(t0), budget)
	}

	// --- hydration: 10k bodies through the interactive path ---
	ids, _, _, _, err := st.QueryEmails(ctx, "soak", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{InMailbox: soakID}, Limit: hydrateN,
	})
	if err != nil || len(ids) < hydrateN {
		t.Fatalf("hydration ids: %v %d/%d", err, len(ids), hydrateN)
	}
	hStart := time.Now()
	const batch = 100
	for start := 0; start < hydrateN; start += batch {
		end := min(start+batch, hydrateN)
		if _, _, _, err := st.EmailsByID(ctx, "soak", ids[start:end], true); err != nil {
			t.Fatalf("hydrate batch: %v", err)
		}
	}
	rate := float64(hydrateN) / time.Since(hStart).Seconds()
	t.Logf("hydration: %d bodies in %v (%.1f/s)", hydrateN, time.Since(hStart), rate)
	if floor := soakHydrationRate(); rate < floor {
		t.Errorf("hydration rate = %.1f/s, want ≥ %.0f/s (NFR-1)", rate, floor)
	}

	// A body-only token must now match, and queryState must have moved.
	probeToken := "zebra" + fmt.Sprintf("%06d", 42) // unique to message 42
	_, _, total, counter, err := st.QueryEmails(ctx, "soak", jmapapi.EmailQuery{
		Filter: jmapapi.EmailFilter{Text: probeToken},
	})
	if err != nil || total != 1 {
		t.Fatalf("body-token search: total=%d err=%v", total, err)
	}
	if !strings.Contains(counter, ":") {
		t.Errorf("query counter %q lacks the search-index component (FR-X.7)", counter)
	}

	// --- NFR-8: idle RSS with the corpus cached and 10k bodies ---
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(2 * time.Second)
	rss, err := vmRSS()
	if err != nil {
		t.Fatalf("rss: %v", err)
	}
	t.Logf("NFR-8 idle RSS: %.1f MB (target < 150 MB)", float64(rss)/(1<<20))
	if budget := soakRSS(); rss > budget {
		t.Errorf("idle RSS = %.1f MB, want < %.0f MB (NFR-8)", float64(rss)/(1<<20), float64(budget)/(1<<20))
	}

	// --- reconnect cycles: engine restarts must not leak goroutines ---
	cancel()
	time.Sleep(2 * time.Second)
	runtime.GC()
	base := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		c := runEngine()
		waitForBudget(t, 30*time.Second, fmt.Sprintf("cycle %d reconnect", i), func() bool {
			mbs, _, err := st.Mailboxes(ctx, "soak")
			return err == nil && len(mbs) > 0
		})
		time.Sleep(3 * time.Second) // let IDLE/backoff settle
		c()
		time.Sleep(2 * time.Second)
		runtime.GC()
		now := runtime.NumGoroutine()
		t.Logf("reconnect cycle %d: goroutines %d (baseline %d)", i, now, base)
		if now > base+4 {
			t.Errorf("goroutines after cycle %d = %d, baseline %d — leak suspected", i, now, base)
		}
	}

	// --- cold browse: reopen the store, first query must stay sane ---
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(ctx, store.Options{DataDir: dataDir, Logger: quiet})
	if err != nil {
		t.Fatalf("cold reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	for _, shape := range shapes[:2] {
		t0 := time.Now()
		if _, _, _, _, err := st2.QueryEmails(ctx, "soak", shape.q); err != nil {
			t.Fatalf("cold %s query: %v", shape.name, err)
		}
		d := time.Since(t0)
		t.Logf("cold %s query: %v", shape.name, d)
		if budget := soakBudget(time.Second); d > budget {
			t.Errorf("cold %s query took %v — exceeds %v", shape.name, d, budget)
		}
	}
}

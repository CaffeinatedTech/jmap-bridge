// Package sync is the per-account sync engine (PLAN §5): discovery and
// tier detection, resumable header backfill, tier-aware incremental
// passes, IDLE-driven foreign-change detection (FR-S.7), lazy body
// hydration with single-flight (FR-S.8), preview fill and bounded
// prefetch (FR-S.9).
//
// One engine owns one account: a work connection (serialised behind
// one mutex — passes, hydration and prefetch all take it) and an idle
// connection that does nothing but watch. IMAP-first is not this
// package's concern yet (M2 writes); here every mutation on the store
// is a faithful reflection of what the server already said.
package sync

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/dav"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/submit"
)

// Config is one engine's tunables, straight from the [sync] config
// block (M0 decoded it; M1 uses it).
type Config struct {
	Account        string
	IMAP           imapdrv.Config
	SMTP           *submit.Config // nil: the account has no submission server (FR-J.5)
	CardDAV        *dav.Config    // nil: contacts are off and the capability is never advertised (FR-P.3)
	Interval       time.Duration  // poll fallback for non-idled folders
	BatchSize      int            // backfill batch (FR-S.3)
	PrefetchWindow time.Duration  // 0 disables prefetch (FR-S.9)
	Concurrency    int            // hydration workers (FR-S.9 rate limit)
	SearchBackfill bool           // hydrate text-search candidates in the background (FR-X.5)
}

// Engine runs one account's sync.
type Engine struct {
	cfg Config
	st  *store.Store
	log *slog.Logger

	// workMu serialises every use of the work connection.
	workMu    sync.Mutex
	work      *imapdrv.Conn
	folders   []string // discovery order, refreshed each pass
	connected bool

	// wake carries pass requests: a folder hint, or "" for a full pass.
	wake chan string

	// wr is the dedicated write session (PLAN §7.1): mutations never
	// queue behind a pass holding workMu.
	wr *writer

	// idleFolder is the folder the idle connection watches (the inbox
	// role when discovery finds one); idleLoop reads it under idleMu.
	idleMu     sync.Mutex
	idleFolder string

	// hydration single-flight (FR-S.8)
	flightMu sync.Mutex
	flights  map[string]*flight

	// prefetch bounding (FR-S.9)
	prefetchSem chan struct{}

	// search-driven backfill lane (FR-X.5): buffered, dropped on
	// overflow, served by Concurrency workers started in Run.
	backfill chan string

	// idleWatching flips once the idle connection has entered IDLE on
	// the watched folder; the live gate and tests wait on it so the
	// measured latency is the IDLE path, not the poll fallback.
	idleWatching atomic.Bool
	// hydrateFetches counts actual IMAP body fetches; single-flight
	// (FR-S.8) means concurrent readers must not raise it (tests).
	hydrateFetches atomic.Int64

	// ownWrites remembers the (folder → uid → when) pairs of our own
	// mutations, so the reconcile paths can spare them from tombstoning
	// while Gmail's lists catch up (grace window, FR-S.12).
	ownMu     sync.Mutex
	ownWrites map[string]map[uint32]time.Time

	// kick wakes the run loop out of its backoff sleep: the OAuth
	// callback uses it when credentials just landed, so the next pass
	// does not wait out a five-minute timer.
	kick chan struct{}

	// CardDAV session (M6, PLAN §8): nil until opened, then reused.
	// The contacts loop owns it; writes borrow it under davMu.
	davMu   sync.Mutex
	davSess *dav.Session
	// contactsKick nudges the contacts loop (shared Kick).
	contactsKick chan struct{}
	// contactsReady gates the urn:ietf:params:jmap:contacts capability:
	// CardDAV configured AND the first full sync succeeded (FR-P.3).
	contactsReady atomic.Bool
}

// errNotConnected means the work connection is down; the run loop
// reconnects on the next cycle.
var errNotConnected = errors.New("sync: not connected")

// New returns an engine and wires it as the store's Ensure hook: the
// HTTP layer's Email/get asks the store for bodies, the store asks
// this engine, and the engine blocks on the IMAP fetch (FR-S.8).
func New(cfg Config, st *store.Store, log *slog.Logger) *Engine {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	e := &Engine{
		cfg:          cfg,
		st:           st,
		log:          log,
		wr:           newWriter(cfg.IMAP, log),
		wake:         make(chan string, 8),
		kick:         make(chan struct{}, 1),
		flights:      map[string]*flight{},
		prefetchSem:  make(chan struct{}, cfg.Concurrency),
		backfill:     make(chan string, 1024),
		idleFolder:   "INBOX",
		contactsKick: make(chan struct{}, 1),
	}
	st.Ensure = e.Ensure
	if cfg.SearchBackfill {
		st.SearchBackfill = e.searchBackfill
	}
	return e
}

// Kick nudges the engine to run a pass now, skipping any backoff sleep:
// the OAuth callback calls it after credentials land (FR-A.5).
func (e *Engine) Kick() {
	e.requestPass("")
	select {
	case e.kick <- struct{}{}:
	default:
	}
	if e.cfg.CardDAV != nil {
		select {
		case e.contactsKick <- struct{}{}:
		default:
		}
	}
}

// Run drives the engine until ctx ends: connect, pass, repeat, with
// exponential backoff on failure (FR-S.4) and the idle loop alongside.
func (e *Engine) Run(ctx context.Context) {
	go e.idleLoop(ctx)
	if e.cfg.SearchBackfill {
		e.backfillWorkers(ctx)
	}
	if e.cfg.CardDAV != nil {
		go e.contactsLoop(ctx)
	}

	e.requestPass("")
	failures := 0
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			e.disconnect()
			e.wr.close()
			return
		}
		if !e.isWorkConnected() {
			if err := e.connect(ctx); err != nil {
				e.log.Warn("sync: connect failed", "account", e.cfg.Account, "err", err)
				failures++
				if !e.sleepOrKick(ctx, backoff(failures)) {
					return
				}
				continue
			}
		}

		var hint string
		select {
		case hint = <-e.wake:
		case <-ticker.C:
		case <-ctx.Done():
			e.disconnect()
			e.wr.close()
			return
		}
		if err := e.doPass(ctx, hint); err != nil {
			e.log.Warn("sync: pass failed", "account", e.cfg.Account, "err", err)
			e.disconnect()
			// A [THROTTLED] pass is the provider saying "back off": the
			// standard failure ladder would knock again in seconds.
			// Cooldown first (FR-S.12); a kick still wakes us early.
			if imapdrv.IsThrottled(err) {
				e.log.Warn("sync: provider throttled the account, cooling down",
					"account", e.cfg.Account, "cooldown", throttleCooldown.String())
				failures = 0
				if !e.sleepOrKick(ctx, throttleCooldown) {
					e.wr.close()
					return
				}
				continue
			}
			failures++
			if !e.sleepOrKick(ctx, backoff(failures)) {
				e.wr.close()
				return
			}
			continue
		}
		failures = 0
	}
}

// throttleCooldown is how long the engine stays quiet after a pass the
// provider answered with [THROTTLED] — long enough for Gmail's rate
// window to reset, short enough that the next IDLE tick recovers
// without operator help.
const throttleCooldown = 15 * time.Minute

// sleepOrKick sleeps d, returning early when the engine was kicked (the
// OAuth callback landed credentials mid-backoff, FR-A.5) or ctx ended.
// The bool reports whether the loop should continue.
func (e *Engine) sleepOrKick(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-e.kick:
		return true
	}
}

// requestPass queues a pass for hint ("" = every folder); a full pass
// already covers any hint, so a full queue wins.
func (e *Engine) requestPass(hint string) {
	// Collapse a pending full pass before queueing anything narrower.
	select {
	case pending := <-e.wake:
		if pending == "" {
			hint = ""
		}
	default:
	}
	select {
	case e.wake <- hint:
	default: // queue full: a pass is already pending and covers us
	}
}

// backoff is exponential with jitter, capped at five minutes
// (FR-S.4).
func backoff(failures int) time.Duration {
	const max = 5 * time.Minute
	d := time.Second << min(failures, 9)
	if d > max {
		d = max
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1)) //nolint:gosec // jitter, not security
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// connect dials the work connection; the pass that follows discovers
// folders (FR-S.1).
func (e *Engine) connect(ctx context.Context) error {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work != nil {
		return nil
	}
	conn, err := imapdrv.Dial(ctx, e.cfg.IMAP)
	if err != nil {
		return err
	}
	e.work = conn
	e.connected = true
	e.wr.noteGmail(conn.GmailExt())
	e.log.Info("sync: connected",
		"account", e.cfg.Account, "tier", conn.Tier().String(),
		"compressed", conn.Compressed())
	return nil
}

func (e *Engine) isWorkConnected() bool {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	return e.work != nil
}

// disconnect drops the work connection (the next loop dials again).
func (e *Engine) disconnect() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work != nil {
		_ = e.work.Close()
		e.work = nil
		e.connected = false
	}
}

// doPass runs one discovery + incremental cycle. hint names a folder
// that just notified us; it is synced first so IDLE latency (FR-S.7)
// does not wait behind the other folders.
func (e *Engine) doPass(ctx context.Context, hint string) error {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.work == nil {
		return errNotConnected
	}
	start := time.Now()
	statuses, err := e.discoverLocked(ctx)
	if err != nil {
		return err
	}
	order := e.folders
	if hint != "" && contains(order, hint) {
		order = append([]string{hint}, filter(order, hint)...)
	}
	synced := 0
	for _, name := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, ok := statuses[name]
		if !ok {
			continue
		}
		// A folder-level failure is logged and retried next pass; only
		// discovery failures (connection trouble) unwind to the run loop.
		if err := e.syncFolderLocked(ctx, name, st); err != nil {
			e.log.Warn("sync: folder pass failed", "folder", name, "err", err)
			continue
		}
		synced++
	}
	// Release the last folder's selection so administrative commands
	// (DELETE/RENAME from another session) are not refused for it.
	if err := e.work.Unselect(ctx); err != nil {
		e.log.Debug("sync: unselect after pass", "err", err)
	}
	e.log.Debug("sync: pass done",
		"account", e.cfg.Account, "folders", synced, "took", time.Since(start).String())
	e.prefetch(ctx)
	return nil
}

// IdleWatching reports whether the idle connection is currently inside
// IDLE on the watched folder — the state the FR-S.7 latency budget
// assumes, and what the live gate waits for before measuring.
func (e *Engine) IdleWatching() bool { return e.idleWatching.Load() }

// setCurrentIdleFolder records which folder the idle connection should
// watch; discovery calls this (FR-S.4).
func (e *Engine) setCurrentIdleFolder(name string) {
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	if name != "" {
		e.idleFolder = name
	}
}

func (e *Engine) currentIdleFolder() string {
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	return e.idleFolder
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func filter(list []string, skip string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != skip {
			out = append(out, v)
		}
	}
	return out
}

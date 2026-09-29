// Command jmap-bridge is a containerised JMAP server fronting IMAP/SMTP
// accounts and their CardDAV contacts (REQUIREMENTS.md). M1 serves the
// session, the batched /jmap endpoint and the eventsource from a
// SQLite cache that a per-account sync engine keeps faithful to the
// IMAP server (PLAN §5).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/config"
	"github.com/CaffeinatedTech/jmap-bridge/internal/httpapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/sync"
)

// version is overridden at build time via -ldflags.
var version = "dev"

// tombstoneRetention is NFR-4's /changes replay window.
const tombstoneRetention = 30 * 24 * time.Hour

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("jmap-bridge", flag.ContinueOnError)
	configPath := fs.String("config", "config.toml", "path to the TOML configuration file")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("jmap-bridge %s\n", version)
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	tokens := auth.NewTokens(tokenMap(cfg))
	hub := push.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Options{
		DataDir: cfg.DataDir,
		Logger:  log,
		Publish: hub.Publish,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("store close failed", "err", err)
		}
	}()

	// One engine per account that has an IMAP backend; an account
	// without one serves whatever the cache holds (log, never lie) and
	// gets no Backend, so its writes fail instead of pretending.
	engines := 0
	backends := map[string]jmapapi.Backend{}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.IMAP == nil {
			log.Warn("account has no [imap] block; serving cache only", "account", a.ID)
			continue
		}
		eng := sync.New(syncConfig(cfg, a), st, log)
		backends[a.ID] = eng
		go eng.Run(ctx)
		engines++
	}

	go purgeLoop(ctx, st, log)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(cfg, tokens, st, backends, hub, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("jmap-bridge starting",
		"version", version,
		"listen", cfg.Listen,
		"base_url", cfg.BaseURL,
		"auth_mode", cfg.Auth.Mode,
		"accounts", accountIDs(cfg),
		"engines", engines,
		"backend", "sqlite+imap (M1)",
	)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Engines stop via ctx; the store checkpoints on Close above.
		return srv.Shutdown(shutdownCtx)
	}
}

// syncConfig lifts one account's [sync]/[search]/[imap] settings into
// the engine's view.
func syncConfig(cfg *config.Config, a *config.Account) sync.Config {
	tls := true
	if a.IMAP.TLS != nil {
		tls = *a.IMAP.TLS
	}
	return sync.Config{
		Account: a.ID,
		IMAP: imapdrv.Config{
			Host:     a.IMAP.Host,
			Port:     a.IMAP.Port,
			TLS:      tls,
			Username: a.IMAP.Username,
			Password: a.IMAP.Password,
		},
		Interval:       cfg.Sync.Interval.Std(),
		BatchSize:      cfg.Sync.BatchSize,
		PrefetchWindow: cfg.Sync.PrefetchWindow.Std(),
		Concurrency:    cfg.Search.Concurrency,
	}
}

// purgeLoop applies NFR-4's tombstone retention daily: /changes stays
// replayable for at least 30 days, older tombstones go away with the
// replay floor raised so stale sinceStates degrade honestly.
func purgeLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.PurgeTombstones(ctx, tombstoneRetention); err != nil {
				log.Warn("tombstone purge failed", "err", err)
			}
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// tokenMap is the account→token pair set used for Basic verification.
// Tokens are read from memory only and never logged (FR-A.2).
func tokenMap(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Accounts))
	for i := range cfg.Accounts {
		out[cfg.Accounts[i].ID] = cfg.Accounts[i].Token
	}
	return out
}

// accountIDs lists configured account ids for the startup log line;
// ids are not secret, token values are never included.
func accountIDs(cfg *config.Config) string {
	ids := make([]string, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		ids = append(ids, cfg.Accounts[i].ID)
	}
	return strings.Join(ids, ",")
}

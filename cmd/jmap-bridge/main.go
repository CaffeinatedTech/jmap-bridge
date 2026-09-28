// Command jmap-bridge is a containerised JMAP server fronting IMAP/SMTP
// accounts and their CardDAV contacts (REQUIREMENTS.md). M0 serves the
// session, the batched /jmap endpoint, and deterministic fixture mail;
// the sync engine lands in M1.
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
	"github.com/CaffeinatedTech/jmap-bridge/internal/fixture"
	"github.com/CaffeinatedTech/jmap-bridge/internal/httpapi"
)

// version is overridden at build time via -ldflags.
var version = "dev"

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
	store := fixture.New() // M0: no sync engine yet; M1 swaps internal/store in here
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(cfg, tokens, store, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("jmap-bridge starting",
		"version", version,
		"listen", cfg.Listen,
		"base_url", cfg.BaseURL,
		"auth_mode", cfg.Auth.Mode,
		"accounts", accountIDs(cfg),
		"backend", "fixture (M0)",
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		return srv.Shutdown(shutdownCtx)
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

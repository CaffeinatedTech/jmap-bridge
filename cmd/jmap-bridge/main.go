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
	"github.com/CaffeinatedTech/jmap-bridge/internal/dav"
	"github.com/CaffeinatedTech/jmap-bridge/internal/httpapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/metrics"
	"github.com/CaffeinatedTech/jmap-bridge/internal/oauth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/push"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/submit"
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
	// The FR-D.6 registry is built before the engines so they can
	// register their gauges; whether it is exposed is gated by
	// cfg.Metrics.Enabled at the HTTP wrapper below.
	reg := metrics.New()

	// Credential encryption at rest (FR-A.8): with a key configured the
	// OAuth2 tokens are sealed before they land in SQLite; without one
	// the bridge runs in plaintext mode and says so loudly (PLAN §9).
	var cipher *auth.Cipher
	if key := os.Getenv("JMAP_BRIDGE_SECRET_KEY"); key != "" {
		c, err := auth.NewCipher(key)
		if err != nil {
			return err
		}
		cipher = c
		log.Info("credential encryption enabled", "key_fingerprint", cipher.Fingerprint())
	} else if anyOAuth(cfg) {
		log.Warn("JMAP_BRIDGE_SECRET_KEY is not set: OAuth2 tokens will be stored UNENCRYPTED " +
			"in the data volume (plaintext mode). Configure a 32-byte key before production use.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Options{
		DataDir:      cfg.DataDir,
		Logger:       log,
		Publish:      hub.Publish,
		BackfillScan: cfg.Search.BackfillScan,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("store close failed", "err", err)
		}
	}()

	// One OAuth2 client per account that has an [accounts.oauth2] block
	// (FR-A.5); the sync engines and the HTTP bootstrap share it.
	ts := &tokenStore{st: st}
	managers := map[string]*oauth.Manager{}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.OAuth2 == nil {
			continue
		}
		m, err := oauth.NewManager(a.ID, oauthConfig(a), cfg.BaseURL, ts, cipher, log)
		if err != nil {
			return err
		}
		managers[a.ID] = m
	}

	// One engine per account that has an IMAP backend; an account
	// without one serves whatever the cache holds (log, never lie) and
	// gets no Backend, so its writes fail instead of pretending.
	engines := map[string]*sync.Engine{}
	backends := map[string]jmapapi.Backend{}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if a.IMAP == nil {
			log.Warn("account has no [imap] block; serving cache only", "account", a.ID)
			continue
		}
		eng := sync.New(syncConfig(cfg, a, managers[a.ID], reg), st, log)
		backends[a.ID] = eng
		engines[a.ID] = eng
		go eng.Run(ctx)
	}
	// One Ensure/SearchBackfill hook serves every account, so route them
	// by account: without this the last engine built would hydrate every
	// account's reads and reject the rest as "foreign account".
	st.Ensure, st.SearchBackfill = sync.Router(engines, cfg.Search.Backfill)

	go purgeLoop(ctx, st, log)

	api := httpapi.New(cfg, tokens, st, backends, hub, log, managers,
		kick(engines, log), contactsReady(engines), httpapi.WithMetrics(reg))
	// The SSE gauge needs the server, so it is registered after the
	// handler exists: the total is one series, each account another
	// (FR-D.6).
	reg.GaugeFunc("jmap_bridge_sse_clients",
		"Open JMAP EventSource connections, total and by account.",
		func() []metrics.Sample {
			total, perAccount := api.SSECounts()
			samples := make([]metrics.Sample, 0, len(perAccount)+1)
			samples = append(samples, metrics.Sample{
				Labels: []metrics.Label{{Name: "account", Value: ""}},
				Value:  float64(total),
			})
			for account, n := range perAccount {
				samples = append(samples, metrics.Sample{
					Labels: []metrics.Label{{Name: "account", Value: account}},
					Value:  float64(n),
				})
			}
			return samples
		})

	handler := withHealth(api, readiness(engines, cfg.Accounts))
	if cfg.Metrics.Enabled {
		// Opt-in (FR-D.6): when off the route is never mounted, so
		// /metrics 404s exactly like any unknown path and cannot be
		// probed.
		handler = withMetrics(handler, reg)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout also covers the upload body, so it is generous
		// enough for a 64 MiB attachment on a slow link while still
		// bounding a slow-loris request. WriteTimeout stays 0: it would
		// kill long-lived EventSource responses.
		ReadTimeout:    5 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	log.Info("jmap-bridge starting",
		"version", version,
		"listen", cfg.Listen,
		"base_url", cfg.BaseURL,
		"auth_mode", cfg.Auth.Mode,
		"accounts", accountIDs(cfg),
		"engines", len(engines),
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

// syncConfig lifts one account's [sync]/[search]/[imap]/[smtp] settings
// into the engine's view. An account with no [smtp] block gets a nil
// SMTP: it can be read from and written to, but not sent from (FR-J.5).
// The account's OAuth2 manager, when one exists, is the token provider
// for both IMAP and SMTP XOAUTH2 (FR-A.7).
func syncConfig(cfg *config.Config, a *config.Account, mgr *oauth.Manager, reg *metrics.Registry) sync.Config {
	tls := true
	if a.IMAP.TLS != nil {
		tls = *a.IMAP.TLS
	}
	imapCfg := imapdrv.Config{
		Host:     a.IMAP.Host,
		Port:     a.IMAP.Port,
		TLS:      tls,
		Username: a.IMAP.Username,
		Password: a.IMAP.Password,
	}
	if a.IMAP.Auth == "oauth2" && mgr != nil {
		imapCfg.Token = mgr.AccessToken
	}
	out := sync.Config{
		Account: a.ID,
		// D-API-2: the engine sees only mailbackend.Backend. The IMAP
		// factory is supplied here; a backend = "gmail_api" account will
		// supply the Gmail API factory in its place (M10).
		NewBackend:     func() mailbackend.Backend { return imapdrv.NewBackend(imapCfg) },
		Interval:       cfg.Sync.Interval.Std(),
		BatchSize:      cfg.Sync.BatchSize,
		PrefetchWindow: cfg.Sync.PrefetchWindow.Std(),
		Concurrency:    cfg.Search.Concurrency,
		SearchBackfill: cfg.Search.Backfill,
		Metrics:        reg,
	}
	if a.CardDAV != nil && a.CardDAV.URL != "" {
		davCfg := dav.Config{
			URL:      a.CardDAV.URL,
			Username: a.CardDAV.Username,
			Password: a.CardDAV.Password,
		}
		if a.CardDAV.Auth == "oauth2" && mgr != nil {
			// FR-A.9: the Google profile carries the carddav scope; the
			// same manager already refreshed for IMAP/SMTP answers DAV
			// too.
			davCfg.Token = func(ctx context.Context) (string, error) {
				return mgr.AccessToken(ctx, false)
			}
		}
		out.CardDAV = &davCfg
	}
	if a.SMTP != nil {
		out.SMTP = &submit.Config{
			Host:     a.SMTP.Host,
			Port:     a.SMTP.Port,
			TLS:      a.SMTP.TLS,
			Auth:     a.SMTP.Auth,
			Username: a.SMTP.Username,
			Password: a.SMTP.Password,
		}
		if a.SMTP.Auth == "oauth2" && mgr != nil {
			out.SMTP.Token = mgr.AccessToken
		}
	}
	return out
}

// oauthConfig lifts the [accounts.oauth2] block into the oauth package's
// view; HasCardDAV names the contacts scope for provider = "google"
// (FR-A.9 — M6 fills contacts, the scope rides along now).
func oauthConfig(a *config.Account) oauth.Config {
	o := a.OAuth2
	return oauth.Config{
		Provider:     o.Provider,
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		AuthURL:      o.AuthURL,
		TokenURL:     o.TokenURL,
		Scopes:       o.Scopes,
		HasCardDAV:   a.CardDAV != nil && a.CardDAV.URL != "",
	}
}

// tokenStore adapts the store's OAuth token table to the oauth package's
// TokenStore interface (FR-A.8: values arrive sealed and are stored
// exactly as handed over).
type tokenStore struct{ st *store.Store }

func (t *tokenStore) LoadToken(ctx context.Context, account string) (string, string, time.Time, error) {
	tok, err := t.st.OAuthToken(ctx, account)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return tok.RefreshToken, tok.AccessToken, tok.AccessExpiry, nil
}

func (t *tokenStore) SaveToken(ctx context.Context, account, refresh, access string, expiry time.Time) error {
	return t.st.SaveOAuthToken(ctx, account, store.OAuthToken{
		RefreshToken: refresh, AccessToken: access, AccessExpiry: expiry,
	})
}

func (t *tokenStore) ClearToken(ctx context.Context, account string) error {
	return t.st.ClearOAuthToken(ctx, account)
}

// kick builds the post-consent wake callback: credentials just landed,
// so the account's engine runs a pass immediately instead of waiting out
// its backoff (FR-A.5).
func kick(engines map[string]*sync.Engine, log *slog.Logger) func(account string) {
	return func(account string) {
		eng, ok := engines[account]
		if !ok {
			return
		}
		eng.Kick()
		log.Info("oauth: engine kicked", "account", account)
	}
}

// contactsReady builds the FR-P.3 capability gate for the session: an
// account is ready only when its engine reports the first CardDAV sync
// succeeded. Accounts without an engine (no IMAP, or no CardDAV block)
// read as not-ready forever.
func contactsReady(engines map[string]*sync.Engine) func(account string) bool {
	return func(account string) bool {
		eng, ok := engines[account]
		return ok && eng.ContactsReady()
	}
}

// withHealth mounts the FR-D.4 probe endpoints ahead of the JMAP
// surface. They are deliberately unauthenticated (a kubelet probe
// carries no client token) and deliberately terse: /readyz says only
// whether every account has synced, never which account or why, so an
// internet-facing bridge does not hand an anonymous GET its account ids.
func withHealth(next http.Handler, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ready != nil && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, "not ready\n")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ready\n")
	})
	mux.Handle("/", next)
	return mux
}

// withMetrics mounts the opt-in FR-D.6 endpoint ahead of the rest of the
// handler. It is installed only when metrics are enabled, so a disabled
// bridge answers /metrics with the same 404 an unknown path gets. The
// endpoint shares the configured origin and listener (golden rule 3).
func withMetrics(next http.Handler, reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = reg.WriteTo(w)
	})
	mux.Handle("/", next)
	return mux
}

// readiness builds the FR-D.4 /readyz gate: true only once every account
// with an IMAP engine has completed a sync pass and none is in
// authentication failure. A cache-only account (no [imap] block) has
// nothing to sync and does not hold readiness back.
func readiness(engines map[string]*sync.Engine, accounts []config.Account) func() bool {
	return func() bool {
		for i := range accounts {
			eng, ok := engines[accounts[i].ID]
			if !ok {
				continue
			}
			if !eng.Ready() || eng.AuthFailed() {
				return false
			}
		}
		return true
	}
}

// anyOAuth reports whether any account needs the OAuth2 machinery.
func anyOAuth(cfg *config.Config) bool {
	for i := range cfg.Accounts {
		if cfg.Accounts[i].OAuth2 != nil {
			return true
		}
	}
	return false
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

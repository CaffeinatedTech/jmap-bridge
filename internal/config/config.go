// Package config loads and strictly validates the bridge's TOML
// configuration (FR-A.1): unknown keys, wrong types, and missing required
// fields are startup errors that name the offending key. Secrets may come
// from the file, JMAP_BRIDGE_* environment variables, or mounted secret
// files (FR-A.2); their values never appear in error strings.
package config

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole bridge configuration, decoded from TOML.
type Config struct {
	Listen   string    `toml:"listen"`
	BaseURL  string    `toml:"base_url"`
	DataDir  string    `toml:"data_dir"`
	LogLevel string    `toml:"log_level"`
	Auth     Auth      `toml:"auth"`
	Search   Search    `toml:"search"`
	Sync     Sync      `toml:"sync"`
	Rate     Rate      `toml:"rate"`
	Metrics  Metrics   `toml:"metrics"`
	Accounts []Account `toml:"accounts"`
}

// Metrics configures the opt-in Prometheus exposition endpoint
// (FR-D.6). Disabled by default: metrics leak operational shape (account
// ids, queue depths), so an operator turns the endpoint on deliberately.
type Metrics struct {
	Enabled bool `toml:"enabled"`
}

// Rate configures the in-process abuse protection (NFR-5, SECURITY-PLAN
// A1/A2/A4). It is the credential-aware layer: the reverse proxy bounds
// raw volume before requests arrive, this bounds failed logins and
// concurrent work. Enabled by default; every zero field takes the
// documented default in defaults().
type Rate struct {
	Enabled bool `toml:"enabled"`

	// AuthFailures failed client-authentication attempts within
	// AuthWindow trip a lockout of AuthBlock for that source+account.
	AuthFailures int      `toml:"auth_failures"`
	AuthWindow   Duration `toml:"auth_window"`
	AuthBlock    Duration `toml:"auth_block"`

	// Advertised as maxConcurrentRequests / maxConcurrentUpload in the
	// session core capability and enforced here (golden rule 4).
	MaxConcurrentRequests int `toml:"max_concurrent_requests"`
	MaxConcurrentUploads  int `toml:"max_concurrent_uploads"`

	// EventSource connection caps.
	MaxEventsourcePerAccount int `toml:"max_eventsource_per_account"`
	MaxEventsourceTotal      int `toml:"max_eventsource_total"`

	// TrustedProxies are CIDRs whose forwarding headers are believed;
	// ClientIPHeader names the real-client header they set (e.g.
	// "CF-Connecting-IP"). Empty TrustedProxies means the connection
	// address is used, so per-source limits become effectively global.
	TrustedProxies []string `toml:"trusted_proxies"`
	ClientIPHeader string   `toml:"client_ip_header"`
}

// Auth configures client-facing authentication (FR-A.3, D-15).
type Auth struct {
	// Mode is "token" (HTTP Basic with the per-account token) or "none"
	// (accepted only on a loopback listener).
	Mode string `toml:"mode"`
}

// Search configures the search index (FR-X, decoded and validated from M0
// so the documented schema is accepted from day one; used from M5).
type Search struct {
	Backfill    bool `toml:"backfill"`
	Concurrency int  `toml:"concurrency"`
	// BackfillScan bounds how many unhydrated candidates one text
	// query may enqueue for background hydration (FR-X.5, FR-X.6);
	// 0 selects the documented default of 2000. Without a bound a
	// single search over a large unhydrated mailbox would become the
	// full mirror D-6 rejects.
	BackfillScan int `toml:"backfill_scan"`
}

// Sync configures the synchronisation engine (FR-S; decoded from M0,
// used from M1).
type Sync struct {
	Interval       Duration `toml:"interval"`
	BatchSize      int      `toml:"batch_size"`
	PrefetchWindow Duration `toml:"prefetch_window"`
}

// Account is one configured upstream mailbox served at /{id}/… (D-13).
type Account struct {
	ID      string `toml:"id"`
	Name    string `toml:"name"`
	Address string `toml:"address"`
	Token   string `toml:"token"`

	// TokenFile points at a mounted file holding the token (FR-A.2).
	TokenFile string `toml:"token_file"`
	// Backend selects the mail protocol: "imap" (default) or
	// "gmail_api" (D-API-1). It is mutually exclusive with the other's
	// backend block (FR-A.13).
	Backend  string    `toml:"backend"`
	IMAP     *IMAP     `toml:"imap"`
	SMTP     *SMTP     `toml:"smtp"`
	CardDAV  *CardDAV  `toml:"carddav"`
	OAuth2   *OAuth2   `toml:"oauth2"`
	GmailAPI *GmailAPI `toml:"gmail_api"`
}

// GmailAPI is the Gmail REST API backend block (D-API-1, GMAIL_API_PLAN
// §2). Its presence marks backend = "gmail_api"; an empty block is
// valid. Watch/PubSub settings are decoded now so the schema is stable
// from day one; M13 wires the push endpoint (M10 reads via history with
// the poll fallback).
type GmailAPI struct {
	// Watch is "pubsub" (Pub/Sub push, D-API-3) or "poll" (the
	// documented fallback). Normalised to a mode-appropriate default.
	Watch string `toml:"watch"`
	// PubSubTopic is the Cloud Pub/Sub topic users.watch registers.
	PubSubTopic string `toml:"pubsub_topic"`
	// PubSubAudience is the OIDC audience verified on push (M13).
	PubSubAudience string `toml:"pubsub_audience"`
	// PubSubServiceAccount is the `email` claim the verified push OIDC
	// token must carry — the service account the Pub/Sub push
	// subscription authenticates with (M13, FR-S.14).
	PubSubServiceAccount string `toml:"pubsub_service_account"`
	// PushAllowPlain accepts a shared-secret push for loopback rigs
	// instead of OIDC verification (M13).
	PushAllowPlain bool `toml:"push_allow_plain"`
	// QuotaUnitsPerSecond paces API calls; 0 selects the default
	// (D-API-7 / GMAIL_API_PLAN §4.3).
	QuotaUnitsPerSecond int `toml:"quota_units_per_second"`
	// BackfillQuery optionally scopes the initial header walk with Gmail
	// search syntax (e.g. "newer_than:30d"); "" walks everything.
	// BackfillLimit caps messages ingested per container (0 = no cap).
	// Both exist so a large mailbox can be served without a multi-hour
	// first pass (GMAIL_API_PLAN §14 cold-start risk).
	BackfillQuery string `toml:"backfill_query"`
	BackfillLimit int    `toml:"backfill_limit"`
	// Endpoint overrides the API base URL. It exists for the in-process
	// fixture gate and self-hosted mocks; production leaves it empty and
	// uses Google's endpoint. Token is a static bearer accepted only when
	// Endpoint names a loopback host, so a cloud account still goes
	// through OAuth2 (FR-A.13, §2.2).
	Endpoint  string `toml:"endpoint"`
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
}

// IMAP is the inbound backend block.
type IMAP struct {
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
	TLS          *bool  `toml:"tls"`
	Auth         string `toml:"auth"` // "password" (default) | "oauth2"
	Username     string `toml:"username"`
	Password     string `toml:"password"`
	PasswordFile string `toml:"password_file"`
}

// SMTP is the submission backend block.
type SMTP struct {
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
	TLS          string `toml:"tls"`  // "implicit" (default) | "starttls" | "none"
	Auth         string `toml:"auth"` // "password" (default) | "oauth2"
	Username     string `toml:"username"`
	Password     string `toml:"password"`
	PasswordFile string `toml:"password_file"`
}

// CardDAV is the contacts backend block; empty means contacts are
// disabled and the capability is not advertised (FR-P.3). URL is the
// configured endpoint (RFC 6764 discovery is the fallback when it names
// only the origin); Auth "password" (default) logs in with
// Username/Password, "oauth2" presents the account's OAuth2 access
// token as a Bearer credential (FR-A.9).
type CardDAV struct {
	URL string `toml:"url"`
	// Auth is "password" (default) or "oauth2".
	Auth     string `toml:"auth"`
	Username string `toml:"username"`
	Password string `toml:"password"`
	// PasswordFile points at a mounted file holding the password
	// (FR-A.2).
	PasswordFile string `toml:"password_file"`
}

// OAuth2 is the OAuth2 client configuration (FR-A.9).
type OAuth2 struct {
	Provider         string   `toml:"provider"` // "google" | "generic"
	ClientID         string   `toml:"client_id"`
	ClientSecret     string   `toml:"client_secret"`
	ClientSecretFile string   `toml:"client_secret_file"`
	AuthURL          string   `toml:"auth_url"`
	TokenURL         string   `toml:"token_url"`
	Scopes           []string `toml:"scopes"`
}

// defaults returns a Config pre-populated with documented defaults so
// that decoding only overwrites keys that are actually present.
func defaults() Config {
	return Config{
		LogLevel: "info",
		Auth:     Auth{Mode: "token"},
		Search:   Search{Backfill: true, Concurrency: 4, BackfillScan: 2000},
		Sync: Sync{
			Interval:       Duration(5 * time.Minute),
			BatchSize:      500,
			PrefetchWindow: Duration(30 * 24 * time.Hour),
		},
		Rate: Rate{
			Enabled:                  true,
			AuthFailures:             10,
			AuthWindow:               Duration(5 * time.Minute),
			AuthBlock:                Duration(15 * time.Minute),
			MaxConcurrentRequests:    8,
			MaxConcurrentUploads:     4,
			MaxEventsourcePerAccount: 8,
			MaxEventsourceTotal:      128,
		},
	}
}

// Load reads, decodes, and validates the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return LoadReader(f)
}

// LoadReader decodes and validates configuration from r; Load feeds it an
// opened file and tests feed it strings.
func LoadReader(r io.Reader) (*Config, error) {
	cfg := defaults()
	md, err := toml.NewDecoder(r).Decode(&cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, 0, len(undec))
		for _, k := range undec {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("config: unknown key(s): %s", strings.Join(keys, ", "))
	}
	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListenLoopback reports whether the configured listener binds only a
// loopback address; it is the gate for auth.mode = "none" (FR-A.11).
func (c *Config) ListenLoopback() bool { return loopbackListen(c.Listen) }

// Account returns the account with the given id.
func (c *Config) Account(id string) *Account {
	for i := range c.Accounts {
		if c.Accounts[i].ID == id {
			return &c.Accounts[i]
		}
	}
	return nil
}

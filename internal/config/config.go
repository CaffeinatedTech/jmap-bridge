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
	TokenFile string   `toml:"token_file"`
	IMAP      *IMAP    `toml:"imap"`
	SMTP      *SMTP    `toml:"smtp"`
	CardDAV   *CardDAV `toml:"carddav"`
	OAuth2    *OAuth2  `toml:"oauth2"`
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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalLoopback = `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"

[[accounts]]
id = "personal"
address = "me@example.com"
token = "t0ken-0123456789abcdef0123456789"
`

func load(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	return LoadReader(strings.NewReader(toml))
}

func mustLoad(t *testing.T, toml string) *Config {
	t.Helper()
	cfg, err := load(t, toml)
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	return cfg
}

func wantErrKey(t *testing.T, toml, key string) {
	t.Helper()
	_, err := load(t, toml)
	if err == nil {
		t.Fatalf("expected error for key %q, got none", key)
	}
	if !strings.Contains(err.Error(), "config key "+key+":") &&
		!strings.Contains(err.Error(), key) {
		t.Fatalf("error %q does not name key %q", err, key)
	}
}

func TestValidMinimal(t *testing.T) {
	cfg := mustLoad(t, minimalLoopback)
	if cfg.Auth.Mode != "token" {
		t.Errorf("auth.mode = %q, want token", cfg.Auth.Mode)
	}
	if !cfg.Search.Backfill {
		t.Error("search.backfill default = false, want true")
	}
	if cfg.Sync.Interval.Std() != 5*time.Minute {
		t.Errorf("sync.interval = %s, want 5m", cfg.Sync.Interval)
	}
	if cfg.Sync.PrefetchWindow.Std() != 30*24*time.Hour {
		t.Errorf("prefetch_window = %s, want 720h", cfg.Sync.PrefetchWindow)
	}
	if cfg.Accounts[0].Token != "t0ken-0123456789abcdef0123456789" {
		t.Errorf("token = %q", cfg.Accounts[0].Token)
	}
}

func TestUnknownKeyNamed(t *testing.T) {
	wantErrKey(t, minimalLoopback+"\nbogus = 1\n", "bogus")
	wantErrKey(t, minimalLoopback+"\n[search]\nnope = true\n", "search.nope")
}

func TestWrongTypeNamesKey(t *testing.T) {
	_, err := load(t, strings.Replace(minimalLoopback, `listen = "127.0.0.1:8080"`, `listen = 123`, 1))
	if err == nil {
		t.Fatal("expected a type error")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("type error %q does not name key listen", err)
	}
}

func TestMissingRequired(t *testing.T) {
	type missCase struct{ name, toml, key string }
	cases := []missCase{
		{"listen", strings.Replace(minimalLoopback, `listen = "127.0.0.1:8080"`, "", 1), "listen"},
		{"base_url", strings.Replace(minimalLoopback, `base_url = "http://127.0.0.1:8080"`, "", 1), "base_url"},
		{"data_dir", strings.Replace(minimalLoopback, `data_dir = "/data"`, "", 1), "data_dir"},
		{"token", strings.Replace(minimalLoopback, `token = "t0ken-0123456789abcdef0123456789"`, "", 1), "token"},
		{"accounts", `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"
`, "accounts"},
		{"account id", strings.Replace(minimalLoopback, `id = "personal"`, `id = ""`, 1), "id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wantErrKey(t, tc.toml, tc.key) })
	}
}

func TestBaseURLRules(t *testing.T) {
	type urlCase struct{ name, baseURL, key string }
	cases := []urlCase{
		{"relative", "/jmap", "base_url"},
		{"https", "https://example.com", ""},
		{"http loopback", "http://localhost:8080", ""},
		{"ftp", "ftp://example.com", "base_url"},
		{"path", "https://example.com/bridge", "base_url"},
		{"userinfo", "https://user@example.com", "base_url"},
		{"http non-loopback", "http://example.com", "base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := strings.Replace(minimalLoopback, `base_url = "http://127.0.0.1:8080"`, `base_url = "`+tc.baseURL+`"`, 1)
			_, err := load(t, toml)
			switch {
			case tc.key == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.key != "" && err == nil:
				t.Fatal("expected error, got none")
			case tc.key != "" && !strings.Contains(err.Error(), tc.key):
				t.Fatalf("error %q does not name %q", err, tc.key)
			}
		})
	}
}

func TestNoneRequiresLoopback(t *testing.T) {
	public := strings.Replace(minimalLoopback, `listen = "127.0.0.1:8080"`, `listen = "0.0.0.0:8080"`, 1)
	withNone := strings.Replace(public, "token = \"t0ken-0123456789abcdef0123456789\"", "token = \"t0ken-0123456789abcdef0123456789\"\n\n[auth]\nmode = \"none\"", 1)
	wantErrKey(t, withNone, "auth.mode")

	loopbackNone := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", "token = \"t0ken-0123456789abcdef0123456789\"\n\n[auth]\nmode = \"none\"", 1)
	cfg := mustLoad(t, loopbackNone)
	if cfg.Auth.Mode != "none" {
		t.Errorf("auth.mode = %q, want none", cfg.Auth.Mode)
	}
}

func TestDuplicateAccountID(t *testing.T) {
	toml := minimalLoopback + `
[[accounts]]
id = "personal"
token = "other-0123456789abcdef01234567"
`
	wantErrKey(t, toml, "id")
}

func TestEnvStemCollision(t *testing.T) {
	toml := minimalLoopback + `
[[accounts]]
id = "a_b"
token = "x-0123456789abcdef0123456789"
`
	toml = strings.Replace(toml, `id = "personal"`, `id = "a-b"`, 1)
	wantErrKey(t, toml, "id")
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("JMAP_BRIDGE_PERSONAL_TOKEN", "env-token-0123456789abcdef0123")
	toml := strings.Replace(minimalLoopback, `token = "t0ken-0123456789abcdef0123456789"`, "", 1)
	cfg := mustLoad(t, toml)
	if cfg.Accounts[0].Token != "env-token-0123456789abcdef0123" {
		t.Errorf("token = %q, want env-token-0123456789abcdef0123", cfg.Accounts[0].Token)
	}
}

func TestTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token-0123456789abcdef0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := strings.Replace(minimalLoopback, `token = "t0ken-0123456789abcdef0123456789"`, `token_file = "`+path+`"`, 1)
	cfg := mustLoad(t, toml)
	if cfg.Accounts[0].Token != "file-token-0123456789abcdef0123" {
		t.Errorf("token = %q, want file-token-0123456789abcdef0123", cfg.Accounts[0].Token)
	}
}

func TestTokenFileMissingNamesKey(t *testing.T) {
	toml := strings.Replace(minimalLoopback, `token = "t0ken-0123456789abcdef0123456789"`, `token_file = "/nonexistent/token"`, 1)
	_, err := load(t, toml)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "accounts[0].token_file") {
		t.Fatalf("error %q does not name accounts[0].token_file", err)
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	secret := "sup3r-s3cret-value"
	toml := strings.Replace(minimalLoopback, `token = "t0ken-0123456789abcdef0123456789"`, `token = "`+secret+`"`, 1)
	toml = strings.Replace(toml, `base_url = "http://127.0.0.1:8080"`, `base_url = "ftp://nope"`, 1)
	_, err := load(t, toml)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the token: %q", err)
	}
}

func TestIMAPValidation(t *testing.T) {
	withIMAP := func(body string) string {
		return strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", "token = \"t0ken-0123456789abcdef0123456789\"\n\n  [accounts.imap]\n  "+body, 1)
	}
	t.Run("missing host", func(t *testing.T) {
		wantErrKey(t, withIMAP(`port = 993`), "accounts[0].imap.host")
	})
	t.Run("missing port", func(t *testing.T) {
		wantErrKey(t, withIMAP(`host = "imap.example.com"`), "accounts[0].imap.port")
	})
	t.Run("bad auth enum", func(t *testing.T) {
		wantErrKey(t, withIMAP(`host = "h"
  port = 993
  auth = "magic"`), "accounts[0].imap.auth")
	})
	t.Run("password auth without password", func(t *testing.T) {
		wantErrKey(t, withIMAP(`host = "h"
  port = 993
  username = "u"`), "accounts[0].imap.password")
	})
	t.Run("valid password account", func(t *testing.T) {
		cfg := mustLoad(t, withIMAP(`host = "imap.example.com"
  port = 993
  username = "me@example.com"
  password = "pw"`))
		if cfg.Accounts[0].IMAP == nil || cfg.Accounts[0].IMAP.TLS == nil || !*cfg.Accounts[0].IMAP.TLS {
			t.Error("imap.tls should default to true")
		}
	})
	t.Run("oauth2 requires oauth2 block", func(t *testing.T) {
		wantErrKey(t, withIMAP(`host = "h"
  port = 993
  auth = "oauth2"`), "accounts[0].oauth2")
	})
}

func TestSMTPDefaultsFromIMAP(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.imap]
  host = "imap.example.com"
  port = 993
  username = "me@example.com"
  password = "pw"

  [accounts.smtp]
  host = "smtp.example.com"
  port = 465`, 1)
	cfg := mustLoad(t, toml)
	s := cfg.Accounts[0].SMTP
	if s.Username != "me@example.com" || s.Password != "pw" {
		t.Errorf("smtp credentials did not default from imap: user=%q", s.Username)
	}
	if s.TLS != "implicit" {
		t.Errorf("smtp.tls = %q, want implicit", s.TLS)
	}
}

func TestOAuth2GenericRequiresURLs(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.oauth2]
  provider = "generic"
  client_id = "cid"
  client_secret = "cs"`, 1)
	wantErrKey(t, toml, "accounts[0].oauth2.auth_url")
}

func TestBadLogLevel(t *testing.T) {
	toml := strings.Replace(minimalLoopback, `listen = "127.0.0.1:8080"`, "log_level = \"loud\"\nlisten = \"127.0.0.1:8080\"", 1)
	wantErrKey(t, toml, "log_level")
}

func TestDurationSyntax(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", "token = \"t0ken-0123456789abcdef0123456789\"\n\n[sync]\ninterval = \"30s\"\nprefetch_window = \"10d\"", 1)
	cfg := mustLoad(t, toml)
	if cfg.Sync.Interval.Std() != 30*time.Second {
		t.Errorf("interval = %s", cfg.Sync.Interval)
	}
	if cfg.Sync.PrefetchWindow.Std() != 240*time.Hour {
		t.Errorf("prefetch_window = %s", cfg.Sync.PrefetchWindow)
	}
}

// FR-D.6: the metrics endpoint is opt-in. It defaults off, decodes
// [metrics] enabled, and strict decoding still rejects an unknown key in
// the block.
func TestMetricsBlock(t *testing.T) {
	if cfg := mustLoad(t, minimalLoopback); cfg.Metrics.Enabled {
		t.Error("metrics.enabled default = true, want false")
	}
	cfg := mustLoad(t, minimalLoopback+"\n[metrics]\nenabled = true\n")
	if !cfg.Metrics.Enabled {
		t.Error("metrics.enabled = false, want true")
	}
	wantErrKey(t, minimalLoopback+"\n[metrics]\nfoo = true\n", "metrics.foo")
}

func TestBadDurationNamesKey(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", "token = \"t0ken-0123456789abcdef0123456789\"\n\n[sync]\ninterval = \"soon\"", 1)
	_, err := load(t, toml)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Fatalf("error %q does not name interval", err)
	}
}

// TestSMTPRequiresAddress pins FR-M.14/FR-M.15: an account that can send
// must be able to say who it is, because accounts.address is both the
// identity Identity/get answers with and the envelope sender.
func TestSMTPRequiresAddress(t *testing.T) {
	noAddress := strings.Replace(minimalLoopback, "address = \"me@example.com\"\n", "", 1)
	toml := noAddress + `
  [accounts.imap]
  host = "imap.example.com"
  port = 993
  username = "me@example.com"
  password = "pw"

  [accounts.smtp]
  host = "smtp.example.com"
  port = 465
`
	wantErrKey(t, toml, "accounts[0].address")

	// With the address present the same configuration loads.
	cfg := mustLoad(t, strings.Replace(toml, "id = \"personal\"", "id = \"personal\"\naddress = \"me@example.com\"", 1))
	if cfg.Accounts[0].Address == "" {
		t.Error("address was dropped")
	}
}

// FR-A.10: an oauth2 account has no password fallback. A password
// configured next to auth = "oauth2" is a startup error, and password
// defaults must not leak into an oauth2 SMTP block.
func TestOAuth2RefusesPasswordFallback(t *testing.T) {
	t.Run("imap password alongside oauth2", func(t *testing.T) {
		toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"
  username = "me@gmail.com"
  password = "legacy"`+"\n\n  [accounts.oauth2]\n  provider = \"google\"\n  client_id = \"cid\"\n  client_secret = \"cs\"", 1)
		wantErrKey(t, toml, "accounts[0].imap.password")
	})
	t.Run("smtp password alongside oauth2", func(t *testing.T) {
		toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"
  username = "me@gmail.com"

  [accounts.smtp]
  host = "smtp.gmail.com"
  port = 465
  auth = "oauth2"
  password = "legacy"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid"
  client_secret = "cs"`, 1)
		wantErrKey(t, toml, "accounts[0].smtp.password")
	})
	t.Run("smtp oauth2 does not inherit the imap password", func(t *testing.T) {
		toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"
  username = "me@gmail.com"

  [accounts.smtp]
  host = "smtp.gmail.com"
  port = 465
  auth = "oauth2"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid"
  client_secret = "cs"`, 1)
		cfg := mustLoad(t, toml)
		if cfg.Accounts[0].SMTP.Password != "" {
			t.Errorf("smtp.password inherited %q despite oauth2", cfg.Accounts[0].SMTP.Password)
		}
		if cfg.Accounts[0].SMTP.Username != "me@gmail.com" {
			t.Errorf("smtp.username = %q, want the imap username (XOAUTH2 carries it)", cfg.Accounts[0].SMTP.Username)
		}
	})
}

// FR-A.9: the google provider profile needs only the client pair.
func TestOAuth2GoogleMinimal(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = \"t0ken-0123456789abcdef0123456789\"", `token = "t0ken-0123456789abcdef0123456789"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"
  username = "me@gmail.com"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid"
  client_secret = "cs"`, 1)
	cfg := mustLoad(t, toml)
	if cfg.Accounts[0].OAuth2.Provider != "google" {
		t.Fatal("provider lost")
	}
}

func TestCardDAVBlock(t *testing.T) {
	// A valid block decodes and gets the documented defaults.
	dir := t.TempDir()
	pwFile := dir + "/davpass"
	if err := os.WriteFile(pwFile, []byte("file-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadReader(strings.NewReader(`
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/x"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-0123456789abcdef01234567"

  [accounts.imap]
  host = "127.0.0.1"
  port = 1143
  tls = false
  username = "u"
  password = "imap-pw"

  [accounts.carddav]
  url = "http://127.0.0.1:5230"
`))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Account("personal").CardDAV
	if d.Auth != "password" {
		t.Errorf("auth default = %q", d.Auth)
	}
	if d.Username != "u" {
		t.Errorf("username default = %q, want the imap login", d.Username)
	}
	if d.Password != "imap-pw" {
		t.Error("password did not default from the same-host imap block")
	}

	// Validation refusals name the key and never echo the secret.
	wantErrKey(t, withCardDAV(`url = ""`), "accounts[0].carddav.url")
	wantErrKey(t, withCardDAV(`url = "not-a-url"`), "accounts[0].carddav.url")
	// Without the IMAP block to default from, the login is required.
	wantErrKey(t, withCardDAVStandalone(`url = "https://carddav.example.net"`), "accounts[0].carddav.username")
	wantErrKey(t, withCardDAVStandalone(`
url = "https://carddav.example.net"
username = "u"`), "accounts[0].carddav.password")
	wantErrKey(t, withCardDAV(`
url = "https://carddav.example.net"
auth = "bearer"`), "accounts[0].carddav.auth")
	// With a real oauth2 block behind it, the leftover password is the
	// misconfiguration named (FR-A.10).
	wantErrKey(t, withCardDAV(`
url = "https://carddav.example.net"
auth = "oauth2"
password = "no-fallback"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid"
  client_secret = "secret"`), "accounts[0].carddav.password")
	wantErrKey(t, withCardDAV(`
url = "https://carddav.example.net"
auth = "oauth2"`), "accounts[0].oauth2")

	// password_file resolves through the standard precedence (FR-A.2).
	t.Setenv("JMAP_BRIDGE_PERSONAL_CARDDAV_PASSWORD_FILE", pwFile)
	cfg, err = LoadReader(strings.NewReader(`
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/x"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-0123456789abcdef01234567"

  [accounts.imap]
  host = "127.0.0.1"
  port = 1143
  tls = false
  username = "u"
  password = "imap-pw"

  [accounts.carddav]
  url = "http://127.0.0.1:5230"
  password = "inline-ignored"
`))
	if err != nil {
		t.Fatal(err)
	}
	if pw := cfg.Account("personal").CardDAV.Password; pw != "file-pass" {
		t.Errorf("env-file precedence failed (got %q)", pw)
	}
}

func withCardDAVStandalone(body string) string {
	return `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/x"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-0123456789abcdef01234567"

  [accounts.carddav]
` + body + "\n"
}

func withCardDAV(body string) string {
	return `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/tmp/x"

[[accounts]]
id = "personal"
address = "me@example.test"
token = "tok-0123456789abcdef01234567"

  [accounts.imap]
  host = "127.0.0.1"
  port = 1143
  tls = false
  username = "u"
  password = "imap-pw"

  [accounts.carddav]
` + body + "\n"
}

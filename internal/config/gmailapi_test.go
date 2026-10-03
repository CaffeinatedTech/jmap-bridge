package config

import (
	"strings"
	"testing"
)

const gapiBase = `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"

[[accounts]]
id = "gmail"
address = "me@gmail.com"
token = "t0ken-0123456789abcdef0123456789"
backend = "gmail_api"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid.apps.googleusercontent.com"
  client_secret = "shh"

  [accounts.gmail_api]
`

func TestBackendDefaultsToIMAP(t *testing.T) {
	cfg := mustLoad(t, minimalLoopback)
	if cfg.Accounts[0].Backend != "imap" {
		t.Fatalf("backend = %q, want imap", cfg.Accounts[0].Backend)
	}
}

func TestGmailAPIValid(t *testing.T) {
	cfg := mustLoad(t, gapiBase)
	a := cfg.Accounts[0]
	if a.Backend != "gmail_api" {
		t.Fatalf("backend = %q", a.Backend)
	}
	if a.GmailAPI.Watch != "poll" {
		t.Errorf("watch = %q, want poll default", a.GmailAPI.Watch)
	}
	if a.GmailAPI.QuotaUnitsPerSecond != 100 {
		t.Errorf("quota = %d, want 100 default", a.GmailAPI.QuotaUnitsPerSecond)
	}
}

func TestGmailAPIStaticTokenRequiresLoopbackEndpoint(t *testing.T) {
	toml := `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"

[[accounts]]
id = "gapi"
address = "fixture@example.test"
token = "t0ken-0123456789abcdef0123456789"
backend = "gmail_api"

  [accounts.gmail_api]
  endpoint = "http://127.0.0.1:9999/"
  token = "fixture-token"
`
	cfg := mustLoad(t, toml)
	if cfg.Accounts[0].GmailAPI.Endpoint != "http://127.0.0.1:9999/" {
		t.Fatalf("endpoint not retained")
	}
	// A cloud https endpoint without oauth2 and with no token is refused.
	bad := strings.Replace(toml, "  token = \"fixture-token\"\n", "", 1)
	bad = strings.Replace(bad, "http://127.0.0.1:9999/", "https://gmail.example.test/", 1)
	wantErrKey(t, bad, "accounts[0].oauth2")
}

func TestGmailAPIRejectsIMAPAndSMTP(t *testing.T) {
	imapBlock := `
listen = "127.0.0.1:8080"
base_url = "http://127.0.0.1:8080"
data_dir = "/data"

[[accounts]]
id = "gmail"
address = "me@gmail.com"
token = "t0ken-0123456789abcdef0123456789"
backend = "gmail_api"

  [accounts.imap]
  host = "imap.gmail.com"
  port = 993
  auth = "oauth2"
  username = "me@gmail.com"

  [accounts.oauth2]
  provider = "google"
  client_id = "cid"
  client_secret = "shh"

  [accounts.gmail_api]
`
	wantErrKey(t, imapBlock, "accounts[0].imap")

	smtpBlock := strings.Replace(imapBlock,
		"  [accounts.imap]\n  host = \"imap.gmail.com\"\n  port = 993\n  auth = \"oauth2\"\n  username = \"me@gmail.com\"\n",
		"  [accounts.smtp]\n  host = \"smtp.gmail.com\"\n  port = 465\n  auth = \"oauth2\"\n", 1)
	wantErrKey(t, smtpBlock, "accounts[0].smtp")
}

func TestGmailAPIRejectsBlockOnIMAPBackend(t *testing.T) {
	toml := minimalLoopback + `
  [accounts.gmail_api]
`
	wantErrKey(t, toml, "accounts[0].gmail_api")
}

func TestGmailAPIRejectsUnknownBackend(t *testing.T) {
	toml := strings.Replace(minimalLoopback, "token = ", "backend = \"pop3\"\ntoken = ", 1)
	wantErrKey(t, toml, "accounts[0].backend")
}

func TestGmailAPIPubSubRequiresTopic(t *testing.T) {
	toml := gapiBase + `  watch = "pubsub"
`
	wantErrKey(t, toml, "accounts[0].gmail_api.pubsub_topic")
}

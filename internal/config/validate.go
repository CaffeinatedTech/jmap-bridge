package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validate normalises optional fields to their documented defaults and
// then checks every required field (FR-A.1), failing with an error that
// names the offending key. Error strings never carry secret values
// (FR-A.2).
func (c *Config) validate() error {
	c.normalize()

	if c.Listen == "" {
		return errKey("listen", "required")
	}
	if err := validateListen(c.Listen); err != nil {
		return errKey("listen", "%s", err)
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return errKey("base_url", "%s", err)
	}
	if c.DataDir == "" {
		return errKey("data_dir", "required")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errKey("log_level", `must be one of "debug", "info", "warn", "error"`)
	}
	switch c.Auth.Mode {
	case "token":
	case "none":
		if !c.ListenLoopback() {
			return errKey("auth.mode", `"none" is allowed only when listen binds a loopback address`)
		}
	default:
		return errKey("auth.mode", `must be "token" or "none"`)
	}
	if c.Search.Concurrency < 1 {
		return errKey("search.concurrency", "must be at least 1")
	}
	if c.Sync.BatchSize < 1 {
		return errKey("sync.batch_size", "must be at least 1")
	}
	if c.Sync.Interval.Std() <= 0 {
		return errKey("sync.interval", "must be positive")
	}
	if c.Sync.PrefetchWindow.Std() < 0 {
		return errKey("sync.prefetch_window", "must not be negative")
	}
	if len(c.Accounts) == 0 {
		return errKey("accounts", "at least one account is required")
	}

	seenID := map[string]int{}
	seenEnv := map[string]int{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if err := c.validateAccount(i, a); err != nil {
			return err
		}
		if prev, dup := seenID[a.ID]; dup {
			return errKey(fmt.Sprintf("accounts[%d].id", i), "duplicate account id %q (also accounts[%d])", a.ID, prev)
		}
		seenID[a.ID] = i
		stem := envStem(a.ID)
		if prev, dup := seenEnv[stem]; dup {
			return errKey(fmt.Sprintf("accounts[%d].id", i),
				`environment stem %q collides with accounts[%d] (%q); JMAP_BRIDGE_* names would be ambiguous`,
				stem, prev, c.Accounts[prev].ID)
		}
		seenEnv[stem] = i
	}
	return nil
}

// normalize fills documented defaults that depend on other keys.
func (c *Config) normalize() {
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.IMAP != nil {
			if a.IMAP.Auth == "" {
				a.IMAP.Auth = "password"
			}
			if a.IMAP.TLS == nil {
				t := true
				a.IMAP.TLS = &t
			}
			if a.IMAP.Username == "" {
				a.IMAP.Username = a.Address
			}
		}
		if a.SMTP != nil {
			if a.SMTP.Auth == "" {
				a.SMTP.Auth = "password"
			}
			if a.SMTP.TLS == "" {
				a.SMTP.TLS = "implicit"
			}
			if a.SMTP.Username == "" && a.IMAP != nil {
				a.SMTP.Username = a.IMAP.Username
			}
			if a.SMTP.Password == "" && a.IMAP != nil {
				a.SMTP.Password = a.IMAP.Password
			}
		}
	}
}

func (c *Config) validateAccount(i int, a *Account) error {
	if a.ID == "" {
		return errKey(fmt.Sprintf("accounts[%d].id", i), "required")
	}
	if !accountIDPattern.MatchString(a.ID) {
		return errKey(fmt.Sprintf("accounts[%d].id", i),
			"%q must match [a-z0-9][a-z0-9_-]* (it is the URL path prefix and the JMAP_BRIDGE_* stem)", a.ID)
	}
	if c.Auth.Mode == "token" && a.Token == "" {
		return errKey(fmt.Sprintf("accounts[%d].token", i),
			`required when auth.mode = "token" (config, JMAP_BRIDGE_%s_TOKEN, or token_file)`, envStem(a.ID))
	}
	if a.IMAP != nil {
		if err := validateIMAP(fmt.Sprintf("accounts[%d].imap", i), a.IMAP); err != nil {
			return err
		}
	}
	if a.SMTP != nil {
		if err := validateSMTP(fmt.Sprintf("accounts[%d].smtp", i), a.SMTP); err != nil {
			return err
		}
	}
	if a.CardDAV != nil && a.CardDAV.URL != "" {
		if err := validateAbsoluteURL(a.CardDAV.URL, "carddav.url must be an absolute URL"); err != nil {
			return errKey(fmt.Sprintf("accounts[%d].carddav.url", i), "%s", err)
		}
	}
	if a.OAuth2 != nil {
		if err := validateOAuth2(fmt.Sprintf("accounts[%d].oauth2", i), a.OAuth2); err != nil {
			return err
		}
	}
	if a.OAuth2 == nil {
		backendAuth := ""
		if a.IMAP != nil && a.IMAP.Auth == "oauth2" {
			backendAuth = "imap.auth"
		}
		if a.SMTP != nil && a.SMTP.Auth == "oauth2" {
			backendAuth = "smtp.auth"
		}
		if backendAuth != "" {
			return errKey(fmt.Sprintf("accounts[%d].oauth2", i), "required when %s = \"oauth2\"", backendAuth)
		}
	}
	return nil
}

func validateIMAP(key string, m *IMAP) error {
	if m.Host == "" {
		return errKey(key+".host", "required")
	}
	if err := validatePort(m.Port); err != nil {
		return errKey(key+".port", "%s", err)
	}
	// The username is the login identity for both auth modes (XOAUTH2
	// carries it too); it defaults to accounts.address in normalize.
	if m.Username == "" {
		return errKey(key+".username", "required (or set accounts.address to default it)")
	}
	switch m.Auth {
	case "password":
		if m.Password == "" {
			return errKey(key+".password", "required when imap.auth = \"password\"")
		}
	case "oauth2":
		// FR-A.10: an oauth2 account never falls back to a password.
	default:
		return errKey(key+".auth", `must be "password" or "oauth2"`)
	}
	return nil
}

func validateSMTP(key string, s *SMTP) error {
	if s.Host == "" {
		return errKey(key+".host", "required")
	}
	if err := validatePort(s.Port); err != nil {
		return errKey(key+".port", "%s", err)
	}
	switch s.TLS {
	case "implicit", "starttls", "none":
	default:
		return errKey(key+".tls", `must be "implicit", "starttls", or "none"`)
	}
	switch s.Auth {
	case "password":
		if s.Username == "" {
			return errKey(key+".username", "required when smtp.auth = \"password\"")
		}
		if s.Password == "" {
			return errKey(key+".password", "required when smtp.auth = \"password\"")
		}
	case "oauth2":
	default:
		return errKey(key+".auth", `must be "password" or "oauth2"`)
	}
	return nil
}

func validateOAuth2(key string, o *OAuth2) error {
	switch o.Provider {
	case "google":
		if o.ClientID == "" {
			return errKey(key+".client_id", "required when provider = \"google\"")
		}
		if o.ClientSecret == "" {
			return errKey(key+".client_secret", "required when provider = \"google\"")
		}
	case "generic":
		if o.ClientID == "" {
			return errKey(key+".client_id", "required when provider = \"generic\"")
		}
		if o.ClientSecret == "" {
			return errKey(key+".client_secret", "required when provider = \"generic\"")
		}
		if err := validateAbsoluteURL(o.AuthURL, "auth_url must be an absolute URL"); err != nil {
			return errKey(key+".auth_url", "%s", err)
		}
		if err := validateAbsoluteURL(o.TokenURL, "token_url must be an absolute URL"); err != nil {
			return errKey(key+".token_url", "%s", err)
		}
		if len(o.Scopes) == 0 {
			return errKey(key+".scopes", "at least one scope is required when provider = \"generic\"")
		}
	default:
		return errKey(key+".provider", `must be "google" or "generic"`)
	}
	return nil
}

func validateListen(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("must be host:port")
	}
	return validatePortString(port)
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be an absolute URL: %v", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be an absolute URL (scheme and host)")
	}
	switch u.Scheme {
	case "http":
		if !loopbackHost(u.Hostname()) {
			return fmt.Errorf("http:// is allowed only for loopback hosts")
		}
	case "https":
	default:
		return fmt.Errorf("scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not contain a path (serve from the origin root)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must not contain a query or fragment")
	}
	return nil
}

func validateAbsoluteURL(raw, msg string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s", msg)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("%s (http:// only for loopback)", msg)
	}
	return nil
}

func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("must be between 1 and 65535")
	}
	return nil
}

func validatePortString(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("port must be numeric")
	}
	return validatePort(n)
}

func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func loopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	return loopbackHost(host)
}

func errKey(key, format string, args ...any) error {
	return fmt.Errorf("config key %s: %s", key, fmt.Sprintf(format, args...))
}

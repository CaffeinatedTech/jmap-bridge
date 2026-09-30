package config

import (
	"fmt"
	"os"
	"strings"
)

// envStem maps an account id to its environment-variable stem: ids are
// restricted to [a-z0-9_-], so uppercasing and swapping "-" for "_" is
// injective only if the id set stays collision-free — validate() enforces
// that (FR-A.11).
func envStem(accountID string) string {
	return strings.ToUpper(strings.ReplaceAll(accountID, "-", "_"))
}

// envName builds JMAP_BRIDGE_<ACCOUNT>_<FIELD…> (FR-A.2). field is a
// dotted key path relative to the account, e.g. "imap.password".
func envName(accountID, field string) string {
	return "JMAP_BRIDGE_" + envStem(accountID) + "_" +
		strings.ToUpper(strings.ReplaceAll(field, ".", "_"))
}

// resolveSecret picks a secret value with precedence file > env > config
// and reports failures as config errors naming the key, never the value
// (FR-A.2). fileVal is the inline `<key>_file` setting; key is the config
// key path used in errors (e.g. "accounts[0].token").
func resolveSecret(accountID, field, key, inline, fileVal string) (string, error) {
	path := fileVal
	if path == "" {
		path = os.Getenv(envName(accountID, field) + "_FILE")
	}
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied config
		if err != nil {
			return "", fmt.Errorf("config key %s_file: %w", key, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("config key %s_file: %s is empty", key, path)
		}
		return v, nil
	}
	if v, ok := os.LookupEnv(envName(accountID, field)); ok {
		return v, nil
	}
	return inline, nil
}

// resolveSecrets fills every secret slot on every account after decode
// and before validation, so validation sees the effective values.
func (c *Config) resolveSecrets() error {
	for i := range c.Accounts {
		a := &c.Accounts[i]
		tok, err := resolveSecret(a.ID, "token", fmt.Sprintf("accounts[%d].token", i), a.Token, a.TokenFile)
		if err != nil {
			return err
		}
		a.Token = tok
		if a.IMAP != nil {
			pw, err := resolveSecret(a.ID, "imap.password", fmt.Sprintf("accounts[%d].imap.password", i), a.IMAP.Password, a.IMAP.PasswordFile)
			if err != nil {
				return err
			}
			a.IMAP.Password = pw
		}
		if a.SMTP != nil {
			pw, err := resolveSecret(a.ID, "smtp.password", fmt.Sprintf("accounts[%d].smtp.password", i), a.SMTP.Password, a.SMTP.PasswordFile)
			if err != nil {
				return err
			}
			a.SMTP.Password = pw
		}
		if a.CardDAV != nil {
			pw, err := resolveSecret(a.ID, "carddav.password", fmt.Sprintf("accounts[%d].carddav.password", i), a.CardDAV.Password, a.CardDAV.PasswordFile)
			if err != nil {
				return err
			}
			a.CardDAV.Password = pw
		}
		if a.OAuth2 != nil {
			sec, err := resolveSecret(a.ID, "oauth2.client_secret", fmt.Sprintf("accounts[%d].oauth2.client_secret", i), a.OAuth2.ClientSecret, a.OAuth2.ClientSecretFile)
			if err != nil {
				return err
			}
			a.OAuth2.ClientSecret = sec
		}
	}
	return nil
}

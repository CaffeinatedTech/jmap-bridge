// Command gmailtoken is a live-gate helper: it unseals the account's
// stored OAuth2 refresh token, mints a fresh access token, and writes it
// to a 0600 file for an independent IMAP probe (the live tests' second
// client). It prints nothing secret and is never run by the bridge.
//
//	gmailtoken <data-dir> <key-file> <out-file>   (env: client id/secret)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/CaffeinatedTech/jmap-bridge/internal/auth"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: gmailtoken <data-dir> <key-file> <out-file>")
		os.Exit(2)
	}
	keyRaw, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cipher, err := auth.NewCipher(strings.TrimSpace(string(keyRaw)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := store.Open(context.Background(), store.Options{DataDir: os.Args[1]})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()
	// The gate account; a second argument may name another.
	account := "gmail"
	if env := os.Getenv("GMAIL_TOKEN_ACCOUNT"); env != "" {
		account = env
	}
	tok, err := st.OAuthToken(context.Background(), account)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	refresh, err := cipher.Open(tok.RefreshToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	clientID := os.Getenv("GMAIL_TOKEN_CLIENT_ID")
	clientSecret := os.Getenv("JMAP_BRIDGE_GMAIL_OAUTH2_CLIENT_SECRET")
	tokenURL := os.Getenv("GMAIL_TOKEN_URL")
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		// The error response carries only an OAuth error code/description,
		// never a token; surface it so a dead consent is diagnosable.
		fmt.Fprintf(os.Stderr, "token endpoint: %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	s := string(body)
	i := strings.Index(s, `"access_token"`)
	if i < 0 {
		fmt.Fprintln(os.Stderr, "no access_token in response")
		os.Exit(1)
	}
	rest := s[i+len(`"access_token"`):]
	if c := strings.IndexByte(rest, ':'); c >= 0 {
		rest = rest[c+1:]
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		fmt.Fprintln(os.Stderr, "malformed access_token value")
		os.Exit(1)
	}
	rest = rest[1:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		fmt.Fprintln(os.Stderr, "malformed response")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[3], []byte(rest[:j]), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

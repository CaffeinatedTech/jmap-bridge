// Package submit relays one message to an SMTP submission server
// (PLAN §7.2, RFC 5321/5322/6409/4954). It is deliberately small: one
// connection, one message, no queue and no retry — the bridge submits
// inline so EmailSubmission/set can answer with the server's own reply
// (FR-M.15), and a client that wants to try again is the client that
// decides it.
package submit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Config is one account's submission server, straight from the
// [accounts.smtp] block.
type Config struct {
	Host     string
	Port     int
	TLS      string // "implicit" (default) | "starttls" | "none"
	Auth     string // "password" (default) | "oauth2"
	Username string
	Password string
}

// Envelope is the SMTP envelope of one submission: who the message is
// from and who it goes to (RFC 5321 §4.1.2).
type Envelope struct {
	From       string
	Recipients []string
}

// ErrAuthUnsupported is an auth mode this build cannot speak yet:
// XOAUTH2 (Gmail's only option) arrives with M4 (PLAN §12).
var ErrAuthUnsupported = errors.New("submit: this auth mode is not implemented yet (XOAUTH2 ships with M4)")

// RejectedError is a refusal from the submission server: the message was
// not accepted. Reply is the server's own words, which is what
// EmailSubmission/set puts in the SetError description (PLAN §7.2).
type RejectedError struct {
	Reply string
}

func (e *RejectedError) Error() string { return "smtp: " + e.Reply }

// Send relays msg — a complete RFC 5322 message — to cfg's server for
// env. ctx cancels the connection, so a client that walks away never
// leaves a socket chewing through a large message.
func Send(ctx context.Context, cfg Config, env Envelope, msg []byte) error {
	if cfg.Auth == "oauth2" {
		return ErrAuthUnsupported
	}
	if env.From == "" {
		return errors.New("submit: envelope sender is empty")
	}
	if len(env.Recipients) == 0 {
		return errors.New("submit: envelope has no recipients")
	}

	conn, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// The request's context cannot interrupt a blocking SMTP read on its
	// own, so it closes the socket instead.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return wrapReply(err, "submit: greeting")
	}
	defer func() { _ = c.Close() }()

	if cfg.TLS == "starttls" {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return errors.New("submit: server does not offer STARTTLS (tls = \"starttls\" refuses to downgrade)")
		}
		if err := c.StartTLS(tlsConfig(cfg.Host)); err != nil {
			return wrapReply(err, "submit: starttls")
		}
	}
	if cfg.Username != "" || cfg.Password != "" {
		if err := authorize(c, cfg); err != nil {
			return err
		}
	}
	if err := c.Mail(env.From); err != nil {
		return wrapReply(err, "submit: MAIL FROM")
	}
	for _, rcpt := range env.Recipients {
		if err := c.Rcpt(rcpt); err != nil {
			return wrapReply(err, "submit: RCPT TO")
		}
	}
	w, err := c.Data()
	if err != nil {
		return wrapReply(err, "submit: DATA")
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("submit: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return wrapReply(err, "submit: end of data")
	}
	if err := c.Quit(); err != nil {
		// The message is already accepted; a failed QUIT must not make
		// the caller believe it was not.
		return nil
	}
	return nil
}

// dial opens the transport: implicit TLS, or a plain socket for
// starttls/none.
func dial(ctx context.Context, cfg Config) (net.Conn, error) {
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	d := &net.Dialer{Timeout: 30 * time.Second}
	if cfg.TLS == "" || cfg.TLS == "implicit" {
		td := &tls.Dialer{NetDialer: d, Config: tlsConfig(cfg.Host)}
		conn, err := td.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("submit: connect to %s: %w", addr, err)
		}
		return conn, nil
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("submit: connect to %s: %w", addr, err)
	}
	return conn, nil
}

func tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// authorize sends AUTH PLAIN when the server offers it. A server that
// advertises no usable mechanism fails with a sentence rather than
// letting the MAIL FROM reject look like a recipient problem.
func authorize(c *smtp.Client, cfg Config) error {
	ok, mechanisms := c.Extension("AUTH")
	if !ok || !advertises(mechanisms, "PLAIN") {
		return errors.New("submit: server does not offer AUTH PLAIN")
	}
	if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
		return wrapReply(err, "submit: AUTH")
	}
	return nil
}

// advertises reports whether a space-separated EHLO parameter list
// names mech.
func advertises(params, mech string) bool {
	for _, m := range strings.Fields(params) {
		if strings.EqualFold(m, mech) {
			return true
		}
	}
	return false
}

// wrapReply turns a server refusal into a RejectedError carrying its
// reply, and leaves everything else (transport trouble, our own
// mistakes) as a plain error — EmailSubmission/set only quotes the
// server's words for a refusal (PLAN §7.2).
func wrapReply(err error, op string) error {
	if err == nil {
		return nil
	}
	// Every non-matching SMTP reply reaches us as *textproto.Error.
	var textErr *textproto.Error
	if errors.As(err, &textErr) {
		return &RejectedError{Reply: replyText(textErr.Code, textErr.Msg)}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func replyText(code int, msg string) string {
	return strings.TrimSpace(strconv.Itoa(code) + " " + msg)
}

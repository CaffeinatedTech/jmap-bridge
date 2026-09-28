// Package imapdrv is the bridge's IMAP driver: kiliant/go-imap behind
// our own interface (D-7, amended 2026-09-29). Library types never
// leave this package — the sync engine sees only driver types and the
// neutral convert types they are mapped into — so swapping or patching
// the library cannot ripple past this seam.
//
// The driver is deliberately dumb: it dials, discovers, selects, fetches
// and idles. Tier policy (FR-S.2), backfill cursors (FR-S.3), anchors
// and tombstones (FR-S.5) all live in internal/sync, which is where the
// interesting tests belong.
package imapdrv

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// Tier is the sync strategy the session can use (FR-S.2). Highest
// supported wins.
type Tier int

const (
	// TierQResync is QRESYNC + CONDSTORE (RFC 7162 full resync).
	TierQResync Tier = iota
	// TierCondStore is CONDSTORE only (Gmail-shaped servers).
	TierCondStore
	// TierBaseline diffs UID lists and flags by hand.
	TierBaseline
)

// String names the tier for logs and metrics (FR-S.1, FR-S.2).
func (t Tier) String() string {
	switch t {
	case TierQResync:
		return "qresync"
	case TierCondStore:
		return "condstore"
	default:
		return "baseline"
	}
}

// Config is one account's IMAP endpoint.
type Config struct {
	Host     string
	Port     int
	TLS      bool
	Username string
	Password string
	Logger   *slog.Logger
}

// Conn is one authenticated IMAP session. It is not safe for
// concurrent command use; the sync engine serialises its calls (PLAN §3
// process model).
type Conn struct {
	client *imapclient.Client
	cfg    Config
	log    *slog.Logger
	tier   Tier
	// notes coalesces unilateral change notifications (Exists/Expunge/
	// Fetch/Vanished) into a single "look again" signal, cap 1 so a busy
	// folder cannot outpace the consumer.
	notes chan struct{}
	// selected is the folder this connection last selected, "" when none.
	selected string
	// idle is the running IDLE session, if any. The sync engine
	// serialises all use of a connection, so no lock guards these.
	idle *IdleSession
}

// Dial connects, authenticates, negotiates COMPRESS/UTF8/QRESYNC, and
// records the session's tier (FR-S.1, FR-S.2). A capability the server
// advertises but refuses to enable downgrades the tier immediately —
// honest capability handling beats a broken session later.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	c := &Conn{cfg: cfg, log: log, notes: make(chan struct{}, 1)}
	opts := &imapclient.Options{
		AllowInsecureAuth: !cfg.TLS,
		UnilateralData: &imapclient.UnilateralDataHandler{
			Exists:   func(uint32) { c.note() },
			Expunge:  func(uint32) { c.note() },
			Fetch:    func(*imap.FetchMessageData) { c.note() },
			Vanished: func(imapclient.VanishedData) { c.note() },
		},
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var err error
	if cfg.TLS {
		c.client, err = imapclient.DialTLS(ctx, addr, opts)
	} else {
		c.client, err = imapclient.Dial(ctx, addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imapdrv: dial %s: %w", addr, err)
	}
	if err := c.client.Login(ctx, cfg.Username, cfg.Password, nil); err != nil {
		_ = c.client.Close()
		return nil, fmt.Errorf("imapdrv: login: %w", err)
	}

	// FR-S.1: COMPRESS where offered, before any bulk transfer.
	if c.caps()["COMPRESS=DEFLATE"] {
		if err := c.client.Compress(ctx, nil); err != nil {
			log.Warn("imapdrv: COMPRESS declined", "err", err)
		}
	}

	c.tier = tierFrom(c.caps())
	// FR-S.1: ENABLE what we will use; a refused extension demotes the
	// tier (FR-S.2 downgrade without restart).
	var enable []string
	switch c.tier {
	case TierQResync:
		enable = append(enable, "CONDSTORE", "QRESYNC")
	case TierCondStore:
		enable = append(enable, "CONDSTORE")
	}
	if c.caps()["UTF8=ACCEPT"] {
		enable = append(enable, "UTF8=ACCEPT")
	}
	if len(enable) > 0 {
		if _, err := c.client.Enable(nil, enable...).Wait(ctx); err != nil {
			log.Warn("imapdrv: ENABLE partially refused", "err", err, "wanted", enable)
			c.reassessTier()
		}
	}
	enabled := c.client.EnabledCapabilities()
	if c.tier == TierQResync && !enabled["QRESYNC"] {
		c.tier = TierCondStore
	}
	if c.tier == TierCondStore && !enabled["CONDSTORE"] {
		// CONDSTORE may still activate via SELECT (CONDSTORE); only drop
		// to baseline when the capability itself is gone.
		if !c.caps()["CONDSTORE"] {
			c.tier = TierBaseline
		}
	}
	if c.tier == TierQResync && !enabled["CONDSTORE"] && !c.caps()["CONDSTORE"] {
		c.tier = TierBaseline
	}
	log.Info("imapdrv: session up",
		"server", addr, "tier", c.tier.String(),
		"compressed", c.client.Compressed(),
		"caps", capList(c.caps()))
	return c, nil
}

// Close logs out and drops the connection.
func (c *Conn) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Tier reports the session's current tier (FR-S.2 recording).
func (c *Conn) Tier() Tier { return c.tier }

// Compressed reports whether COMPRESS=DEFLATE is active (FR-S.11
// metrics read this).
func (c *Conn) Compressed() bool { return c.client.Compressed() }

// SupportsPREVIEW reports RFC 8970 availability (PLAN §5 preview
// strategy).
func (c *Conn) SupportsPREVIEW() bool { return c.caps()["PREVIEW"] }

func (c *Conn) caps() map[string]bool { return c.client.Capabilities() }

// reassessTier re-reads capabilities after a refused enable.
func (c *Conn) reassessTier() {
	if t := tierFrom(c.caps()); t < c.tier {
		c.tier = t
	}
}

// tierFrom picks the highest tier the server advertises (FR-S.2).
func tierFrom(caps map[string]bool) Tier {
	switch {
	case caps["QRESYNC"]:
		return TierQResync
	case caps["CONDSTORE"]:
		return TierCondStore
	default:
		return TierBaseline
	}
}

// note coalesces a change notification (never blocks: cap-1 channel).
func (c *Conn) note() {
	select {
	case c.notes <- struct{}{}:
	default:
	}
}

// Notes is the coalesced "selected folder changed" signal the idle
// loop consumes (FR-S.7).
func (c *Conn) Notes() <-chan struct{} { return c.notes }

// drainNotes empties the signal after a pass consumed it.
func (c *Conn) drainNotes() {
	select {
	case <-c.notes:
	default:
	}
}

func capList(caps map[string]bool) []string {
	out := make([]string, 0, len(caps))
	for k := range caps {
		out = append(out, k)
	}
	return out
}

// downgradeAfterError re-checks the tier when a tier-specific command
// is rejected mid-session (server lost an extension after re-auth,
// FR-S.2) and reports whether the caller should retry at the new tier.
func (c *Conn) downgradeAfterError(err error, from Tier) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch from {
	case TierQResync:
		if strings.Contains(msg, "QRESYNC") || strings.Contains(msg, "BAD") {
			c.tier = TierCondStore
			c.log.Warn("imapdrv: QRESYNC rejected, downgrading", "to", c.tier.String())
			return true
		}
	case TierCondStore:
		if strings.Contains(msg, "CONDSTORE") || strings.Contains(msg, "CHANGEDSINCE") || strings.Contains(msg, "BAD") {
			c.tier = TierBaseline
			c.log.Warn("imapdrv: CONDSTORE rejected, downgrading", "to", c.tier.String())
			return true
		}
	}
	return false
}

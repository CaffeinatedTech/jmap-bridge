// Package fixtureimap is the in-process IMAP server the sync and driver
// tests run against (AGENTS.md testing rules). It wraps kiliant's
// imapserver over its memory backend — the wire code is the same
// library's, so tests exercise a real protocol implementation — and
// impersonates the three sync tiers by withholding QRESYNC/CONDSTORE
// from CAPABILITY exactly as a smaller server would (PLAN §5, FR-S.2).
//
// Mutations happen through a second protocol session ("the other IMAP
// client"), so foreign-change tests behave like the live gate: one
// connection notices another's work over the wire, never through test
// hooks.
package fixtureimap

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Tier selects which sync capabilities the fixture advertises.
type Tier int

const (
	// TierQResync advertises QRESYNC and CONDSTORE (Dovecot-shaped).
	TierQResync Tier = iota
	// TierCondStore advertises CONDSTORE only (Gmail-shaped).
	TierCondStore
	// TierBare advertises neither; the driver must fall back to the
	// baseline UID-diff loop.
	TierBare
)

// Options configures a fixture server.
type Options struct {
	Tier  Tier
	Users map[string]string // username → password (non-empty)
}

// Server is a running fixture IMAP server with an admin session.
type Server struct {
	t      testing.TB
	addr   string
	cancel context.CancelFunc
	ln     net.Listener

	adminMu sync.Mutex
	admin   *imapclient.Client
	user    string
}

// Start brings a fixture server up on a loopback ephemeral port, opens
// the admin (foreign) session, and stops everything on test cleanup.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	if len(opts.Users) == 0 {
		opts.Users = map[string]string{"test": "test-pass"}
	}
	backend := &tierBackend{Backend: memory.New(&memory.Options{Users: opts.Users}), tier: opts.Tier}
	srv := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fixtureimap: listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, ln, nil) }()

	var user, pass string
	for u, p := range opts.Users {
		user, pass = u, p
		break
	}
	admin, err := imapclient.Dial(ctx, ln.Addr().String(), &imapclient.Options{AllowInsecureAuth: true})
	if err != nil {
		cancel()
		t.Fatalf("fixtureimap: admin dial: %v", err)
	}
	if err := admin.Login(ctx, user, pass, nil); err != nil {
		cancel()
		t.Fatalf("fixtureimap: admin login: %v", err)
	}

	s := &Server{t: t, addr: ln.Addr().String(), cancel: cancel, ln: ln, admin: admin, user: user}
	t.Cleanup(s.Close)
	return s
}

// Close tears down the admin session and the listener.
func (s *Server) Close() {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if s.admin != nil {
		ctx, cancel := context.WithCancel(context.Background())
		_ = s.admin.Logout(ctx, nil)
		cancel()
		_ = s.admin.Close()
		s.admin = nil
	}
	s.cancel()
	_ = s.ln.Close()
}

// Addr returns the loopback address to dial.
func (s *Server) Addr() string { return s.addr }

// withAdmin runs fn against the admin session under the lock.
func (s *Server) withAdmin(fn func(ctx context.Context, c *imapclient.Client) error) {
	s.t.Helper()
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fn(ctx, s.admin); err != nil {
		s.t.Fatalf("fixtureimap: admin op: %v", err)
	}
}

// tierBackend withholds capabilities per tier; everything else comes
// from the memory backend unchanged.
type tierBackend struct {
	*memory.Backend
	tier Tier
}

var _ imapserver.CapabilitySupport = (*tierBackend)(nil)

// SupportsCapability implements imapserver.CapabilitySupport.
func (b *tierBackend) SupportsCapability(name string) bool {
	switch strings.ToUpper(name) {
	case "QRESYNC":
		return b.tier == TierQResync
	case "CONDSTORE":
		return b.tier != TierBare
	default:
		return b.Backend.SupportsCapability(name)
	}
}

// Append stores raw message bytes in a folder as the admin client and
// returns the assigned UID and the folder's UIDVALIDITY (FR-S.3
// backfill seeding).
func (s *Server) Append(folder string, raw []byte, flags ...imap.Flag) (uid, uidValidity uint32) {
	s.t.Helper()
	s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
		data, err := c.Append(ctx, folder, &imapclient.AppendOptions{Flags: flags},
			int64(len(raw)), strings.NewReader(string(raw))).Wait(ctx)
		if err != nil {
			return err
		}
		uid, uidValidity = uint32(data.UID), data.UIDValidity
		return nil
	})
	return uid, uidValidity
}

// SetFlag flips one flag on one message — the "other IMAP client"
// moving \Seen under the bridge's feet (FR-S.7's live gate, scripted).
func (s *Server) SetFlag(folder string, uid uint32, flag imap.Flag, on bool) {
	s.t.Helper()
	s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
		if _, err := c.Select(folder, nil).Wait(ctx); err != nil {
			return err
		}
		op := imapclient.StoreFlagsAdd
		if !on {
			op = imapclient.StoreFlagsRemove
		}
		return c.StoreUID(imap.UIDSetNum(imap.UID(uid)), []imap.Flag{flag},
			&imapclient.StoreOptions{Op: op}).Wait(ctx)
	})
}

// SetFlags replaces the whole flag set of one message.
func (s *Server) SetFlags(folder string, uid uint32, flags []imap.Flag) {
	s.t.Helper()
	s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
		if _, err := c.Select(folder, nil).Wait(ctx); err != nil {
			return err
		}
		return c.StoreUID(imap.UIDSetNum(imap.UID(uid)), flags, nil).Wait(ctx)
	})
}

// Expunge removes the given uids (marks \Deleted then UID EXPUNGE, so
// nothing else in the folder is touched).
func (s *Server) Expunge(folder string, uids ...uint32) {
	s.t.Helper()
	if len(uids) == 0 {
		return
	}
	s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
		if _, err := c.Select(folder, nil).Wait(ctx); err != nil {
			return err
		}
		set := imap.UIDSetNum(imap.UID(uids[0]))
		for _, u := range uids[1:] {
			set.AddNum(imap.UID(u))
		}
		if err := c.StoreUID(set, []imap.Flag{imap.FlagDeleted}, nil).Wait(ctx); err != nil {
			return err
		}
		return c.UIDExpunge(set, nil).Wait(ctx)
	})
}

// CreateFolder makes a folder, optionally with SPECIAL-USE attributes.
func (s *Server) CreateFolder(name string, use ...imap.MailboxAttr) {
	s.t.Helper()
	s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
		return c.Create(name, &imapclient.CreateOptions{SpecialUse: use}).Wait(ctx)
	})
}

// DeleteFolder removes a folder (also the fixture's UIDVALIDITY-reset
// trick: memory assigns a fresh validity to the recreated name).
// Servers refuse DELETE while any session holds the mailbox selected —
// the engine releases its selections a moment later — so a short
// retry keeps the fixture honest instead of racy.
func (s *Server) DeleteFolder(name string) {
	s.t.Helper()
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		lastErr = nil
		s.withAdmin(func(ctx context.Context, c *imapclient.Client) error {
			lastErr = c.Delete(name, nil).Wait(ctx)
			return nil
		})
		if lastErr == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("fixtureimap: delete %q: %v", name, lastErr)
}

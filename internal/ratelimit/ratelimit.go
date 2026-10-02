// Package ratelimit is the in-process, credential-aware half of the
// bridge's abuse protection (NFR-5, SECURITY-PLAN.md A1). The reverse
// proxy bounds raw volume before a request arrives; this layer knows
// whether an authentication attempt failed, so it can lock out a source
// that keeps guessing the client token.
//
// State is bounded: entries idle beyond the auth window are swept when
// the map reaches its cap, so the limiter cannot itself become a memory
// sink.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config tunes a Limiter. Zero fields take documented defaults in New.
type Config struct {
	// AuthFailures failed attempts within AuthWindow trip a lockout of
	// AuthBlock. A non-positive AuthFailures disables the lockout.
	AuthFailures int
	AuthWindow   time.Duration
	AuthBlock    time.Duration
	// ClientIPHeader names the header holding the real client IP when
	// the immediate peer is a trusted proxy (e.g. "CF-Connecting-IP").
	// Empty falls back to X-Forwarded-For.
	ClientIPHeader string
	// TrustedProxies are CIDRs whose forwarding headers are believed.
	// Empty means never trust X-Forwarded-For; the connection address
	// is used (fail-closed, at the cost of grouping proxy traffic).
	TrustedProxies []string
	// MaxKeys bounds tracked sources.
	MaxKeys int
}

// Limiter tracks per-source authentication failures.
type Limiter struct {
	cfg     Config
	trusted []*net.IPNet

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	fails        int
	windowStart  time.Time
	blockedUntil time.Time
}

// New returns a Limiter, applying defaults for zero-valued options.
func New(cfg Config) *Limiter {
	if cfg.AuthFailures <= 0 {
		cfg.AuthFailures = 10
	}
	if cfg.AuthWindow <= 0 {
		cfg.AuthWindow = 5 * time.Minute
	}
	if cfg.AuthBlock <= 0 {
		cfg.AuthBlock = 15 * time.Minute
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 10000
	}
	l := &Limiter{cfg: cfg, entries: map[string]*entry{}}
	for _, c := range cfg.TrustedProxies {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			l.trusted = append(l.trusted, n)
		}
	}
	return l
}

// Blocked reports whether key is locked out right now. An elapsed
// lockout is cleared so the next window starts clean.
func (l *Limiter) Blocked(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil || e.blockedUntil.IsZero() {
		return false
	}
	if now.Before(e.blockedUntil) {
		return true
	}
	e.blockedUntil = time.Time{}
	e.fails = 0
	e.windowStart = now
	return false
}

// Fail records one failed authentication for key, tripping a lockout
// once AuthFailures have accumulated inside AuthWindow.
func (l *Limiter) Fail(key string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(key, now)
	if e.windowStart.IsZero() || now.Sub(e.windowStart) > l.cfg.AuthWindow {
		e.windowStart = now
		e.fails = 0
	}
	e.fails++
	if e.fails >= l.cfg.AuthFailures {
		e.blockedUntil = now.Add(l.cfg.AuthBlock)
		e.fails = 0
	}
}

// Succeed clears the failure state for key — a correct credential resets
// the count but does not hand out a fresh lockout window.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[key]; e != nil {
		e.fails = 0
		e.windowStart = time.Time{}
		e.blockedUntil = time.Time{}
	}
}

// ClientIP returns the request's source address. X-Forwarded-For (or
// ClientIPHeader) is honoured only when the immediate peer is inside
// TrustedProxies; otherwise the connection address is used, so a direct
// client cannot spoof its way out of a lockout.
func (l *Limiter) ClientIP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !l.trustedPeer(peer) {
		return peer
	}
	if l.cfg.ClientIPHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(l.cfg.ClientIPHeader)); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				return ip.String()
			}
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Walk from the right: the hops our proxies appended are the
		// trustworthy ones, a client-supplied prefix is not.
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if ip == "" || l.trustedPeer(ip) {
				continue
			}
			if parsed := net.ParseIP(ip); parsed != nil {
				return parsed.String()
			}
		}
		if first := strings.TrimSpace(parts[0]); first != "" {
			return first
		}
	}
	return peer
}

func (l *Limiter) trustedPeer(hostport string) bool {
	ip := net.ParseIP(hostOnly(hostport))
	if ip == nil {
		return false
	}
	for _, n := range l.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (l *Limiter) entryLocked(key string, now time.Time) *entry {
	if e := l.entries[key]; e != nil {
		return e
	}
	if len(l.entries) >= l.cfg.MaxKeys {
		l.evictLocked(now)
	}
	e := &entry{}
	l.entries[key] = e
	return e
}

// evictLocked drops idle entries, then the least-recently-active one, so
// the map stays bounded under a spray of distinct keys.
func (l *Limiter) evictLocked(now time.Time) {
	for k, e := range l.entries {
		if !e.blockedUntil.IsZero() && now.Before(e.blockedUntil) {
			continue // active lockout: keep
		}
		if !e.windowStart.IsZero() && now.Sub(e.windowStart) <= l.cfg.AuthWindow {
			continue // active window: keep
		}
		delete(l.entries, k)
	}
	if len(l.entries) < l.cfg.MaxKeys {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for k, e := range l.entries {
		if oldestKey == "" || e.windowStart.Before(oldest) {
			oldestKey, oldest = k, e.windowStart
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

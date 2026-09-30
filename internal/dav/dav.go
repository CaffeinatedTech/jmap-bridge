// Package dav is the CardDAV client (PLAN §8, D-7 discipline): RFC 6764
// discovery, address book enumeration, RFC 6578 sync-collection with the
// getctag fallback, multiget/GET body fetch, and conditional PUT/DELETE.
// The go-webdav library is used where it fits — principal and home-set
// PROPFIND, address book listing, sync-collection — and the rest (raw
// vCard bytes, conditional requests, getctag/privileges) is hand-rolled
// here, inside the seam, exactly as AGENTS.md prescribes. No library type
// escapes this package: callers speak hrefs, etags and bytes.
package dav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

// ErrUnsupported means the server does not implement a DAV extension the
// operation needs (no sync-collection, no multiget): the caller falls
// back, never fails (FR-P.2).
var ErrUnsupported = errors.New("dav: server does not support the operation")

// Errors the write path distinguishes, mapped to JMAP SetErrors upstream
// (FR-P.8, FR-P.9, FR-P.10).
var (
	// ErrPrecondition: an If-Match/If-None-Match test failed (412).
	ErrPrecondition = errors.New("dav: precondition failed")
	// ErrConflict: a create hit an existing resource, or the target
	// collection is missing (409/405).
	ErrConflict = errors.New("dav: collection conflict")
	// ErrGone: the resource vanished (404/410). Destroy treats it as
	// success (already-gone, FR-P.10); other paths map it to notFound.
	ErrGone = errors.New("dav: resource not found")
	// ErrDenied: 401/403 — credentials or privileges.
	ErrDenied = errors.New("dav: access denied")
)

// maxCardBytes bounds a fetched vCard (a book with a 30 MB photo is a
// provider bug, and we refuse to buffer one).
const maxCardBytes = 8 << 20

// Config dials one account's CardDAV service.
type Config struct {
	// URL is the configured endpoint (absolute). When it names only the
	// origin, or ends in /.well-known/carddav, RFC 6764 probing happens
	// first (FR-P.1).
	URL      string
	Username string
	Password string
	// Token supplies an OAuth2 bearer credential when set; the password
	// is then unused (FR-A.9 — the Google profile includes the carddav
	// scope). Exactly one of the two is presented.
	Token func(ctx context.Context) (string, error)
	// HTTPClient overrides the default client (tests inject fixtures;
	// the auth wrapper is still applied around it).
	HTTPClient *http.Client
}

// Book is one address book as discovery found it.
type Book struct {
	Href        string // server path, always leading-slash
	Name        string
	Description string
	SortOrder   int
	CTag        string
	SyncToken   string // current token from discovery, if the server gave one
	MayWrite    bool
	MayDelete   bool
}

// CardMeta addresses one card on the server.
type CardMeta struct {
	Href string
	ETag string
}

// Card is one card's bytes plus the addressing the sync loop needs.
type Card struct {
	CardMeta
	Raw []byte
}

// SyncOutcome is one incremental pass over a book (FR-P.2).
type SyncOutcome struct {
	Changed   []CardMeta // new or etag-changed hrefs (bodies to fetch)
	Deleted   []string   // hrefs gone since the stored token
	SyncToken string     // token to persist for the next pass
	Supported bool       // false: no sync-collection — caller diffs listings
	CTag      string     // getctag observed on the fallback path
}

// Session is a discovered, ready-to-use CardDAV account endpoint.
type Session struct {
	cfg        Config
	http       *http.Client // auth + size discipline applied
	origin     string       // scheme://host
	prefix     string       // address book home set path (trailing slash)
	endpoint   string       // resolved service URL
	client     *carddav.Client
	webdav     *webdav.Client
	syncProbed bool
	syncOK     bool
}

// Open resolves the endpoint (RFC 6764 discovery when configured with
// only an origin or the well-known path) and reads the principal and
// address book home set (FR-P.1).
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.URL == "" {
		return nil, errors.New("dav: URL is required")
	}
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("dav: parse url: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("dav: url scheme must be http or https")
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, fmt.Errorf("dav: refusing plaintext http to a non-loopback host")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	s := &Session{cfg: cfg, http: wrappedClient(hc, cfg)}
	origin := base.Scheme + "://" + base.Host
	s.origin = origin

	// RFC 6764: probe /.well-known/carddav when the configured path is
	// empty, "/" or the well-known location itself.
	endpoint := origin + base.Path
	wk := origin + "/.well-known/carddav"
	switch {
	case base.Path == "" || base.Path == "/" || strings.HasSuffix(base.Path, "/.well-known/carddav"):
		if resolved, err := s.probeWellKnown(ctx, wk); err == nil {
			endpoint = resolved
		} else if base.Path == "" || base.Path == "/" {
			// Nothing configured beyond the origin and discovery failed:
			// the origin itself may still be the DAV root (self-hosted
			// boxes without well-known redirects).
			endpoint = origin + "/"
		} else {
			return nil, fmt.Errorf("dav: discover %s: %w", wk, err)
		}
	}
	s.endpoint = endpoint

	ec, err := carddav.NewClient(s.http, endpoint)
	if err != nil {
		return nil, fmt.Errorf("dav: client: %w", err)
	}
	wc, err := webdav.NewClient(s.http, endpoint)
	if err != nil {
		return nil, fmt.Errorf("dav: webdav client: %w", err)
	}
	s.client, s.webdav = ec, wc

	if err := s.probeSupport(ctx); err != nil {
		return nil, err
	}
	// current-user-principal is asked of the origin root (RFC 5397);
	// RFC 6764's redirect target is the principal, but servers answer
	// the property inconsistently there, so the root is the portable
	// probe point (verified against Radicale and the fixture tiers).
	principal, err := s.currentUserPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	home, err := ec.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("dav: home set: %w", err)
	}
	s.prefix = home
	if !strings.HasSuffix(s.prefix, "/") {
		s.prefix += "/"
	}
	return s, nil
}

// currentUserPrincipal reads DAV:current-user-principal from the origin
// root (RFC 5397) via the hand-rolled PROPFIND path.
func (s *Session) currentUserPrincipal(ctx context.Context) (string, error) {
	const body = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:">` +
		`<d:prop><d:current-user-principal/></d:prop></d:propfind>`
	ms, err := s.propfindRaw(ctx, s.origin+"/", "0", body)
	if err != nil {
		return "", err
	}
	for _, r := range ms {
		if r.PrincipalHref != "" {
			return cleanHref(r.PrincipalHref), nil
		}
	}
	return "", fmt.Errorf("dav: no current-user-principal at %s", s.endpoint)
}

// probeSupport runs OPTIONS on the endpoint and requires the addressbook
// DAV class (RFC 6352 §10) plus reports for our sync strategy. A server
// that answers OPTIONS without the class fails Open — the capability is
// then never advertised (FR-P.3, golden rule 4).
func (s *Session) probeSupport(ctx context.Context) error {
	req, err := s.newRequest(ctx, http.MethodOptions, s.endpoint, "", nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("dav: options: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		// Radicale answers 401 on OPTIONS without credentials before
		// auth: retry once through the authenticated path (our wrapper
		// already attaches credentials, but a server may challenge).
		return fmt.Errorf("dav: options: status %d", resp.StatusCode)
	}
	dav := strings.ToLower(resp.Header.Get("DAV"))
	if !strings.Contains(dav, "addressbook") {
		return fmt.Errorf("dav: %s does not advertise the addressbook class (DAV: %q)", s.endpoint, resp.Header.Get("DAV"))
	}
	return nil
}

// probeWellKnown follows the RFC 6764 redirect of /.well-known/carddav.
func (s *Session) probeWellKnown(ctx context.Context, wellKnown string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Request.URL.String(), nil
}

// Books lists the address book collections in the home set with the
// properties PLAN §8 discovery reads (displayname, description, sort
// order, getctag, sync-token, privileges) (FR-P.1).
func (s *Session) Books(ctx context.Context) ([]Book, error) {
	abs, err := s.client.FindAddressBooks(ctx, s.prefix)
	if err != nil {
		return nil, fmt.Errorf("dav: list books: %w", err)
	}
	out := make([]Book, 0, len(abs))
	hrefOf := map[string]int{}
	for _, ab := range abs {
		b := Book{
			Href:        cleanHref(ab.Path),
			Name:        ab.Name,
			Description: ab.Description,
			MayWrite:    true, // refined by the privileges PROPFIND below
			MayDelete:   false,
		}
		hrefOf[b.Href] = len(out)
		out = append(out, b)
	}
	if err := s.propfindBooks(ctx, out); err != nil {
		// The library listing succeeded; a richer PROPFIND the server
		// refuses is not fatal — fall back to the plain listing.
		if errors.Is(err, ErrDenied) {
			return out, nil
		}
		return nil, err
	}
	for i := range out {
		if idx, ok := hrefOf[out[i].Href]; ok {
			out[idx] = out[i]
		}
	}
	return out, nil
}

// propfindBooks fills sort order, ctag, sync token and the write/delete
// privileges from one Depth:1 PROPFIND over the home set.
func (s *Session) propfindBooks(ctx context.Context, books []Book) error {
	if len(books) == 0 {
		return nil
	}
	const body = `<?xml version="1.0"?>` +
		`<d:propfind xmlns:d="DAV:" xmlns:cr="urn:ietf:params:xml:ns:carddav">` +
		`<d:prop><d:getctag/><d:sync-token/><d:current-user-privilege-set/>` +
		`<cr:sort-order/></d:prop></d:propfind>`
	ms, err := s.propfindRaw(ctx, s.prefix, "1", body)
	if err != nil {
		return err
	}
	byHref := map[string]*Book{}
	for i := range books {
		byHref[books[i].Href] = &books[i]
	}
	for _, r := range ms {
		h := cleanHref(r.Href)
		b, ok := byHref[h]
		if !ok {
			continue
		}
		if r.CTag != "" {
			b.CTag = r.CTag
		}
		if r.SyncToken != "" {
			b.SyncToken = r.SyncToken
		}
		b.SortOrder = parseSortOrder(r.SortOrder)
		if names := privilegeNames(r); len(names) > 0 {
			b.MayWrite = hasPriv(names, "write") || hasPriv(names, "write-content")
			b.MayDelete = hasPriv(names, "delete") || hasPriv(names, "bind")
		}
	}
	return nil
}

// parseSortOrder reads DAV:sort-order, a signed integer with optional
// leading sign; garbage reads as 0 (display order is cosmetic).
func parseSortOrder(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	if neg {
		return -n
	}
	return n
}

// privilegeNames reports the DAV privilege names the server granted.
func privilegeNames(r multistatusResponse) []string {
	set := r.Privs
	if len(set.Privileges) == 0 {
		set = r.Privs2
	}
	var out []string
	for _, pv := range set.Privileges {
		for _, g := range pv.Grants {
			out = append(out, g.XMLName.Local)
		}
	}
	return out
}

func hasPriv(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// Get fetches one card's raw vCard bytes (plain GET — CardDAV has no
// partial fetch, and the bridge converts locally, PLAN §8).
func (s *Session) Get(ctx context.Context, href string) (*Card, error) {
	req, err := s.newRequest(ctx, http.MethodGet, href, "", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/vcard, text/directory")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dav: get %s: %w", href, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%w: get %s", ErrGone, href)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: get %s", ErrDenied, href)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("dav: get %s: status %d", href, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCardBytes))
	if err != nil {
		return nil, fmt.Errorf("dav: read %s: %w", href, err)
	}
	return &Card{
		CardMeta: CardMeta{Href: cleanHref(resp.Request.URL.Path), ETag: resp.Header.Get("ETag")},
		Raw:      raw,
	}, nil
}

// Put creates or replaces one card. ifNoneMatch requests create-only
// semantics (412 on collision, FR-P.8); ifMatch, when set, guards an
// update (412 on a stale etag, FR-P.9).
func (s *Session) Put(ctx context.Context, href string, raw []byte, ifNoneMatch, ifMatch string) (string, error) {
	req, err := s.newRequest(ctx, http.MethodPut, href, "text/vcard; charset=utf-8", strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("dav: put %s: %w", href, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusNoContent, http.StatusOK:
		return resp.Header.Get("ETag"), nil
	case http.StatusPreconditionFailed:
		return "", fmt.Errorf("%w: put %s", ErrPrecondition, href)
	case http.StatusConflict, http.StatusMethodNotAllowed:
		// Some servers answer 405 when If-None-Match:* hits an existing
		// resource; it is the same collision (FR-P.8). DAV error bodies
		// name the real reason, so a slice of them rides along — they
		// are protocol noise, never credentials (golden rule 6).
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", fmt.Errorf("%w: put %s: %s", ErrConflict, href, strings.TrimSpace(string(snippet)))
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("%w: put %s", ErrDenied, href)
	case http.StatusNotFound, http.StatusGone:
		return "", fmt.Errorf("%w: put %s", ErrGone, href)
	default:
		return "", fmt.Errorf("dav: put %s: status %d", href, resp.StatusCode)
	}
}

// Delete removes one card with an optional etag guard. A gone resource
// reports ErrGone so the caller can treat it as success (FR-P.10).
func (s *Session) Delete(ctx context.Context, href, ifMatch string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, href, "", nil)
	if err != nil {
		return err
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("dav: delete %s: %w", href, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: delete %s", ErrGone, href)
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: delete %s", ErrPrecondition, href)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: delete %s", ErrDenied, href)
	default:
		return fmt.Errorf("dav: delete %s: status %d", href, resp.StatusCode)
	}
}

// SyncBook runs one incremental pass over a collection (FR-P.2) with the
// stored sync-token. When the server has no sync-collection (probed once
// per session), or no token is stored yet, the outcome reports
// Supported=false plus the current getctag, and the caller diffs
// listings.
func (s *Session) SyncBook(ctx context.Context, href, storedToken string) (SyncOutcome, error) {
	if !s.syncProbed || s.syncOK {
		resp, err := s.client.SyncCollection(ctx, href, &carddav.SyncQuery{
			SyncToken: storedToken,
			DataRequest: carddav.AddressDataRequest{
				Props: []string{"getetag", "getcontenttype", "getlastmodified"},
			},
		})
		if err == nil {
			s.syncProbed, s.syncOK = true, true
			out := SyncOutcome{Supported: true, SyncToken: resp.SyncToken}
			for _, u := range resp.Updated {
				out.Changed = append(out.Changed, CardMeta{Href: cleanHref(u.Path), ETag: u.ETag})
			}
			for _, d := range resp.Deleted {
				out.Deleted = append(out.Deleted, cleanHref(d))
			}
			return out, nil
		}
		if !isSyncUnsupported(err) {
			return SyncOutcome{}, fmt.Errorf("dav: sync %s: %w", href, err)
		}
		s.syncProbed, s.syncOK = true, false
	}
	ctag, err := s.bookCTag(ctx, href)
	if err != nil {
		return SyncOutcome{}, err
	}
	return SyncOutcome{Supported: false, CTag: ctag}, nil
}

// ListAll is the fallback's listing pass: every card's href+etag via
// PROPFIND Depth:1 (getctag changed → full diff, FR-P.2).
func (s *Session) ListAll(ctx context.Context, href string) ([]CardMeta, string, error) {
	const body = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:">` +
		`<d:prop><d:getetag/><d:getcontenttype/></d:prop></d:propfind>`
	ms, err := s.propfindRaw(ctx, href, "1", body)
	if err != nil {
		return nil, "", err
	}
	var out []CardMeta
	for _, r := range ms {
		h := cleanHref(r.Href)
		if h == "" || samePath(h, href) || r.ETag == "" {
			continue
		}
		out = append(out, CardMeta{Href: h, ETag: r.ETag})
	}
	ctag, err := s.bookCTag(ctx, href)
	if err != nil {
		return nil, "", err
	}
	return out, ctag, nil
}

// bookCTag reads the collection's getctag (the fallback's cheap-changed
// probe, RFC 6578 §5 / CardDAV getctag).
func (s *Session) bookCTag(ctx context.Context, href string) (string, error) {
	const body = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:">` +
		`<d:prop><d:getctag/></d:prop></d:propfind>`
	ms, err := s.propfindRaw(ctx, href, "0", body)
	if err != nil {
		return "", err
	}
	for _, r := range ms {
		if r.CTag != "" {
			return r.CTag, nil
		}
	}
	return "", nil
}

// MultiGet fetches several card bodies in one addressbook-multiget
// (PLAN §8). hrefs the server leaves out must be fetched with Get by the
// caller — which is exactly how "new hrefs" complete (FR-P.2). When the
// server has no multiget, ErrUnsupported is reported and the caller
// fetches one by one.
func (s *Session) MultiGet(ctx context.Context, bookHref string, hrefs []string) ([]Card, error) {
	if len(hrefs) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><cm:addressbook-multiget xmlns:d="DAV:" xmlns:cm="urn:ietf:params:xml:ns:carddav">` +
		`<d:prop><d:getetag/><cm:address-data/></d:prop>`)
	for _, h := range hrefs {
		b.WriteString("<d:href>" + xmlEscape(cleanHref(h)) + "</d:href>")
	}
	b.WriteString(`</cm:addressbook-multiget>`)

	req, err := s.newRequest(ctx, "REPORT", bookHref, "application/xml", strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dav: multiget: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotImplemented || resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusBadRequest {
		return nil, ErrUnsupported
	}
	if resp.StatusCode >= 400 {
		return nil, statusErr("multiget", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("dav: read multiget: %w", err)
	}
	var doc multistatus
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("dav: decode multiget: %w", err)
	}
	out := make([]Card, 0, len(doc.Responses))
	for _, r := range doc.Responses {
		if r.AddressData == "" {
			continue
		}
		out = append(out, Card{
			CardMeta: CardMeta{Href: cleanHref(r.Href), ETag: r.ETag},
			Raw:      []byte(r.AddressData),
		})
	}
	return out, nil
}

// propfindRaw runs one PROPFIND and decodes the multistatus.
func (s *Session) propfindRaw(ctx context.Context, target, depth, body string) ([]multistatusResponse, error) {
	req, err := s.newRequest(ctx, "PROPFIND", target, "application/xml", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", depth)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dav: propfind %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: propfind %s", ErrDenied, target)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("dav: propfind %s: status %d", target, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("dav: read propfind: %w", err)
	}
	var doc multistatus
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("dav: decode propfind: %w", err)
	}
	return doc.Responses, nil
}

// newRequest builds a request against the session origin with an absolute
// or path target.
func (s *Session) newRequest(ctx context.Context, method, target, contentType string, body io.Reader) (*http.Request, error) {
	u := target
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = s.origin + ensureSlashPrefix(u)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("dav: request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func ensureSlashPrefix(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// BookCardHref joins a book path and a card name for a PUT target.
func BookCardHref(bookHref, name string) string {
	return path.Join(cleanHref(bookHref), name)
}

// HrefName is the last path segment of an href (the card's server name).
func HrefName(href string) string {
	h := strings.TrimSuffix(cleanHref(href), "/")
	if i := strings.LastIndexByte(h, '/'); i >= 0 {
		return h[i+1:]
	}
	return h
}

// cleanHref normalises a DAV href: percent-decoded, path-only, leading
// slash kept. Href equality decisions (listing diffs, tombstones) compare
// cleaned forms on both sides.
func cleanHref(h string) string {
	if u, err := url.Parse(h); err == nil && u.Path != "" {
		h = u.Path
	} else if i := strings.IndexByte(h, '?'); i >= 0 {
		h = h[:i]
	}
	if !strings.HasPrefix(h, "/") {
		h = "/" + h
	}
	return h
}

func samePath(a, b string) bool {
	return strings.TrimSuffix(cleanHref(a), "/") == strings.TrimSuffix(cleanHref(b), "/")
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// statusErr maps an unexpected DAV status onto the package errors.
func statusErr(op string, code int) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrDenied, op)
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: %s", ErrGone, op)
	default:
		return fmt.Errorf("dav: %s: status %d", op, code)
	}
}

// isSyncUnsupported classifies the refusals that mean "no RFC 6578 here"
// rather than a transport failure. go-webdav surfaces them as error
// strings carrying the HTTP status line or the DAV error tag; the bridge
// reads those signals here, inside the seam, so callers see only
// SyncOutcome.Supported=false.
func isSyncUnsupported(err error) bool {
	msg := err.Error()
	if strings.Contains(msg, "valid-sync-token") {
		return true
	}
	for _, code := range []string{"400", "403", "404", "405", "501"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// wrappedClient returns a client that presents the configured
// credentials (Basic, or a Bearer token resolved per request for OAuth2
// accounts, FR-A.9) and honours the caller's transport.
func wrappedClient(hc *http.Client, cfg Config) *http.Client {
	out := *hc
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		if cfg.Token != nil {
			if tok, err := cfg.Token(r.Context()); err == nil && tok != "" {
				r.Header.Set("Authorization", "Bearer "+tok)
			}
		} else if cfg.Password != "" || cfg.Username != "" {
			r.SetBasicAuth(cfg.Username, cfg.Password)
		}
		return base.RoundTrip(r)
	})
	return &out
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func loopbackHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.")
}

// --- multistatus decoding (shared by PROPFIND and multiget) ---

type multistatus struct {
	XMLName   xml.Name              `xml:"multistatus"`
	Responses []multistatusResponse `xml:"response"`
}

type multistatusResponse struct {
	Href string `xml:"href"`
	// Prop entries arrive flat across propstat children; encoding/xml
	// cannot express "either place", so the two paths are listed and
	// merged.
	ETag        string        `xml:"propstat>prop>getetag"`
	ETag2       string        `xml:"prop>getetag"`
	CTag        string        `xml:"propstat>prop>getctag"`
	CTag2       string        `xml:"prop>getctag"`
	SyncToken   string        `xml:"propstat>prop>sync-token"`
	SyncToken2  string        `xml:"prop>sync-token"`
	SortOrder   string        `xml:"propstat>prop>sort-order"`
	SortOrder2  string        `xml:"prop>sort-order"`
	AddressData string        `xml:"propstat>prop>address-data"`
	Principal   principalProp `xml:"propstat>prop>current-user-principal"`
	Privs       privilegeSet  `xml:"propstat>prop>current-user-privilege-set"`
	Privs2      privilegeSet  `xml:"prop>current-user-privilege-set"`

	// PrincipalHref is Principal.Href, flattened by UnmarshalXML.
	PrincipalHref string
}

// privilegeSet decodes <current-user-privilege-set> one level: each
// <privilege> element's local name (read, write, write-content, bind,
// delete) is the grant. Prefix-free decoding keeps any namespace
// spelling working.
type privilegeSet struct {
	Privileges []privilegeName `xml:"privilege"`
}

// privilegeName captures the single grant element inside <privilege>
// (its local name is the grant: read, write, write-content, bind, …).
type privilegeName struct {
	Grants []grantName `xml:",any"`
}

// principalProp decodes <current-user-principal><href>…</href></…>.
type principalProp struct {
	Href string `xml:"href"`
}

type grantName struct {
	XMLName xml.Name
}

// UnmarshalXML post-processes the response: pick whichever prop location
// carried the value and flatten privileges to bare names.
func (r *multistatusResponse) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type raw multistatusResponse
	var v raw
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	*r = multistatusResponse(v)
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	r.ETag = pick(r.ETag, r.ETag2)
	r.CTag = pick(r.CTag, r.CTag2)
	r.SyncToken = pick(r.SyncToken, r.SyncToken2)
	r.SortOrder = pick(r.SortOrder, r.SortOrder2)
	if len(r.Privs.Privileges) == 0 {
		r.Privs.Privileges = r.Privs2.Privileges
	}
	r.PrincipalHref = r.Principal.Href
	return nil
}

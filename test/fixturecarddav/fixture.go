// Package fixturecarddav is an in-process CardDAV server for protocol
// tests (AGENTS.md testing rules): a real HTTP server speaking real
// XML, no real network, no provider. It impersonates the shapes M6's
// sync must survive:
//
//   - Sync tier: OPTIONS advertises DAV 3 + addressbook, REPORT
//     sync-collection works, addressbook-multiget works, cards carry
//     getetag, collections carry getctag.
//   - Getctag tier (no RFC 6578): sync-collection answers 400 with a
//     valid-sync-token error; ctag/listing/multiget work.
//   - Bare tier: no sync, no ctag, no multiget — plain PROPFIND + GET +
//     conditional PUT/DELETE only.
//
// Conditional writes (If-Match / If-None-Match: *) are enforced in every
// tier, because 412-on-stale-etag is a behaviour the bridge's write path
// must get right regardless of server shape (FR-P.8, FR-P.9).
//
// One mutex serialises every request — deliberate for a test fixture:
// handler body reads happen under it, and the tests await each response
// anyway.
package fixturecarddav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
)

// Tier selects which server shape the fixture impersonates.
type Tier int

const (
	// TierSync is a full RFC 6352 + RFC 6578 server (Radicale-shaped).
	TierSync Tier = iota
	// TierGetctag has no sync-collection but a working getctag.
	TierGetctag
	// TierBare is PROPFIND/GET/PUT/DELETE only (the conservative
	// baseline the fallback chain must reach).
	TierBare
)

type card struct {
	Body string
	ETag string
}

type syncEntry struct {
	href string
	etag string // "" means deleted
}

type book struct {
	Href     string
	Cards    map[string]*card
	CTag     string
	SyncSeq  int
	SyncLog  map[int][]syncEntry
	Display  string
	ReadOnly bool
}

// Server is the fixture.
type Server struct {
	tier   Tier
	mu     sync.Mutex
	books  map[string]*book
	princ  string
	home   string
	AuthU  string
	AuthP  string
	ts     *httptest.Server
	Counts map[string]int
}

// New starts a fixture server of the given tier with one empty book at
// {prefix}/addresses.vcf/.
func New(tier Tier, user, pass string) *Server {
	s := &Server{
		tier:   tier,
		books:  map[string]*book{},
		princ:  "/test/",
		home:   "/test/",
		AuthU:  user,
		AuthP:  pass,
		Counts: map[string]int{},
	}
	s.addBookLocked("/test/addresses.vcf/", "Addresses", false)
	s.ts = httptest.NewServer(s)
	return s
}

func (s *Server) addBookLocked(href, display string, readOnly bool) {
	if !strings.HasSuffix(href, "/") {
		href += "/"
	}
	b := &book{
		Href: href, Cards: map[string]*card{}, Display: display,
		SyncLog: map[int][]syncEntry{}, ReadOnly: readOnly,
	}
	if s.tier != TierBare {
		b.CTag = "1"
	}
	s.books[href] = b
}

// URL is the service root for dav.Config.
func (s *Server) URL() string { return s.ts.URL }

// Close shuts the fixture down.
func (s *Server) Close() { s.ts.Close() }

// AddBook creates an extra address book (discovery lists all).
func (s *Server) AddBook(href string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addBookLocked(href, "Extra", false)
}

// AddReadOnlyBook creates a book whose privileges say read-only.
func (s *Server) AddReadOnlyBook(href string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addBookLocked(href, "View Only", true)
}

// PutCard stores a card fixture-side (seeding that bypasses conditional
// logic; etag is content-derived).
func (s *Server) PutCard(bookHref, name, vcard string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putCardLocked(bookHref, name, vcard)
}

// DeleteCard removes one card fixture-side.
func (s *Server) DeleteCard(bookHref, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	href := path.Join(bookHref, name)
	if b, ok := s.books[bookHref]; ok {
		delete(b.Cards, href)
		s.touchLocked(b, href, "")
	}
}

// BookCTag exposes the current getctag for assertions.
func (s *Server) BookCTag(href string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.books[href].CTag
}

func (s *Server) putCardLocked(bookHref, name, vcard string) string {
	sum := sha256.Sum256([]byte(vcard))
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	href := path.Join(bookHref, name)
	b, ok := s.books[bookHref]
	if !ok {
		s.addBookLocked(bookHref, path.Base(bookHref), false)
		b = s.books[bookHref]
	}
	b.Cards[href] = &card{Body: vcard, ETag: etag}
	s.touchLocked(b, href, etag)
	return etag
}

// touchLocked advances ctag + sync bookkeeping after a content change.
func (s *Server) touchLocked(b *book, href, etag string) {
	if s.tier != TierBare {
		n := 0
		if _, err := fmt.Sscanf(b.CTag, "%d", &n); err != nil {
			n = 0
		}
		b.CTag = fmt.Sprintf("%d", n+1)
	}
	if s.tier == TierSync {
		b.SyncSeq++
		b.SyncLog[b.SyncSeq] = []syncEntry{{href, etag}}
	}
}

// ServeHTTP implements the DAV surface.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.AuthU != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != s.AuthU || p != s.AuthP {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Path == "/.well-known/carddav" {
		http.Redirect(w, r, s.princ, http.StatusFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Counts[r.Method]++
	switch r.Method {
	case http.MethodOptions:
		dav := "1, 2"
		if s.tier == TierSync {
			dav += ", 3"
		}
		dav += ", addressbook"
		w.Header().Set("DAV", dav)
		w.Header().Set("Allow", "OPTIONS, GET, PUT, DELETE, PROPFIND, REPORT")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodPut:
		s.handlePut(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	case "PROPFIND":
		s.handlePropfind(w, r)
	case "REPORT":
		s.handleReport(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	b, name := s.splitLocked(r.URL.Path)
	if b == nil || name == "" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>carddav fixture</body></html>"))
		return
	}
	c, ok := b.Cards[r.URL.Path]
	if !ok {
		http.Error(w, "gone", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("ETag", c.ETag)
	_, _ = w.Write([]byte(c.Body))
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	href := r.URL.Path
	b, name := s.splitLocked(href)
	if name == "" {
		http.Error(w, "bad put", http.StatusBadRequest)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/vcard") {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	body := readAll(r)
	if b.ReadOnly {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Header.Get("If-None-Match") == "*" {
		if _, exists := b.Cards[href]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}
	if ifm := r.Header.Get("If-Match"); ifm != "" {
		c, exists := b.Cards[href]
		if !exists || !etagMatches(ifm, c.ETag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}
	etag := s.putCardLocked(b.Href, name, body)
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	href := r.URL.Path
	b, name := s.splitLocked(href)
	if name == "" {
		http.Error(w, "bad delete", http.StatusBadRequest)
		return
	}
	c, exists := b.Cards[href]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if ifm := r.Header.Get("If-Match"); ifm != "" && !etagMatches(ifm, c.ETag) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	delete(b.Cards, href)
	s.touchLocked(b, href, "")
	w.WriteHeader(http.StatusNoContent)
}

type propfindReq struct {
	XMLName xml.Name `xml:"propfind"`
	Prop    struct {
		Raw []byte `xml:",innerxml"`
	} `xml:"prop"`
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request) {
	req := propfindReq{}
	_ = xml.Unmarshal(readAllBytes(r), &req)
	inner := string(req.Prop.Raw)
	want := func(tag string) bool { return strings.Contains(inner, tag) }

	p := r.URL.Path

	if want("current-user-principal") {
		multiStatus(w, fmt.Sprintf(
			`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:current-user-principal><D:href>%s</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
			p, s.princ))
		return
	}
	if want("addressbook-home-set") {
		multiStatus(w, fmt.Sprintf(
			`<D:response><D:href>%s</D:href><D:propstat><D:prop><CARD:addressbook-home-set><D:href>%s</D:href></CARD:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
			p, s.home))
		return
	}
	if b, ok := s.books[p]; ok && r.Header.Get("Depth") == "1" {
		s.collectionResponse(w, b, want)
		return
	}
	if p == s.home || want("resourcetype") || want("displayname") {
		// Home-set PROPFIND: every book with the properties the client
		// asked for (discovery listing *and* the bridge's richer
		// getctag/sync-token/privileges pass share this branch).
		var sb strings.Builder
		for _, b := range sortedBooks(s.books) {
			fmt.Fprintf(&sb,
				`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:resourcetype><D:collection/><CARD:addressbook/></D:resourcetype><D:displayname>%s</D:displayname>`,
				b.Href, xmlEscape(b.Display))
			s.appendBookProps(&sb, b, want)
			sb.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		}
		multiStatus(w, sb.String())
		return
	}
	if want("getctag") {
		if b, ok := s.books[p]; ok && b.CTag != "" {
			multiStatus(w, fmt.Sprintf(
				`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getctag>%s</D:getctag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
				p, b.CTag))
			return
		}
		multiStatus(w, fmt.Sprintf(
			`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getctag/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>`,
			p))
		return
	}
	http.Error(w, "unsupported propfind", http.StatusBadRequest)
}

// collectionResponse answers the Depth:1 book PROPFIND: collection props
// on the book href, then every card's getetag.
func (s *Server) collectionResponse(w http.ResponseWriter, b *book, want func(string) bool) {
	var sb strings.Builder
	fmt.Fprintf(&sb, `<D:response><D:href>%s</D:href><D:propstat><D:prop>`, b.Href)
	// FindAddressBooks decodes resourcetype from every response and
	// fails hard when it is missing, so the collection and its cards all
	// carry it (cards: empty → not an addressbook → skipped).
	sb.WriteString(`<D:resourcetype><D:collection/><CARD:addressbook/></D:resourcetype>`)
	fmt.Fprintf(&sb, `<D:displayname>%s</D:displayname>`, xmlEscape(b.Display))
	s.appendBookProps(&sb, b, want)
	sb.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	hrefs := make([]string, 0, len(b.Cards))
	for h := range b.Cards {
		hrefs = append(hrefs, h)
	}
	sort.Strings(hrefs)
	for _, h := range hrefs {
		c := b.Cards[h]
		fmt.Fprintf(&sb,
			`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:resourcetype/><D:getetag>%s</D:getetag><D:getcontenttype>text/vcard</D:getcontenttype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
			h, c.ETag)
	}
	multiStatus(w, sb.String())
}

func (s *Server) appendBookProps(sb *strings.Builder, b *book, want func(string) bool) {
	if want("getctag") && b.CTag != "" {
		fmt.Fprintf(sb, `<D:getctag>%s</D:getctag>`, b.CTag)
	}
	if want("sync-token") && s.tier == TierSync {
		fmt.Fprintf(sb, `<D:sync-token>http://fixture/sync/%d</D:sync-token>`, b.SyncSeq)
	}
	if want("current-user-privilege-set") {
		if b.ReadOnly {
			sb.WriteString(`<D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set>`)
		} else {
			sb.WriteString(`<D:current-user-privilege-set><D:privilege><D:read/><D:write/><D:write-content/><D:bind/><D:delete/></D:privilege></D:current-user-privilege-set>`)
		}
	}
	if want("sort-order") {
		sb.WriteString(`<CARD:sort-order>100</CARD:sort-order>`)
	}
	if want("supported-address-data") {
		sb.WriteString(`<CARD:supported-address-data><CARD:address-data-type CARD:content-type="text/vcard" CARD:version="3.0"/><CARD:address-data-type CARD:content-type="text/vcard" CARD:version="4.0"/></CARD:supported-address-data>`)
	}
}

// peekRoot reads the local name of the report body's root element;
// clients send sync-collection or addressbook-multiget directly, and
// encoding/xml matches struct fields against *children* of the root, so
// the root name must be inspected before decoding the right shape.
func peekRoot(raw []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	raw := readAllBytes(r)
	switch peekRoot(raw) {
	case "sync-collection":
		var q struct {
			SyncToken string `xml:"sync-token"`
		}
		if err := xml.Unmarshal(raw, &q); err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		s.handleSyncReport(w, r, q.SyncToken)
		return
	case "addressbook-multiget":
		var q struct {
			Hrefs []string `xml:"href"`
		}
		if err := xml.Unmarshal(raw, &q); err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		s.handleMultigetReport(w, r, q.Hrefs)
		return
	default:
		http.Error(w, "unknown report", http.StatusBadRequest)
		return
	}
}

func (s *Server) handleMultigetReport(w http.ResponseWriter, r *http.Request, hrefs []string) {
	href := r.URL.Path
	b, ok := s.books[href]
	if !ok {
		http.Error(w, "no such collection", http.StatusNotFound)
		return
	}

	if s.tier == TierBare {
		http.Error(w, "no multiget", http.StatusNotFound)
		return
	}
	{
		var sb strings.Builder
		for _, h := range hrefs {
			c, exists := b.Cards[h]
			if !exists {
				fmt.Fprintf(&sb, `<D:response><D:href>%s</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`, h)
				continue
			}
			fmt.Fprintf(&sb,
				`<D:response><D:href>%s</D:href><D:propstat><D:prop><CARD:address-data>%s</CARD:address-data><D:getetag>%s</D:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
				h, xmlEscape(c.Body), c.ETag)
		}
		multiStatus(w, sb.String())
		return
	}
}

func (s *Server) handleSyncReport(w http.ResponseWriter, r *http.Request, tokenStr string) {
	href := r.URL.Path
	b, ok := s.books[href]
	if !ok {
		http.Error(w, "no such collection", http.StatusNotFound)
		return
	}
	{
		if s.tier != TierSync {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`))
			return
		}
		token := 0
		if tokenStr != "" {
			if _, err := fmt.Sscanf(tokenStr, "http://fixture/sync/%d", &token); err != nil {
				http.Error(w, "invalid sync token", http.StatusBadRequest)
				return
			}
		}
		// An unknown token forces a full resync (RFC 6578 §3.2's
		// valid-sync-token path), exactly like a real server whose
		// token window has expired.
		if tokenStr != "" && token > b.SyncSeq {
			token = 0
		}
		var sb strings.Builder
		if token < b.SyncSeq {
			// Replay the DELTA after `token`: SyncLog[t] holds only the
			// change made at step t.
			latest := map[string]syncEntry{}
			for t := token + 1; t <= b.SyncSeq; t++ {
				for _, e := range b.SyncLog[t] {
					latest[e.href] = e
				}
			}
			var hrefs []string
			for h := range latest {
				hrefs = append(hrefs, h)
			}
			sort.Strings(hrefs)
			for _, h := range hrefs {
				e := latest[h]
				if e.etag == "" {
					fmt.Fprintf(&sb, `<D:response><D:href>%s</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`, h)
					continue
				}
				fmt.Fprintf(&sb,
					`<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getetag>%s</D:getetag><D:getcontenttype>text/vcard</D:getcontenttype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
					h, e.etag)
			}
		}
		fmt.Fprintf(&sb, `<D:sync-token>http://fixture/sync/%d</D:sync-token>`, b.SyncSeq)
		multiStatus(w, sb.String())
		return
	}
}

// splitLocked returns the book + whether the path names a card inside
// it. Callers hold s.mu.
func (s *Server) splitLocked(p string) (*book, string) {
	for href, b := range s.books {
		if strings.HasPrefix(p, href) {
			name := strings.TrimPrefix(p, href)
			if name != "" {
				return b, name
			}
		}
	}
	if b, ok := s.books[p]; ok {
		return b, ""
	}
	return nil, ""
}

func sortedBooks(m map[string]*book) []*book {
	out := make([]*book, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Href < out[j].Href })
	return out
}

func etagMatches(given, have string) bool {
	for _, e := range strings.Split(given, ",") {
		e = strings.TrimSpace(e)
		if e == "*" || e == have || strings.Trim(e, `"`) == strings.Trim(have, `"`) {
			return true
		}
	}
	return false
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func multiStatus(w http.ResponseWriter, responses string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(`<?xml version="1.0"?>` +
		`<D:multistatus xmlns:D="DAV:" xmlns:CARD="urn:ietf:params:xml:ns:carddav">`))
	_, _ = w.Write([]byte(responses))
	_, _ = w.Write([]byte(`</D:multistatus>`))
}

func readAll(r *http.Request) string { return string(readAllBytes(r)) }

func readAllBytes(r *http.Request) []byte {
	defer func() { _ = r.Body.Close() }()
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return raw
}

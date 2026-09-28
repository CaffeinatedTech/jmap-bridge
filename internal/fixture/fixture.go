// Package fixture is M0's in-memory jmapapi.Store: deterministic sample
// mail served while there is no sync engine yet (PLAN §12, M0). M1
// replaces it with the SQLite-backed internal/store behind the same
// seam; the wire behaviour the fixture produces is the contract the
// protocol tests pin.
package fixture

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// Store implements jmapapi.Store over per-account in-memory data. Data
// is seeded on first use of an account id, so any configured account
// serves the same deterministic sample mail.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]*accountData
}

type accountData struct {
	mailboxes []*jmapapi.Mailbox
	emails    []*jmapapi.Email
}

// States are static: the fixture never mutates, so every type state is
// the initial counter value (PLAN §4.1).
const (
	mailboxState = "1"
	emailState   = "1"
)

// New returns an empty fixture store; accounts seed on first use.
func New() *Store {
	return &Store{accounts: map[string]*accountData{}}
}

func (s *Store) data(account string) *accountData {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.accounts[account]
	if !ok {
		d = seed()
		s.accounts[account] = d
	}
	return d
}

// Mailboxes implements jmapapi.Store.
func (s *Store) Mailboxes(_ context.Context, account string) ([]*jmapapi.Mailbox, string, error) {
	d := s.data(account)
	out := make([]*jmapapi.Mailbox, len(d.mailboxes))
	copy(out, d.mailboxes)
	return out, mailboxState, nil
}

// MailboxesByID implements jmapapi.Store.
func (s *Store) MailboxesByID(_ context.Context, account string, ids []string) ([]*jmapapi.Mailbox, string, []string, error) {
	d := s.data(account)
	if ids == nil {
		out := make([]*jmapapi.Mailbox, len(d.mailboxes))
		copy(out, d.mailboxes)
		return out, mailboxState, nil, nil
	}
	byID := make(map[string]*jmapapi.Mailbox, len(d.mailboxes))
	for _, mb := range d.mailboxes {
		byID[mb.ID] = mb
	}
	var out []*jmapapi.Mailbox
	var notFound []string
	for _, id := range ids {
		if mb, ok := byID[id]; ok {
			out = append(out, mb)
		} else {
			notFound = append(notFound, id)
		}
	}
	return out, mailboxState, notFound, nil
}

// EmailsByID implements jmapapi.Store.
func (s *Store) EmailsByID(_ context.Context, account string, ids []string) ([]*jmapapi.Email, string, []string, error) {
	d := s.data(account)
	if ids == nil {
		out := make([]*jmapapi.Email, len(d.emails))
		copy(out, d.emails)
		return out, emailState, nil, nil
	}
	byID := make(map[string]*jmapapi.Email, len(d.emails))
	for _, e := range d.emails {
		byID[e.ID] = e
	}
	var out []*jmapapi.Email
	var notFound []string
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		} else {
			notFound = append(notFound, id)
		}
	}
	return out, emailState, notFound, nil
}

// ThreadsByID implements jmapapi.Store: threads are grouped from the
// email fixtures, members oldest-first (RFC 8621 §3.1).
func (s *Store) ThreadsByID(_ context.Context, account string, ids []string) ([]*jmapapi.Thread, string, []string, error) {
	d := s.data(account)
	grouped := map[string][]*jmapapi.Email{}
	for _, e := range d.emails {
		grouped[e.ThreadID] = append(grouped[e.ThreadID], e)
	}
	for _, members := range grouped {
		sort.SliceStable(members, func(i, j int) bool {
			return members[i].ReceivedAt.Before(members[j].ReceivedAt)
		})
	}
	var out []*jmapapi.Thread
	var notFound []string
	for _, id := range ids {
		members, ok := grouped[id]
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		t := &jmapapi.Thread{ID: id, EmailIDs: make([]string, 0, len(members))}
		for _, e := range members {
			t.EmailIDs = append(t.EmailIDs, e.ID)
		}
		out = append(out, t)
	}
	return out, emailState, notFound, nil
}

// Changes implements jmapapi.Store: the fixture never changes, so a
// sinceState equal to the current state yields an empty change set and
// anything else cannot be replayed.
func (s *Store) Changes(_ context.Context, _, kind, sinceState string) (jmapapi.ChangeSet, error) {
	current := emailState
	if kind == "Mailbox" {
		current = mailboxState
	}
	if sinceState == current {
		return jmapapi.ChangeSet{NewState: current}, nil
	}
	return jmapapi.ChangeSet{}, jmapapi.ErrCannotCalculateChanges
}

// QueryEmails implements jmapapi.Store over the in-memory fixtures with
// the same token semantics as the reference mock server (PLAN §2.1):
// text/from/to/subject match whole tokens, AND across the needle.
func (s *Store) QueryEmails(_ context.Context, account string, q jmapapi.EmailQuery) ([]string, int, int, string, error) {
	d := s.data(account)

	cands := make([]*jmapapi.Email, 0, len(d.emails))
	for _, e := range d.emails {
		if q.Filter.InMailbox != "" && !containsString(e.MailboxIDs, q.Filter.InMailbox) {
			continue
		}
		if matchesSearch(e, q.Filter) {
			cands = append(cands, e)
		}
	}

	ascending := len(q.Sort) > 0 && q.Sort[0].Ascending
	prop := "receivedAt"
	if len(q.Sort) > 0 {
		prop = q.Sort[0].Property
	}
	sort.SliceStable(cands, func(i, j int) bool {
		less := emailLess(cands[i], cands[j], prop)
		if ascending {
			return less
		}
		return !less && emailLess(cands[j], cands[i], prop)
	})

	if q.CollapseThreads {
		seen := map[string]bool{}
		kept := cands[:0]
		for _, e := range cands {
			if seen[e.ThreadID] {
				continue
			}
			seen[e.ThreadID] = true
			kept = append(kept, e)
		}
		cands = kept
	}

	ids := make([]string, 0, len(cands))
	for _, e := range cands {
		ids = append(ids, e.ID)
	}
	total := len(ids)

	position := q.Position
	if q.Anchor != "" {
		position = len(ids)
		for i, id := range ids {
			if id == q.Anchor {
				position = i + q.AnchorOffset
				break
			}
		}
	}
	if position < 0 {
		position = 0
	}
	if position > len(ids) {
		position = len(ids)
	}
	end := len(ids)
	if q.Limit > 0 && position+q.Limit < end {
		end = position + q.Limit
	}
	window := ids[position:end]

	return window, position, total, emailState, nil
}

// emailLess orders two emails by one supported sort property.
func emailLess(a, b *jmapapi.Email, prop string) bool {
	switch prop {
	case "subject":
		return a.Subject < b.Subject
	case "from":
		return fromKey(a) < fromKey(b)
	case "size":
		return a.Size < b.Size
	case "hasAttachment":
		return !a.HasAttachment && b.HasAttachment
	default: // "receivedAt"
		return a.ReceivedAt.Before(b.ReceivedAt)
	}
}

func fromKey(e *jmapapi.Email) string {
	parts := make([]string, 0, len(e.From))
	for _, a := range e.From {
		parts = append(parts, strings.ToLower(a.Name+" "+a.Email))
	}
	return strings.Join(parts, ", ")
}

// matchesSearch applies the RFC 8621 §4.4.1 content conditions with
// whole-token semantics (a multi-word needle is an AND of its words).
func matchesSearch(e *jmapapi.Email, f jmapapi.EmailFilter) bool {
	if f.Text != "" {
		if !tokenMatch(tokenHaystack(subjectAndAddresses(e)...), f.Text) {
			return false
		}
	}
	if f.From != "" && !tokenMatch(tokenHaystack(addressesOf(e.From)...), f.From) {
		return false
	}
	if f.To != "" && !tokenMatch(tokenHaystack(addressesOf(e.To)...), f.To) {
		return false
	}
	if f.Subject != "" && !tokenMatch(tokenHaystack(e.Subject), f.Subject) {
		return false
	}
	if f.After != nil && !e.ReceivedAt.After(*f.After) {
		return false
	}
	if f.Before != nil && !e.ReceivedAt.Before(*f.Before) {
		return false
	}
	if f.HasKeyword != "" && !e.Keywords[f.HasKeyword] {
		return false
	}
	if f.HasAttachment != nil && *f.HasAttachment != e.HasAttachment {
		return false
	}
	return true
}

func subjectAndAddresses(e *jmapapi.Email) []string {
	parts := []string{e.Subject, e.Preview, e.TextBody, e.HTMLBody}
	parts = append(parts, addressesOf(e.From)...)
	parts = append(parts, addressesOf(e.To)...)
	return parts
}

func addressesOf(addrs []jmapapi.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Name+" "+a.Email)
	}
	return out
}

func tokenHaystack(parts ...string) map[string]bool {
	tokens := map[string]bool{}
	for _, p := range parts {
		for _, f := range strings.Fields(strings.ToLower(p)) {
			tokens[strings.Trim(f, ".,;:!?<>\"'")] = true
		}
	}
	return tokens
}

func tokenMatch(tokens map[string]bool, needle string) bool {
	for _, w := range strings.Fields(strings.ToLower(needle)) {
		if !tokens[w] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

var _ jmapapi.Store = (*Store)(nil)

// seed builds the deterministic sample mail for one account.
func seed() *accountData {
	mailboxes := []*jmapapi.Mailbox{
		{
			ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
		{
			ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 10,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
		{
			ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 20,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
		{
			ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 30,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
		{
			ID: "mb-archive-2026", ParentID: "mb-archive", Name: "2026", SortOrder: 31,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
		{
			ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 40,
			MayRead: true, MayAddItems: true, MayRemoveItems: true, MayCreateChild: true, MayRename: true, MayDelete: true,
		},
	}

	emails := []*jmapapi.Email{
		{
			ID: "em-welcome", ThreadID: "thr-welcome",
			MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "jmap-bridge", Email: "hello@example.test"}},
			To:         []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Welcome to jmap-bridge",
			ReceivedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
			Size:       2048, Preview: "Your JMAP bridge is running.",
			TextBody: "Your JMAP bridge is running.\n\nPoint a JMAP client at it and browse away.\n",
			HTMLBody: "<html><body><p>Your JMAP bridge is <b>running</b>.</p></body></html>",
		},
		{
			ID: "em-report", ThreadID: "thr-report",
			MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": true},
			MessageID:  []string{"<report-1@example.test>"},
			From:       []jmapapi.Address{{Name: "Alice Example", Email: "alice@example.test"}},
			To:         []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Quarterly report",
			ReceivedAt: time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC),
			Size:       4096, Preview: "The quarterly numbers are attached…",
			TextBody: "The quarterly numbers are attached in spirit — this fixture has no attachments yet.\n",
		},
		{
			ID: "em-report-reply", ThreadID: "thr-report",
			MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": false, "$flagged": true},
			MessageID:  []string{"<report-2@example.test>"},
			References: []string{"<report-1@example.test>"},
			InReplyTo:  []string{"<report-1@example.test>"},
			From:       []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			To:         []jmapapi.Address{{Name: "Alice Example", Email: "alice@example.test"}},
			Subject:    "Re: Quarterly report",
			ReceivedAt: time.Date(2026, 9, 3, 14, 15, 0, 0, time.UTC),
			Size:       1024, Preview: "Great work, let's discuss on Monday.",
			TextBody: "Great work, let's discuss on Monday.\n",
		},
		{
			ID: "em-meeting", ThreadID: "thr-meeting",
			MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": false},
			From:       []jmapapi.Address{{Name: "Bob Example", Email: "bob@example.test"}},
			To:         []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Meeting notes",
			ReceivedAt: time.Date(2026, 9, 4, 16, 45, 0, 0, time.UTC),
			Size:       3072, Preview: "Notes from today's stand-up…",
			TextBody: "Notes from today's stand-up:\n\n1. Bridge sync tiers\n2. Search backfill\n",
		},
		{
			ID: "em-newsletter", ThreadID: "thr-newsletter",
			MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "Example Weekly", Email: "newsletter@example.test"}},
			To:         []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "This week in IMAP",
			ReceivedAt: time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC),
			Size:       8192, Preview: "CONDSTORE turns twenty…",
			TextBody: "CONDSTORE turns twenty, and other headlines.\n",
		},
		{
			ID: "em-sent", ThreadID: "thr-lunch",
			MailboxIDs: []string{"mb-sent"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			To:         []jmapapi.Address{{Name: "Alice Example", Email: "alice@example.test"}},
			Subject:    "Lunch on Friday",
			ReceivedAt: time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC),
			Size:       768, Preview: "Same place as last time?",
			TextBody: "Same place as last time?\n",
		},
		{
			ID: "em-draft", ThreadID: "thr-ideas",
			MailboxIDs: []string{"mb-drafts"},
			Keywords:   map[string]bool{"$draft": true, "$seen": false},
			From:       []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Draft: project ideas",
			ReceivedAt: time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC),
			Size:       512, Preview: "1. Bridge for calendars…",
			TextBody: "1. Bridge for calendars\n2. Bridge for tasks\n",
		},
		{
			ID: "em-invoice", ThreadID: "thr-invoice",
			MailboxIDs: []string{"mb-archive"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "Billing", Email: "billing@example.test"}},
			To:         []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Invoice 2026-0042",
			ReceivedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			Size:       1536, Preview: "Your invoice is ready.",
			TextBody: "Your invoice is ready.\n",
		},
		{
			ID: "em-oldnote", ThreadID: "thr-oldnote",
			MailboxIDs: []string{"mb-archive-2026"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "Me", Email: "me@example.test"}},
			Subject:    "Old note",
			ReceivedAt: time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC),
			Size:       256, Preview: "Remember to water the plants.",
			TextBody: "Remember to water the plants.\n",
		},
		{
			ID: "em-junk", ThreadID: "thr-junk",
			MailboxIDs: []string{"mb-trash"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []jmapapi.Address{{Name: "Deals", Email: "deals@example.test"}},
			Subject:    "Subscription offer",
			ReceivedAt: time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC),
			Size:       6144, Preview: "Exclusive deal inside!",
			TextBody: "Exclusive deal inside!\n",
		},
	}

	// Recompute the denormalised mailbox counts from the membership, so
	// the numbers clients see always match the data.
	for _, mb := range mailboxes {
		mb.TotalEmails, mb.UnreadEmails, mb.TotalThreads, mb.UnreadThreads = countsFor(mailboxes, emails, mb.ID)
	}
	return &accountData{mailboxes: mailboxes, emails: emails}
}

// countsFor derives the four mailbox counters from actual membership.
func countsFor(mailboxes []*jmapapi.Mailbox, emails []*jmapapi.Email, id string) (total, unread, threads, unreadThreads int) {
	seen := map[string]bool{}
	unreadSeen := map[string]bool{}
	for _, e := range emails {
		if !containsString(e.MailboxIDs, id) {
			continue
		}
		total++
		seen[e.ThreadID] = true
		if !e.Keywords["$seen"] {
			unread++
			unreadSeen[e.ThreadID] = true
		}
	}
	// A child folder's mail does not count toward its parent (JMAP
	// counts are exact membership, not rollups).
	return total, unread, len(seen), len(unreadSeen)
}

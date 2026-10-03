package jmapapi_test

// Fakes for the contacts tests: the Store seam answers deterministic
// books/cards, and the Backend seam records mutations so the tests can
// assert dispatch semantics (patch shapes, error mapping, state strings)
// without a DAV server. The protocol behaviour of the real backend is
// covered in internal/sync's tests.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

type fakeStore struct {
	mu    sync.Mutex
	books map[string]*jmapapi.AddressBook
	cards map[string]*jmapapi.ContactCard
	blobs map[string]struct {
		data  []byte
		media string
	}
	bookSt  string
	cardSt  string
	changes map[string]jmapapi.ChangeSet
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		books: map[string]*jmapapi.AddressBook{},
		cards: map[string]*jmapapi.ContactCard{},
		blobs: map[string]struct {
			data  []byte
			media string
		}{},
		bookSt:  "1",
		cardSt:  "1",
		changes: map[string]jmapapi.ChangeSet{},
	}
}

func (*fakeStore) Mailboxes(context.Context, string) ([]*jmapapi.Mailbox, string, error) {
	return nil, "1", nil
}

func (*fakeStore) MailboxesByID(context.Context, string, []string) ([]*jmapapi.Mailbox, string, []string, error) {
	return nil, "1", nil, nil
}

func (*fakeStore) QueryEmails(context.Context, string, jmapapi.EmailQuery) ([]string, int, int, string, error) {
	return nil, 0, 0, "1", nil
}

func (*fakeStore) EmailsByID(context.Context, string, []string, bool) ([]*jmapapi.Email, string, []string, error) {
	return nil, "1", nil, nil
}

func (*fakeStore) ThreadsByID(context.Context, string, []string) ([]*jmapapi.Thread, string, []string, error) {
	return nil, "1", nil, nil
}

func (f *fakeStore) States(_ context.Context, _ string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]string{
		"Mailbox": "1", "Email": "1", "Thread": "1",
		"AddressBook": f.bookSt, "ContactCard": f.cardSt,
	}, nil
}

func (f *fakeStore) Changes(_ context.Context, _, kind, since string) (jmapapi.ChangeSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs, ok := f.changes[kind]
	if !ok {
		return jmapapi.ChangeSet{}, jmapapi.ErrCannotCalculateChanges
	}
	return cs, nil
}

func (f *fakeStore) PutBlob(_ context.Context, _, _ string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("blob%d", len(f.blobs)+1)
	f.blobs[id] = struct {
		data  []byte
		media string
	}{data, "application/octet-stream"}
	return id, nil
}

func (f *fakeStore) ReadBlob(_ context.Context, _, id string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[id]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", jmapapi.ErrBlobNotFound, id)
	}
	return b.data, b.media, nil
}

func (f *fakeStore) AddressBooksByID(_ context.Context, _ string, ids []string) ([]*jmapapi.AddressBook, string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*jmapapi.AddressBook
	var nf []string
	if ids == nil {
		for _, b := range f.books {
			out = append(out, b)
		}
	}
	for _, id := range ids {
		if b, ok := f.books[id]; ok {
			out = append(out, b)
		} else {
			nf = append(nf, id)
		}
	}
	return out, f.bookSt, nf, nil
}

func (f *fakeStore) CardsByID(_ context.Context, _ string, ids []string) ([]*jmapapi.ContactCard, string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*jmapapi.ContactCard
	var nf []string
	if ids == nil {
		for _, c := range f.cards {
			out = append(out, c)
		}
	}
	for _, id := range ids {
		if c, ok := f.cards[id]; ok {
			out = append(out, c)
		} else {
			nf = append(nf, id)
		}
	}
	return out, f.cardSt, nf, nil
}

func (f *fakeStore) addBook(b *jmapapi.AddressBook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.books[b.ID] = b
}

func (f *fakeStore) addCard(c *jmapapi.ContactCard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards[c.ID] = c
}

func (f *fakeStore) addBlob(id string, data []byte, media string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[id] = struct {
		data  []byte
		media string
	}{data, media}
}

// fakeBackend records contact mutations.
type fakeBackend struct {
	created    []jmapapi.ContactSpec
	updated    map[string]jmapapi.ContactSpec
	destroyed  []string
	existsUIDs map[string]bool // uid → collision

	err      error           // force a failure on the next mutation
	knownIDs map[string]bool // when set: unknown ids fail with notFound

	importErr  error                 // force ImportEmail to fail
	importDone *jmapapi.CreatedEmail // when set: the ImportEmail result
	imported   []jmapapi.ImportSpec  // every spec ImportEmail was handed
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		updated:    map[string]jmapapi.ContactSpec{},
		existsUIDs: map[string]bool{},
	}
}

func (*fakeBackend) ApplyEmailPatch(context.Context, string, string, jmapapi.EmailPatch) error {
	return nil
}
func (*fakeBackend) DestroyEmails(context.Context, string, string) error { return nil }
func (*fakeBackend) CreateDraft(context.Context, string, jmapapi.DraftSpec) (*jmapapi.CreatedEmail, error) {
	return nil, nil
}

func (b *fakeBackend) ImportEmail(_ context.Context, _ string, spec jmapapi.ImportSpec) (*jmapapi.CreatedEmail, error) {
	if b.importErr != nil {
		return nil, b.importErr
	}
	b.imported = append(b.imported, spec)
	if b.importDone != nil {
		return b.importDone, nil
	}
	return &jmapapi.CreatedEmail{
		ID: "em-import", BlobID: spec.BlobID, ThreadID: "th-import", Size: 128,
	}, nil
}

func (*fakeBackend) SubmitEmail(context.Context, string, jmapapi.SubmissionSpec) (*jmapapi.CreatedSubmission, error) {
	return nil, nil
}

func (*fakeBackend) CreateMailbox(context.Context, string, string, string, int) (string, error) {
	return "", nil
}

func (*fakeBackend) RenameMailbox(context.Context, string, string, string, string) error {
	return nil
}
func (*fakeBackend) DestroyMailbox(context.Context, string, string, bool) error { return nil }

func (b *fakeBackend) CreateContact(_ context.Context, _ string, spec jmapapi.ContactSpec) (string, error) {
	if b.err != nil {
		return "", b.err
	}
	var c struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(spec.Content, &c)
	if c.UID != "" && b.existsUIDs[c.UID] {
		return "", jmapapi.ErrContactExists
	}
	b.created = append(b.created, spec)
	if c.UID == "" {
		c.UID = "gen-1"
	}
	return c.UID, nil
}

func (b *fakeBackend) UpdateContact(_ context.Context, _, id string, spec jmapapi.ContactSpec) error {
	if b.err != nil {
		return b.err
	}
	b.updated[id] = spec
	return nil
}

func (b *fakeBackend) DestroyContact(_ context.Context, _, id string) error {
	if b.err != nil {
		return b.err
	}
	if b.knownIDs != nil && !b.knownIDs[id] {
		return jmapapi.ErrObjectNotFound
	}
	b.destroyed = append(b.destroyed, id)
	return nil
}

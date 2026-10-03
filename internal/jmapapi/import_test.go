package jmapapi_test

// Email/import method-surface tests (RFC 8621 §4.8): dispatch shapes,
// argument validation and backend error mapping against the fake seams.

import (
	"errors"
	"testing"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

var importCaps = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}

func importCall(t *testing.T, st *fakeStore, be *fakeBackend, args map[string]any) []callResult {
	t.Helper()
	_, results, _ := dispatchContacts(t, st, be, importCaps,
		[]any{"Email/import", args, "c1"})
	return results
}

func TestEmailImportSuccess(t *testing.T) {
	st := newFakeStore()
	be := newFakeBackend()
	results := importCall(t, st, be, map[string]any{
		"accountId": "acct1",
		"emails": map[string]any{
			"i1": map[string]any{
				"blobId":     "blob-1",
				"mailboxIds": map[string]bool{"mb1": true},
				"keywords":   map[string]bool{"$seen": true},
				"receivedAt": "2026-09-02T09:30:00Z",
			},
		},
	})
	if len(results) != 1 || results[0].Name != "Email/import" {
		t.Fatalf("results = %+v", results)
	}
	args := results[0].Args
	if args["accountId"] != "acct1" || args["oldState"] != "1" || args["newState"] != "1" {
		t.Errorf("identity/state = %v", args)
	}
	created, _ := args["created"].(map[string]any)
	entry, _ := created["i1"].(map[string]any)
	if entry == nil {
		t.Fatalf("created = %v", args["created"])
	}
	if entry["id"] != "em-import" || entry["blobId"] != "blob-1" ||
		entry["threadId"] != "th-import" || entry["size"] != float64(128) {
		t.Errorf("created entry = %v", entry)
	}
	if args["notCreated"] != nil {
		t.Errorf("notCreated = %v, want null", args["notCreated"])
	}
	if len(be.imported) != 1 || be.imported[0].BlobID != "blob-1" || !be.imported[0].Keywords["$seen"] {
		t.Errorf("backend spec = %+v", be.imported)
	}
}

func TestEmailImportArgumentValidation(t *testing.T) {
	st := newFakeStore()
	be := newFakeBackend()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing blobId", map[string]any{
			"accountId": "acct1",
			"emails":    map[string]any{"i1": map[string]any{"mailboxIds": map[string]bool{"mb1": true}}},
		}},
		{"empty mailboxIds", map[string]any{
			"accountId": "acct1",
			"emails":    map[string]any{"i1": map[string]any{"blobId": "b", "mailboxIds": map[string]bool{}}},
		}},
		{"false keyword", map[string]any{
			"accountId": "acct1",
			"emails": map[string]any{"i1": map[string]any{
				"blobId": "b", "mailboxIds": map[string]bool{"mb1": true},
				"keywords": map[string]bool{"$seen": false},
			}},
		}},
		{"no emails", map[string]any{"accountId": "acct1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := importCall(t, st, be, tc.args)
			if len(results) != 1 || results[0].Name != "error" {
				t.Fatalf("results = %+v, want an error invocation", results)
			}
			if results[0].Args["type"] != "invalidArguments" {
				t.Errorf("error = %v, want invalidArguments", results[0].Args)
			}
		})
	}
	if len(be.imported) != 0 {
		t.Errorf("backend called for malformed imports: %+v", be.imported)
	}
}

func TestEmailImportBackendErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"blob not found", jmapapi.ErrBlobNotFound, "invalidProperties"},
		{"unknown mailbox", jmapapi.ErrUnknownMailbox, "invalidProperties"},
		{"server fail", errors.New("boom"), "serverFail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			be := newFakeBackend()
			be.importErr = tc.err
			results := importCall(t, st, be, map[string]any{
				"accountId": "acct1",
				"emails": map[string]any{"i1": map[string]any{
					"blobId": "b", "mailboxIds": map[string]bool{"mb1": true},
				}},
			})
			notCreated, _ := results[0].Args["notCreated"].(map[string]any)
			entry, _ := notCreated["i1"].(map[string]any)
			if entry == nil || entry["type"] != tc.want {
				t.Errorf("notCreated = %v, want %s", results[0].Args["notCreated"], tc.want)
			}
			if results[0].Args["created"] != nil {
				t.Errorf("created = %v, want null", results[0].Args["created"])
			}
		})
	}
}

func TestEmailImportIfInStateMismatch(t *testing.T) {
	st := newFakeStore()
	be := newFakeBackend()
	results := importCall(t, st, be, map[string]any{
		"accountId": "acct1",
		"ifInState": "999",
		"emails": map[string]any{"i1": map[string]any{
			"blobId": "b", "mailboxIds": map[string]bool{"mb1": true},
		}},
	})
	if len(results) != 1 || results[0].Name != "error" || results[0].Args["type"] != "stateMismatch" {
		t.Fatalf("results = %+v, want stateMismatch", results)
	}
	if len(be.imported) != 0 {
		t.Errorf("backend called despite stateMismatch: %+v", be.imported)
	}
}

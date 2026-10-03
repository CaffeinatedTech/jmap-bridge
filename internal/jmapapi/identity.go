package jmapapi

import (
	"context"
	"crypto/sha1" //nolint:gosec // id and state derivation, not security
	"encoding/hex"
	"encoding/json"
)

// SubmissionURN is the capability Identity and EmailSubmission live
// under (RFC 8621 §1.3.2). Both methods are served only for an account
// that offers it (FR-M.14, FR-J.5), and it is the session resource's
// job to advertise exactly the same URN (FR-J.5) — one constant, so the
// two can never disagree.
const SubmissionURN = "urn:ietf:params:jmap:submission"

// Identity is one address an account may send from (RFC 8621 §6). The
// bridge builds it from configuration — accounts.address plus the
// account's display name — so it is stable across restarts; Identity/set
// is roadmap (PLAN §15) and mayDelete stays false.
type Identity struct {
	ID    string
	Name  string
	Email string
}

// IdentityID derives the stable id of an account's identity from the
// account id: the same account answers the same id on every start
// (FR-M.14) without spending a row — there is exactly one identity per
// account and nothing to synchronise.
func IdentityID(account string) string {
	sum := sha1.Sum([]byte("jmap-bridge:identity:" + account)) //nolint:gosec // non-security id
	return "I" + hex.EncodeToString(sum[:8])
}

// identityState is the Identity type state: a hash of the identity's
// values, so a configuration change moves it and nothing else does.
// Identity/changes is roadmap, so there is no counter to spend.
func identityState(id *Identity) string {
	h := sha1.New() //nolint:gosec // non-security state string
	_, _ = h.Write([]byte(id.ID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id.Name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id.Email))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (id *Identity) object() map[string]any {
	return map[string]any{
		"id":        id.ID,
		"name":      id.Name,
		"email":     id.Email,
		"mayDelete": false,
	}
}

// --- Identity/get (FR-M.14, RFC 8621 §6.1) ---

// identityGet answers Identity/get with the account's single identity.
// The method exists only where submission is configured: an account the
// session did not advertise submission for does not support this data
// type, which is exactly accountNotSupportedByMethod (RFC 8620 §9.5.3).
func (h *Handler) identityGet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	if acct.Identity == nil || !accountOffers(acct, SubmissionURN) {
		return nil, methodErrorf("accountNotSupportedByMethod",
			"this account has no sendable identity (submission is not configured)")
	}
	id := acct.Identity
	list := []map[string]any{filterProps(id.object(), args.Properties)}
	notFound := []string{}
	if args.IDs != nil && !containsString(*args.IDs, id.ID) {
		// ids was given and does not name this identity: the list is
		// empty and the requested id is reported as not found.
		list = []map[string]any{}
		notFound = *args.IDs
	}
	return map[string]any{
		"accountId": acct.ID,
		"state":     identityState(id),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

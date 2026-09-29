package jmapapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"sort"
	"strings"
	"time"
)

// This file is EmailSubmission/set (RFC 8621 §7.5, FR-M.15, PLAN §7.2):
// the arguments are validated before a single byte leaves for SMTP,
// every create that survives submits over SMTP, and the onSuccess*
// effects are performed afterwards by one implicit Email/set whose
// response follows this one — because SMTP cannot be undone, a patch
// must never run first.

// submissionState is the EmailSubmission type state. The bridge does not
// keep submissions (RFC 8621 §7 lets a server destroy them the moment a
// message is relayed; EmailSubmission/get|query are roadmap, PLAN §15),
// so nothing can move this state — and a constant says exactly that
// rather than pretending there is a change log.
const submissionState = "0"

type submissionSetArgs struct {
	AccountID             string                     `json:"accountId"`
	IfInState             *string                    `json:"ifInState"`
	Create                map[string]json.RawMessage `json:"create"`
	OnSuccessUpdateEmail  map[string]json.RawMessage `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail *[]string                  `json:"onSuccessDestroyEmail"`
}

// submissionCreate is one create member as the bridge accepts it. id,
// threadId and sendAt are server-set (and v0.1 has no delayed send at
// all), so a client that sends them is told so instead of having them
// silently rewritten.
type submissionCreate struct {
	ID         *string           `json:"id"`
	ThreadID   *string           `json:"threadId"`
	SendAt     *time.Time        `json:"sendAt"`
	IdentityID string            `json:"identityId"`
	EmailID    string            `json:"emailId"`
	Envelope   *submissionEnvArg `json:"envelope"`
}

type submissionEnvArg struct {
	MailFrom *struct {
		Email string `json:"email"`
	} `json:"mailFrom"`
	RcptTo []*struct {
		Email string `json:"email"`
	} `json:"rcptTo"`
}

func (h *Handler) emailSubmissionSet(ctx context.Context, acct *Account, raw json.RawMessage) (any, *methodErr) {
	var args submissionSetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, methodErrorf("invalidArguments", "%s", err)
	}
	if merr := checkAccount(acct, args.AccountID); merr != nil {
		return nil, merr
	}
	if acct.Identity == nil || !accountOffers(acct, SubmissionURN) {
		return nil, methodErrorf("accountNotSupportedByMethod",
			"this account cannot send (submission is not configured)")
	}
	if args.IfInState != nil && *args.IfInState != submissionState {
		return nil, methodErrorf("stateMismatch",
			"ifInState %s does not match the current EmailSubmission state", *args.IfInState)
	}
	if len(args.Create) > maxObjectsInSet {
		return nil, methodErrorf("requestTooLarge",
			"create is %d, maxObjectsInSet is %d", len(args.Create), maxObjectsInSet)
	}
	// Destroying a submission is meaningless here: the bridge relays in
	// line and keeps no record, and jmap-tui's own fallback for "no Sent
	// mailbox" is a client-side Email/set destroy (PLAN §2.1). Refusing
	// is what Fastmail does too — an unsupported argument, not a silent
	// drop (FR-M.15).
	if args.OnSuccessDestroyEmail != nil && len(*args.OnSuccessDestroyEmail) > 0 {
		return nil, &methodErr{
			Type:        "invalidProperties",
			Description: "onSuccessDestroyEmail is not supported; retire the draft with Email/set instead",
			Properties:  []string{"onSuccessDestroyEmail"},
		}
	}
	if acct.Backend == nil {
		return nil, serverFail(ErrNoWriteBackend)
	}

	// Patches are parsed first: a malformed onSuccessUpdateEmail must
	// fail the whole request before any message is submitted.
	type pending struct {
		handle  string
		emailID string
		spec    SubmissionSpec
	}
	handles := sortedKeys(args.Create)
	patches := make(map[string]EmailPatch, len(args.OnSuccessUpdateEmail))
	for key, val := range args.OnSuccessUpdateEmail {
		handle := strings.TrimPrefix(key, "#")
		if handle == "" || !containsString(handles, handle) {
			return nil, methodErrorf("invalidArguments",
				"onSuccessUpdateEmail key %q does not name a submission created in this request", key)
		}
		patch, merr := parseEmailPatch(val)
		if merr != nil {
			return nil, merr
		}
		patches[handle] = patch
	}

	old := submissionState
	resp := &SetResponse{AccountID: acct.ID, OldState: &old, NewState: old}

	var ok []pending
	for _, handle := range handles {
		var c submissionCreate
		if err := json.Unmarshal(args.Create[handle], &c); err != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = methodErrorf("invalidArguments", "%s", err).set()
			continue
		}
		spec, merr := prepareSubmission(ctx, acct, &c, patches[handle])
		if merr != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[handle] = merr.set()
			continue
		}
		ok = append(ok, pending{handle: handle, emailID: spec.EmailID, spec: spec})
	}

	// Submit: server-first (golden rule 1) — the Sent copy and the
	// patches only exist for messages SMTP actually accepted.
	for _, p := range ok {
		created, err := acct.Backend.SubmitEmail(ctx, acct.ID, p.spec)
		if err != nil {
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[p.handle] = submissionErr(err)
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[p.handle] = created
	}

	// RFC 8621 §7.5: after every create has been processed, one implicit
	// Email/set performs the onSuccessUpdateEmail effects, and its
	// response follows this one.
	var applied []appliedPatch
	for _, p := range ok {
		if resp.Created[p.handle] == nil {
			continue // the create failed; onSuccess* does not apply
		}
		patch, has := patches[p.handle]
		if !has || patch.Empty() {
			continue
		}
		applied = append(applied, appliedPatch{emailID: p.emailID, patch: patch})
	}
	callID := ""
	if scope := callScopeOf(ctx); scope != nil {
		callID = scope.callID
	}
	if len(applied) == 0 {
		return resp, nil
	}
	implicit := h.implicitEmailSet(ctx, acct, applied)
	return &callResult{Value: resp, Followup: &invocation{
		Name: "Email/set", Args: implicit, CallID: callID,
	}}, nil
}

// appliedPatch is one onSuccessUpdateEmail entry that is due to run.
type appliedPatch struct {
	emailID string
	patch   EmailPatch
}

// prepareSubmission validates one create member and hands the backend a
// spec. Everything checked here is checked before SMTP runs, so a bad
// argument can never leave a half-sent message behind.
func prepareSubmission(ctx context.Context, acct *Account, c *submissionCreate, patch EmailPatch) (SubmissionSpec, *methodErr) {
	switch {
	case c.ID != nil:
		return SubmissionSpec{}, invalidProps("id", "id is server-set")
	case c.ThreadID != nil:
		return SubmissionSpec{}, invalidProps("threadId", "threadId is server-set")
	case c.SendAt != nil:
		return SubmissionSpec{}, invalidProps("sendAt",
			"delayed send is not supported (maxDelayedSend is 0)")
	case strings.TrimSpace(c.IdentityID) == "":
		return SubmissionSpec{}, invalidProps("identityId", "identityId is required")
	case c.IdentityID != acct.Identity.ID:
		return SubmissionSpec{}, invalidProps("identityId",
			"no such identity in this account")
	case strings.TrimSpace(c.EmailID) == "":
		return SubmissionSpec{}, invalidProps("emailId", "emailId is required")
	}
	emailID := c.EmailID
	if strings.HasPrefix(emailID, "#") {
		// Creation reference: the draft this same batch just created
		// (RFC 8620 §3.7's form as EmailSubmission/set spells it).
		resolved, ok := resolveCreationRef(ctx, emailID)
		if !ok {
			return SubmissionSpec{}, invalidProps("emailId",
				"emailId refers to a creation that did not happen in this request")
		}
		emailID = resolved
	}
	spec := SubmissionSpec{
		EmailID:    emailID,
		IdentityID: c.IdentityID,
		From:       acct.Identity.Email,
		Patch:      patch,
	}
	if c.Envelope != nil {
		env := &SubmissionEnvelope{}
		if c.Envelope.MailFrom != nil && c.Envelope.MailFrom.Email != "" {
			addr, err := mail.ParseAddress(c.Envelope.MailFrom.Email)
			if err != nil {
				return SubmissionSpec{}, invalidProps("envelope.mailFrom",
					"envelope.mailFrom is not a valid email address")
			}
			env.MailFrom = addr.Address
		}
		var invalid []string
		for _, rcpt := range c.Envelope.RcptTo {
			if rcpt == nil || strings.TrimSpace(rcpt.Email) == "" {
				continue
			}
			addr, err := mail.ParseAddress(rcpt.Email)
			if err != nil {
				invalid = append(invalid, rcpt.Email)
				continue
			}
			env.RcptTo = append(env.RcptTo, addr.Address)
		}
		// RFC 8621 §7.5 wants every offending address, not just the
		// first one the parser tripped over.
		if len(invalid) > 0 {
			return SubmissionSpec{}, &methodErr{
				Type:              "invalidRecipients",
				Description:       "envelope.rcptTo contains addresses that are not valid for sending",
				InvalidRecipients: invalid,
			}
		}
		// An envelope with no recipients means "use the message's" —
		// no client means to send to nobody; that case is the headers'
		// (and only then noRecipients, from the backend).
		if len(env.RcptTo) == 0 {
			env.RcptTo = nil
		}
		spec.Envelope = env
	}
	return spec, nil
}

// implicitEmailSet performs the onSuccessUpdateEmail patches and builds
// the response RFC 8621 §7.5 requires to follow the submission's. A
// state it cannot read is reported as unknown (oldState null) rather
// than as a guess — the client refetches when it cares.
func (h *Handler) implicitEmailSet(ctx context.Context, acct *Account, applied []appliedPatch) *SetResponse {
	resp := &SetResponse{AccountID: acct.ID}
	if states, err := acct.Store.States(ctx, acct.ID); err == nil {
		old := states["Email"]
		resp.OldState = &old
		resp.NewState = old
	}
	for _, p := range applied {
		if err := acct.Backend.ApplyEmailPatch(ctx, acct.ID, p.emailID, p.patch); err != nil {
			if resp.NotUpdated == nil {
				resp.NotUpdated = map[string]SetError{}
			}
			resp.NotUpdated[p.emailID] = setErrFor(err, patchErrorProperty(p.patch))
			continue
		}
		resp.Updated = append(resp.Updated, p.emailID)
	}
	// Read-your-writes: the state this response ends with includes the
	// patches it just reported (FR-M.13).
	if states, err := acct.Store.States(ctx, acct.ID); err == nil {
		resp.NewState = states["Email"]
	}
	return resp
}

// submissionErr maps a backend failure onto the SetError RFC 8621 §7.5
// defines for it. An SMTP refusal carries the server's own reply, and
// means the message was not accepted — no Sent copy exists (FR-M.15).
func submissionErr(err error) SetError {
	var inv *InvalidRecipientsError
	var smtp *SMTPError
	switch {
	case errors.Is(err, ErrNoRecipients):
		return NewSetError("noRecipients", nil, "")
	case errors.As(err, &inv):
		return SetError{
			Type:              "invalidRecipients",
			Description:       optStr("envelope rcptTo contains addresses the server will not send to"),
			InvalidRecipients: inv.Addresses,
		}
	case errors.As(err, &smtp):
		return NewSetError("serverFail", nil, smtp.Reply)
	case errors.Is(err, ErrObjectNotFound):
		return NewSetError("invalidProperties", []string{"emailId"},
			"emailId does not name an Email in this account")
	case errors.Is(err, ErrNoSubmissionBackend):
		return NewSetError("serverFail", nil, ErrNoSubmissionBackend.Error())
	case errors.Is(err, ErrNoWriteBackend):
		return NewSetError("serverFail", nil, ErrNoWriteBackend.Error())
	default:
		return NewSetError("serverFail", nil, err.Error())
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

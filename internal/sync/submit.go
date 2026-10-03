package sync

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
	mb "github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
	"github.com/CaffeinatedTech/jmap-bridge/internal/store"
	"github.com/CaffeinatedTech/jmap-bridge/internal/submit"
)

// This file is the send half of the write path (PLAN §7.2, FR-M.15):
// SMTP first, the Sent copy second, and the caller's patches last —
// they are performed by the implicit Email/set that follows, so nothing
// is filed for a message the submission server did not accept (golden
// rule 1).

// SubmitEmail relays one EmailSubmission/create over SMTP and then files
// the sent message. Filing has exactly two shapes: the caller's
// onSuccessUpdateEmail patch moves the draft into Sent itself (what
// every composing client sends), in which case APPending a second copy
// would leave Sent holding the message twice — or, when the client said
// nothing about where the message belongs, the bridge APPENDs the bytes
// it just sent to the Sent mailbox, because a sent message with no
// record anywhere is worse than one filed by the server.
func (e *Engine) SubmitEmail(ctx context.Context, account string, spec jmapapi.SubmissionSpec) (*jmapapi.CreatedSubmission, error) {
	if account != e.cfg.Account {
		return nil, fmt.Errorf("sync: submit for foreign account %q", account)
	}
	emails, _, notFound, err := e.st.EmailsByID(ctx, account, []string{spec.EmailID}, false)
	if err != nil {
		return nil, err
	}
	if len(notFound) > 0 || len(emails) == 0 {
		return nil, jmapapi.ErrObjectNotFound
	}
	raw, err := e.rawForSubmission(ctx, account, spec.EmailID)
	if err != nil {
		return nil, err
	}
	env, err := submissionEnvelope(spec, emails[0])
	if err != nil {
		return nil, err
	}
	if e.cfg.SMTP == nil {
		// API mode has no SMTP block (config validation rejects it); the
		// backend submits over its own provider API (GMAIL_API_PLAN §8.1).
		return e.submitViaProvider(ctx, account, spec, raw, env)
	}
	return e.submitViaSMTP(ctx, account, spec, raw, env)
}

// submitViaSMTP is the IMAP-path submission: the message is relayed over
// SMTP and the Sent copy is filed afterwards (PLAN §7.2).
func (e *Engine) submitViaSMTP(ctx context.Context, account string, spec jmapapi.SubmissionSpec, raw []byte, env submit.Envelope) (*jmapapi.CreatedSubmission, error) {
	// The id is minted before the send: after SMTP has accepted the
	// message there is no error left that the caller may be told about,
	// because "not created" would make it send the message twice.
	id, err := e.st.MintID(ctx)
	if err != nil {
		return nil, err
	}

	// Gmail silently declines to relay an SMTP submission whose
	// Message-ID already exists in the mailbox (it answers 250 and files
	// the message as a draft-like Sent item), and the bridge APPENDed
	// this very message as the draft. Removing that upstream draft first
	// makes the submission a genuinely new message, which Gmail relays
	// and auto-saves to Sent. A failed send restores the draft, so an
	// error never costs the client its draft.
	var draft *store.Copy
	if e.wr.gmail() {
		draft, err = e.gmailDraftCopy(ctx, account, spec.EmailID)
		if err != nil {
			return nil, err
		}
		if draft != nil {
			if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
				_, e2 := b.Destroy(ctx, []mb.Ref{mb.NewRef(draft.Folder, draft.UIDValidity, draft.UID)}, "")
				return e2
			}); err != nil {
				return nil, err
			}
		}
	}

	// Bcc travels to the envelope but never on the wire (RFC 8621 §7.5):
	// the stored copy keeps its Bcc header, the delivered one does not.
	if err := submit.Send(ctx, *e.cfg.SMTP, env, convert.StripBcc(raw)); err != nil {
		e.restoreGmailDraft(ctx, account, spec.EmailID, draft, raw)
		var rejected *submit.RejectedError
		if errors.As(err, &rejected) {
			return nil, &jmapapi.SMTPError{Reply: rejected.Reply}
		}
		return nil, err
	}

	// The message exists out there; every failure below is logged, never
	// reported as a failed create. Gmail's own SMTP already files the
	// sent copy in Sent, so only servers without that behaviour get the
	// bridge's APPEND.
	if !spec.FilesItself() && !e.wr.gmail() {
		if err := e.fileSentCopy(ctx, account, spec.EmailID, raw); err != nil {
			e.log.Warn("sync: sent copy not filed",
				"email", spec.EmailID, "err", err)
		}
	}
	e.log.Debug("sync: submitted",
		"email", spec.EmailID, "submission", id, "rcpt", len(env.Recipients),
		"filedByPatch", spec.FilesItself())
	return &jmapapi.CreatedSubmission{
		ID:         id,
		EmailID:    spec.EmailID,
		IdentityID: spec.IdentityID,
		UndoStatus: "final", // relayed already: there is nothing to undo (FR-M.15)
	}, nil
}

// submitViaProvider relays the message through the backend's own provider
// API (Gmail API, GMAIL_API_PLAN §8.1). Gmail files its own Sent copy, so
// the bridge never APPENDs one; it records the accepted message in the
// cache so a read right after the submission sees it (FR-M.13) instead of
// waiting for the next sync pass. The submission id is minted before the
// send for the same reason as the SMTP path: once Gmail has it, no error
// may be reported to the caller.
func (e *Engine) submitViaProvider(ctx context.Context, account string, spec jmapapi.SubmissionSpec, raw []byte, env submit.Envelope) (*jmapapi.CreatedSubmission, error) {
	id, err := e.st.MintID(ctx)
	if err != nil {
		return nil, err
	}
	copies, err := e.st.EmailCopies(ctx, account, spec.EmailID)
	if err != nil {
		return nil, err
	}
	msgid, err := e.st.EmailMessageID(ctx, account, spec.EmailID)
	if err != nil {
		return nil, err
	}
	var res mb.SendResult
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		s, ok := b.(mb.Sender)
		if !ok {
			return jmapapi.ErrNoSubmissionBackend
		}
		var e2 error
		res, e2 = s.Send(ctx, mb.SendRequest{
			Raw:        convert.StripBcc(raw),
			MessageID:  msgid,
			From:       env.From,
			Recipients: env.Recipients,
			Copies:     backendCopies(copies),
		})
		return e2
	}); err != nil {
		return nil, err
	}
	// The message exists out there; a recording failure is logged, never
	// reported as a failed create (the client would send twice).
	if err := e.recordProviderSent(ctx, account, spec.EmailID, res, raw); err != nil {
		e.log.Warn("sync: provider sent copy not recorded",
			"email", spec.EmailID, "err", err)
	}
	e.log.Debug("sync: submitted via provider",
		"email", spec.EmailID, "submission", id, "rcpt", len(env.Recipients),
		"draftConsumed", res.DraftConsumed)
	return &jmapapi.CreatedSubmission{
		ID:         id,
		EmailID:    spec.EmailID,
		IdentityID: spec.IdentityID,
		UndoStatus: "final",
	}, nil
}

// recordProviderSent mirrors an accepted provider send into the cache. A
// consumed draft keeps its identity and only changes membership; a fresh
// message becomes a new Sent record sharing the submitted bytes.
func (e *Engine) recordProviderSent(ctx context.Context, account, emailID string, res mb.SendResult, raw []byte) error {
	if res.DraftConsumed {
		return e.moveDraftToSent(ctx, account, emailID, res)
	}
	return e.commitSentCopy(ctx, account, emailID, res, raw)
}

// moveDraftToSent retires the local draft and files it in Sent, matching
// what Gmail did server-side. It is a no-op when the message is already in
// Sent (the caller's patch usually got there first).
func (e *Engine) moveDraftToSent(ctx context.Context, account, emailID string, res mb.SendResult) error {
	sentID, err := e.st.MailboxIDByRole(ctx, account, "sent")
	if err != nil {
		return err
	}
	if sentID == "" {
		return errors.New(`sync: account has no mailbox with the "sent" role`)
	}
	draftID, err := e.st.MailboxIDByRole(ctx, account, "drafts")
	if err != nil {
		return err
	}
	copies, err := e.st.EmailCopies(ctx, account, emailID)
	if err != nil {
		return err
	}
	var add *store.MembershipAdd
	uid, _ := res.Ref.UID()
	version := res.Ref.VersionNum()
	for _, c := range copies {
		if c.MailboxID == sentID {
			return nil // already recorded
		}
		if draftID != "" && c.MailboxID == draftID {
			a := store.MembershipAdd{MailboxID: sentID, UID: c.UID, UIDValidity: c.UIDValidity}
			add = &a
			version = c.UIDValidity
			uid = c.UID
		}
	}
	if add == nil {
		if uid == 0 {
			return nil
		}
		add = &store.MembershipAdd{MailboxID: sentID, UID: uid, UIDValidity: version}
	}
	removes := []string{}
	if draftID != "" && containsMailbox(copies, draftID) {
		removes = append(removes, draftID)
	}
	_, err = e.st.CommitPatch(ctx, account, emailID, []string{"$seen"}, []string{"$draft"},
		[]store.MembershipAdd{*add}, removes)
	return err
}

// commitSentCopy records a provider-sent message as a new Sent email. The
// record carries the synthetic uid the adapter allocated, so cache and
// server agree from the first read; the copy shares the submitted bytes'
// blob rather than storing them twice.
func (e *Engine) commitSentCopy(ctx context.Context, account, emailID string, res mb.SendResult, raw []byte) error {
	uid, _ := res.Ref.UID()
	if uid == 0 {
		return errors.New("sync: provider send result carries no message uid")
	}
	sentID, err := e.st.MailboxIDByRole(ctx, account, "sent")
	if err != nil {
		return err
	}
	if sentID == "" {
		return errors.New(`sync: account has no mailbox with the "sent" role`)
	}
	folder, err := e.st.MailboxPath(ctx, account, sentID)
	if err != nil {
		return err
	}
	rec, err := convert.SummaryFromRaw(raw, time.Now())
	if err != nil {
		return err
	}
	rec.UID = uid
	rec.UIDValidity = res.Ref.VersionNum()
	rec.Flags = res.Keywords
	if len(rec.Flags) == 0 {
		rec.Flags = []string{`\Seen`}
	}
	created, err := e.st.CommitAppend(ctx, account, folder, rec)
	if err != nil {
		return err
	}
	if blobID, err := e.st.RawBlobID(ctx, account, emailID); err == nil && blobID != "" {
		if err := e.st.LinkRawBlob(ctx, account, created.ID, blobID); err != nil {
			e.log.Warn("sync: sent copy raw link failed", "email", created.ID, "err", err)
		}
	}
	e.log.Debug("sync: provider sent copy recorded", "folder", folder, "email", created.ID)
	return nil
}

// containsMailbox reports whether any copy lives in the named mailbox.
func containsMailbox(copies []store.Copy, mailboxID string) bool {
	for _, c := range copies {
		if c.MailboxID == mailboxID {
			return true
		}
	}
	return false
}

// rawForSubmission returns the message's RFC 5322 bytes: from the blob
// the bridge cached when it built or sent the message, or — for a draft
// written by another client — fetched from IMAP now. Sending needs the
// message, so this is never a list-path download (golden rule 2), and
// the fetched copy is cached so the next send and Email/get's blobId
// come for free.
func (e *Engine) rawForSubmission(ctx context.Context, account, emailID string) ([]byte, error) {
	blobID, err := e.st.RawBlobID(ctx, account, emailID)
	if err != nil {
		return nil, err
	}
	if blobID != "" {
		raw, _, err := e.st.ReadBlob(ctx, account, blobID)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
	raw, err := e.fetchRawBody(emailID)
	if err != nil {
		if errors.Is(err, errNotHydrated) {
			return nil, fmt.Errorf("%w: %s", jmapapi.ErrNoLocation, emailID)
		}
		return nil, err
	}
	newBlob, err := e.st.PutBlob(ctx, account, "message/rfc822", raw)
	if err != nil {
		e.log.Warn("sync: sent raw copy not cached", "email", emailID, "err", err)
		return raw, nil
	}
	if err := e.st.LinkRawBlob(ctx, account, emailID, newBlob); err != nil {
		e.log.Warn("sync: sent raw copy not linked", "email", emailID, "err", err)
	}
	return raw, nil
}

// submissionEnvelope builds the SMTP envelope: the client's when it
// supplied one, otherwise the message's own recipients (To, Cc and Bcc
// all go on the envelope; only Bcc is stripped from the bytes).
func submissionEnvelope(spec jmapapi.SubmissionSpec, msg *jmapapi.Email) (submit.Envelope, error) {
	env := submit.Envelope{From: spec.From}
	if spec.Envelope != nil {
		if spec.Envelope.MailFrom != "" {
			env.From = spec.Envelope.MailFrom
		}
		env.Recipients = append(env.Recipients, spec.Envelope.RcptTo...)
	}
	if len(env.Recipients) == 0 {
		for _, group := range [][]jmapapi.Address{msg.To, msg.Cc, msg.Bcc} {
			for _, a := range group {
				if strings.TrimSpace(a.Email) != "" {
					env.Recipients = append(env.Recipients, a.Email)
				}
			}
		}
	}
	env.Recipients = uniqueAddresses(env.Recipients)
	if len(env.Recipients) == 0 {
		return env, jmapapi.ErrNoRecipients
	}
	var invalid []string
	for _, r := range env.Recipients {
		if _, err := mail.ParseAddress(r); err != nil {
			invalid = append(invalid, r)
		}
	}
	if len(invalid) > 0 {
		return env, &jmapapi.InvalidRecipientsError{Addresses: invalid}
	}
	if _, err := mail.ParseAddress(env.From); err != nil {
		return env, fmt.Errorf("sync: envelope sender is not a valid email address: %w", err)
	}
	return env, nil
}

// uniqueAddresses drops duplicates while keeping order, so a recipient
// on both To and Cc is not RCPTed twice.
func uniqueAddresses(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		key := strings.ToLower(a)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// fileSentCopy APPENDs the message that was just sent to the Sent
// mailbox with \Seen (PLAN §7.2) and records it as its own Email —
// separate from the draft, so a client that retires the draft afterwards
// cannot take the Sent record with it.
func (e *Engine) fileSentCopy(ctx context.Context, account, emailID string, raw []byte) error {
	sentID, err := e.st.MailboxIDByRole(ctx, account, "sent")
	if err != nil {
		return err
	}
	if sentID == "" {
		return errors.New("account has no mailbox with the \"sent\" role")
	}
	folder, err := e.st.MailboxPath(ctx, account, sentID)
	if err != nil {
		return err
	}
	rec, err := convert.SummaryFromRaw(raw, time.Now())
	if err != nil {
		return err
	}
	var ref mb.Ref
	if err := e.wr.withBackend(ctx, func(b mb.Backend) error {
		var err error
		ref, _, err = b.Append(ctx, folder, raw, []string{"$seen"}, &rec.ReceivedAt)
		return err
	}); err != nil {
		return err
	}
	rec.UID, _ = ref.UID()
	rec.UIDValidity = ref.VersionNum()
	rec.Flags = []string{`\Seen`}
	created, err := e.st.CommitAppend(ctx, account, folder, rec)
	if err != nil {
		return err
	}
	// The copy is byte-identical to what was submitted, so it shares the
	// submitted message's blob instead of storing the bytes twice.
	if blobID, err := e.st.RawBlobID(ctx, account, emailID); err == nil && blobID != "" {
		if err := e.st.LinkRawBlob(ctx, account, created.ID, blobID); err != nil {
			e.log.Warn("sync: sent copy raw link failed", "email", created.ID, "err", err)
		}
	}
	e.log.Debug("sync: sent copy filed", "folder", folder, "email", created.ID)
	return nil
}

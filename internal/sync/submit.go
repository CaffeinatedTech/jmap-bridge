package sync

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/convert"
	"github.com/CaffeinatedTech/jmap-bridge/internal/imapdrv"
	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
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
	if e.cfg.SMTP == nil {
		return nil, jmapapi.ErrNoSubmissionBackend
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
	// The id is minted before the send: after SMTP has accepted the
	// message there is no error left that the caller may be told about,
	// because "not created" would make it send the message twice.
	id, err := e.st.MintID(ctx)
	if err != nil {
		return nil, err
	}

	// Bcc travels to the envelope but never on the wire (RFC 8621 §7.5):
	// the stored copy keeps its Bcc header, the delivered one does not.
	if err := submit.Send(ctx, *e.cfg.SMTP, env, convert.StripBcc(raw)); err != nil {
		var rejected *submit.RejectedError
		if errors.As(err, &rejected) {
			return nil, &jmapapi.SMTPError{Reply: rejected.Reply}
		}
		return nil, err
	}

	// The message exists out there; every failure below is logged, never
	// reported as a failed create.
	if !spec.FilesItself() {
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
	var uid, uidValidity uint32
	if err := e.wr.withConn(ctx, func(conn *imapdrv.Conn) error {
		var err error
		uid, uidValidity, err = conn.AppendMessage(ctx, folder, raw, []string{`\Seen`}, &rec.ReceivedAt)
		return err
	}); err != nil {
		return err
	}
	rec.UID, rec.UIDValidity = uid, uidValidity
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

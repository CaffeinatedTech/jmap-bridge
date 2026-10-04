package gmailapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// The write half of the thin client (M11): the generated calls whose return
// shape differs from the read helpers, plus the delete calls that return no
// body. Like the read operations these are unexported so no google type
// escapes the package (D-API-2).

// errCall is the shape of a generated call that returns no value (Delete).
type errCall interface {
	Do(...googleapi.CallOption) error
	Header() http.Header
}

// invokeErr is invoke for a call with no response body: pace, retry and
// classify exactly like invoke.
func invokeErr[C errCall](ctx context.Context, c *Client, cost int, call C) error {
	for attempt := 0; ; attempt++ {
		if err := c.pacer.Wait(ctx, cost); err != nil {
			return err
		}
		err := call.Do()
		if err == nil {
			return nil
		}
		classified := classify(err)
		if attempt >= c.maxRetries || !retryable(classified) {
			return classified
		}
		if serr := c.clock.Sleep(ctx, backoffDelay(err, attempt, c.clock.Now())); serr != nil {
			return serr
		}
	}
}

// deleteMessage permanently deletes one message. A 404 classifies as
// ErrNotFound, which the caller treats as already-gone (FR-M.10).
func (c *Client) deleteMessage(ctx context.Context, id string) error {
	return invokeErr(ctx, c, CostMessagesDelete, c.svc.Users.Messages.Delete(userMe, id).Context(ctx))
}

// createDraft stores raw RFC 5322 bytes as a Gmail draft with the given
// labels, returning the draft handle and its message id.
func (c *Client) createDraft(ctx context.Context, raw []byte, labelIDs []string) (*gmail.Draft, error) {
	draft := &gmail.Draft{Message: &gmail.Message{
		Raw:      base64.RawURLEncoding.EncodeToString(raw),
		LabelIds: labelIDs,
	}}
	return invoke(ctx, c, CostDraftsCreate, c.svc.Users.Drafts.Create(userMe, draft).Context(ctx))
}

// importMessage inserts raw RFC 5322 bytes into the mailbox with the given
// labels via users.messages.import (RFC 8621 Email/import, FR-M.11). Unlike
// messages.send it never relays; Gmail scans and files the message as if it
// had arrived, which is what importing a message into a mailbox means.
// internalDateSource=dateHeader makes the message's received time follow its
// Date header — the closest the API allows to IMAP APPEND's INTERNALDATE
// (D-API-10), and the source the conformance fixtures use.
func (c *Client) importMessage(ctx context.Context, raw []byte, labelIDs []string) (*gmail.Message, error) {
	msg := &gmail.Message{
		Raw:      base64.RawURLEncoding.EncodeToString(raw),
		LabelIds: labelIDs,
	}
	call := c.svc.Users.Messages.Import(userMe, msg).Context(ctx).InternalDateSource("dateHeader")
	return invoke(ctx, c, CostMessagesImport, call)
}

// createLabel creates a user label; Gmail interprets "/" in name as
// hierarchy (M11 Mailbox/set create).
func (c *Client) createLabel(ctx context.Context, name string) (*gmail.Label, error) {
	body := &gmail.Label{
		Name:                  name,
		LabelListVisibility:   "labelShow",
		MessageListVisibility: "show",
	}
	return invoke(ctx, c, CostLabelsCreate, c.svc.Users.Labels.Create(userMe, body).Context(ctx))
}

// patchLabel renames (and, via "/" in name, reparents) a label.
func (c *Client) patchLabel(ctx context.Context, id, name string) (*gmail.Label, error) {
	return invoke(ctx, c, CostLabelsUpdate, c.svc.Users.Labels.Patch(userMe, id, &gmail.Label{Name: name}).Context(ctx))
}

// deleteLabel removes a user label.
func (c *Client) deleteLabel(ctx context.Context, id string) error {
	return invokeErr(ctx, c, CostLabelsDelete, c.svc.Users.Labels.Delete(userMe, id).Context(ctx))
}

// sendMessage relays raw RFC 5322 bytes as a new message; Gmail files its
// own Sent copy. It does not retry (invokeOnce): an ambiguous failure is
// reconciled by Message-ID by the caller.
func (c *Client) sendMessage(ctx context.Context, raw []byte) (*gmail.Message, error) {
	msg := &gmail.Message{Raw: base64.RawURLEncoding.EncodeToString(raw)}
	return invokeOnce(ctx, c, CostMessagesSend, c.svc.Users.Messages.Send(userMe, msg).Context(ctx))
}

// sendDraft sends an existing Gmail draft (drafts.send): Gmail consumes the
// draft handle and files Sent. Like sendMessage it is single-shot.
func (c *Client) sendDraft(ctx context.Context, draftID string) (*gmail.Message, error) {
	draft := &gmail.Draft{Id: draftID}
	return invokeOnce(ctx, c, CostDraftsSend, c.svc.Users.Drafts.Send(userMe, draft).Context(ctx))
}

// findByRFC822MessageID resolves the message id of the message whose
// Message-ID header is msgid, or "" when none is indexed yet. It is the
// reconciliation read for an ambiguous send (GMAIL_API_PLAN §8.1).
func (c *Client) findByRFC822MessageID(ctx context.Context, msgid string) (string, error) {
	q := "rfc822msgid:" + strings.Trim(strings.TrimSpace(msgid), "<>")
	resp, err := c.listMessages(ctx, q, nil, 1, "")
	if err != nil {
		return "", err
	}
	if len(resp.Messages) == 0 {
		return "", nil
	}
	return resp.Messages[0].Id, nil
}

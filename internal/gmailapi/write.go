package gmailapi

import (
	"context"
	"encoding/base64"
	"net/http"

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

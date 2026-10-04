package gmailapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"

	"google.golang.org/api/gmail/v1"
)

// maxBatchGet bounds how many messages.get sub-requests one /batch call
// fans out. Google accepts up to 100 per batch, but a large fan-out trips
// the per-user "Too many concurrent requests for user" limit (Gmail's own
// error guide names "batches with a large number of requests" as a
// cause) — observed live with a 100-item batch against a real mailbox.
// Batching affects round trips, not quota (the pacer is charged the sum),
// so a small cap costs nothing in throughput and stays well clear of the
// concurrency limit. The Gmail per-user quota, not the batch count,
// governs how fast a cold backfill drains.
const maxBatchGet = 10

// batchGetMessages fetches several messages in one /batch round trip,
// returning them keyed by id. A sub-response that is 404 (the message was
// deleted between list and fetch) is omitted rather than failing the
// batch; any other per-item error fails the call. metadataHeaders are
// passed through for the metadata format.
func (c *Client) batchGetMessages(ctx context.Context, ids []string, format messageFormat, metadataHeaders []string) (map[string]*gmail.Message, error) {
	out := make(map[string]*gmail.Message, len(ids))
	for start := 0; start < len(ids); start += maxBatchGet {
		end := min(start+maxBatchGet, len(ids))
		chunk := ids[start:end]
		items := make([]BatchItem, len(chunk))
		for i, id := range chunk {
			q := url.Values{}
			q.Set("format", string(format))
			for _, h := range metadataHeaders {
				q.Add("metadataHeaders", h)
			}
			items[i] = BatchItem{
				Method: "GET",
				Path:   fmt.Sprintf("gmail/v1/users/me/messages/%s?%s", url.PathEscape(id), q.Encode()),
				Cost:   CostMessagesGet,
			}
		}
		results, err := c.Batch(ctx, items)
		if err != nil {
			return nil, err
		}
		for i, res := range results {
			if res.Err != nil {
				if IsNotFound(res.Err) {
					continue
				}
				return nil, res.Err
			}
			var m gmail.Message
			if err := json.Unmarshal(res.Body, &m); err != nil {
				return nil, fmt.Errorf("gmailapi: decode batch message %s: %w", chunk[i], err)
			}
			out[chunk[i]] = &m
		}
	}
	return out, nil
}

// decodeRaw decodes a base64url message body, tolerating both the padded
// and unpadded encodings the API has used (D-API-7).
func decodeRaw(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

package gmailapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"google.golang.org/api/googleapi"
)

// Batch limits. Google accepts up to 100 sub-requests per batch; 32 MiB is the
// bridge's own per-part ceiling (upload cap 64 MiB is for raw bodies, not JSON).
const (
	maxBatchItems     = 100
	maxBatchPartBytes = 32 << 20
)

// BatchItem is one sub-request in a Gmail batch.
type BatchItem struct {
	// Method is "GET" or "POST"; empty means GET.
	Method string
	// Path is the API path with no leading slash, query included, e.g.
	// "gmail/v1/users/me/messages/abc?format=metadata".
	Path string
	// Body is the JSON request body for POST, nil for GET.
	Body []byte
	// Cost is the quota units this sub-request consumes (see costs.go).
	Cost int
	// Header carries any extra request headers.
	Header http.Header
}

// BatchResult is one sub-response. Status and Body are always populated when
// the envelope parsed; Err is set (via the same classifier as a direct call)
// when the sub-response was not 2xx, so a throttled item satisfies
// errors.Is(err, mailbackend.ErrThrottled).
type BatchResult struct {
	Status int
	Header http.Header
	Body   []byte
	Err    error
}

// Batch sends sub-requests in one multipart/mixed request and correlates each
// response part to its request by Content-ID. The generated client has no batch
// support (google-api-go-client #179, GMAIL_API_PLAN §4.2), so this is the one
// hand-rolled wire piece. Batching reduces round trips, not quota: the pacer is
// charged the sum of the item costs. An envelope-level throttle or 5xx is
// retried with backoff; per-item errors are returned in place, never retried
// here, because the caller knows which items are idempotent.
func (c *Client) Batch(ctx context.Context, items []BatchItem) ([]BatchResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > maxBatchItems {
		return nil, fmt.Errorf("gmailapi: batch of %d sub-requests exceeds the %d-item limit", len(items), maxBatchItems)
	}
	total := 0
	for _, it := range items {
		total += it.Cost
	}
	body, boundary, err := buildBatchBody(items)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := c.pacer.Wait(ctx, total); err != nil {
			return nil, err
		}
		resp, err := c.postBatch(ctx, body, boundary)
		if err == nil {
			return parseBatchResponse(resp, len(items))
		}
		classified := classify(err)
		lastErr = classified
		if attempt >= c.maxRetries || !retryable(classified) {
			return nil, classified
		}
		if serr := c.clock.Sleep(ctx, backoffDelay(err, attempt, c.clock.Now())); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// postBatch issues one batch request and returns the open response.
func (c *Client) postBatch(ctx context.Context, body []byte, boundary string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.batchURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+boundary)
	req.ContentLength = int64(len(body))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if err := googleapi.CheckResponse(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// buildBatchBody serialises the outer multipart envelope. Each part is an
// application/http message whose Content-ID is <item-N>; response parts are
// matched back by that id (Google echoes it, sometimes as <response-item-N>).
func buildBatchBody(items []BatchItem) (body []byte, boundary string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, it := range items {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", "application/http")
		h.Set("Content-Transfer-Encoding", "binary")
		h.Set("Content-ID", fmt.Sprintf("<item-%d>", i))
		part, perr := mw.CreatePart(h)
		if perr != nil {
			return nil, "", perr
		}
		method := it.Method
		if method == "" {
			method = http.MethodGet
		}
		path := "/" + strings.TrimPrefix(it.Path, "/")
		if _, werr := fmt.Fprintf(part, "%s %s HTTP/1.1\r\n", method, path); werr != nil {
			return nil, "", werr
		}
		if it.Body != nil {
			if _, werr := fmt.Fprintf(part, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(it.Body)); werr != nil {
				return nil, "", werr
			}
		}
		for k, vs := range it.Header {
			for _, v := range vs {
				if _, werr := fmt.Fprintf(part, "%s: %s\r\n", k, v); werr != nil {
					return nil, "", werr
				}
			}
		}
		if _, werr := io.WriteString(part, "\r\n"); werr != nil {
			return nil, "", werr
		}
		if len(it.Body) > 0 {
			if _, werr := part.Write(it.Body); werr != nil {
				return nil, "", werr
			}
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.Boundary(), nil
}

// parseBatchResponse reads the multipart response and returns results indexed
// to match the request slice.
func parseBatchResponse(resp *http.Response, n int) ([]BatchResult, error) {
	defer func() { _ = resp.Body.Close() }()
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("gmailapi: batch response is not multipart (Content-Type %q)", resp.Header.Get("Content-Type"))
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("gmailapi: batch response has no boundary")
	}
	results := make([]BatchResult, n)
	seen := make([]bool, n)
	mr := multipart.NewReader(resp.Body, boundary)
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return nil, fmt.Errorf("gmailapi: batch part: %w", perr)
		}
		idx := contentIDIndex(part.Header.Get("Content-ID"), n, seen)
		inner, rerr := http.ReadResponse(bufio.NewReader(part), nil)
		if rerr != nil {
			_ = part.Close()
			return nil, fmt.Errorf("gmailapi: batch part %d: %w", idx, rerr)
		}
		raw, _ := io.ReadAll(io.LimitReader(inner.Body, maxBatchPartBytes))
		_ = inner.Body.Close()
		_ = part.Close()
		r := BatchResult{Status: inner.StatusCode, Header: inner.Header, Body: raw}
		if cerr := googleapi.CheckResponseWithBody(inner, raw); cerr != nil {
			r.Err = classify(cerr)
		}
		results[idx] = r
		seen[idx] = true
	}
	return results, nil
}

// contentIDIndex parses a response Content-ID into a request index, falling
// back to the first unassigned slot when the id is absent or unrecognised so a
// server that omits ids still correlates positionally.
func contentIDIndex(raw string, n int, seen []bool) int {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, "<>")
	s = strings.TrimPrefix(s, "response-")
	s = strings.TrimPrefix(s, "item-")
	if idx, err := strconv.Atoi(s); err == nil && idx >= 0 && idx < n && !seen[idx] {
		return idx
	}
	for i, ok := range seen {
		if !ok {
			return i
		}
	}
	return 0
}

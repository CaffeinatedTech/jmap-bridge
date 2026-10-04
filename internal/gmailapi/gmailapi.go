// Package gmailapi is the Gmail REST API client the Gmail-API-mode backend is
// built on (GMAIL_API_PLAN.md, D-API-2/D-API-7). It wraps the official
// google.golang.org/api/gmail/v1 generated client with the pieces that client
// lacks: a quota pacer, an error taxonomy that speaks the backend-neutral
// mailbackend vocabulary, and a hand-rolled batch endpoint (google-api-go-client
// #179). OAuth2 is not re-implemented here — TokenSourceFromManager bridges the
// project's existing internal/oauth.Manager into golang.org/x/oauth2.
//
// Generated *gmail.* types never leave this package: the thin operations below
// are unexported so the M10 adapter (also in this package) can use them while no
// caller can name a google type. All calls are context-aware and every network
// call is classified and paced.
package gmailapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// userMe is Gmail's alias for the authenticated user; API mode is one account,
// one token, so "me" is always the right principal.
const userMe = "me"

// Options configures a Client. Exactly one of TokenSource or HTTPClient is
// required; TokenSource is the production path (see TokenSourceFromManager).
type Options struct {
	// TokenSource authenticates every request. It is wrapped in an
	// oauth2-authenticated http.Client.
	TokenSource oauth2.TokenSource
	// HTTPClient is a pre-authenticated client, used by tests that inject a
	// static bearer token. It wins when both are set.
	HTTPClient *http.Client
	// Endpoint overrides the API base URL; empty means Google's default. Only
	// the fixture tests set it (GMAIL_API_PLAN §4.1 keeps the endpoint
	// non-configurable in production).
	Endpoint string
	// QuotaUnitsPerSecond paces requests; 0 selects DefaultQuotaUnitsPerSecond.
	// Negative disables pacing.
	QuotaUnitsPerSecond int
	// MaxRetries bounds retries of a retryable failure; 0 selects
	// DefaultMaxRetries.
	MaxRetries int
	// Clock overrides the time source for pacing and backoff; tests inject a
	// fake to stay deterministic. Nil selects the real clock.
	Clock Clock
}

// Client is one account's Gmail API client. It is safe for concurrent use by
// net/http's transport, but the engine serialises calls per account (PLAN §10).
type Client struct {
	svc        *gmail.Service
	httpClient *http.Client
	base       string
	pacer      *Pacer
	maxRetries int
	clock      Clock
}

// New builds a client. It fails closed on missing credentials or a bad endpoint
// rather than making an unauthenticated request.
func New(ctx context.Context, opts Options) (*Client, error) {
	hc := opts.HTTPClient
	if hc == nil {
		if opts.TokenSource == nil {
			return nil, errors.New("gmailapi: Options needs TokenSource or HTTPClient")
		}
		hc = oauth2.NewClient(ctx, opts.TokenSource)
	}
	clientOpts := []option.ClientOption{option.WithHTTPClient(hc)}
	if opts.Endpoint != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(strings.TrimRight(opts.Endpoint, "/")+"/"))
	}
	svc, err := gmail.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, err
	}
	rate := opts.QuotaUnitsPerSecond
	if rate == 0 {
		rate = DefaultQuotaUnitsPerSecond
	}
	retries := opts.MaxRetries
	if retries == 0 {
		retries = DefaultMaxRetries
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Client{
		svc:        svc,
		httpClient: hc,
		base:       svc.BasePath,
		pacer:      NewPacer(rate, clock),
		maxRetries: retries,
		clock:      clock,
	}, nil
}

// Close releases idle connections the client opened.
func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// BasePath returns the API base URL the client targets (Google's default, or
// the fixture endpoint in tests).
func (c *Client) BasePath() string { return c.base }

// batchURL is the Gmail batch endpoint on the same origin as the API (D-13).
func (c *Client) batchURL() string {
	return strings.TrimRight(c.base, "/") + "/batch/gmail/v1"
}

// apiCall is the shape every generated method call shares: a Do that returns
// the typed response and a Header accessor. It lets one generic helper pace,
// retry and classify every call.
type apiCall[T any] interface {
	Do(...googleapi.CallOption) (T, error)
	Header() http.Header
}

// invoke runs one generated call under the pacer, retrying throttles and
// transient failures with backoff and returning the classified error otherwise.
func invoke[T any, C apiCall[T]](ctx context.Context, c *Client, cost int, call C) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		if err := c.pacer.Wait(ctx, cost); err != nil {
			return zero, err
		}
		v, err := call.Do()
		if err == nil {
			return v, nil
		}
		classified := classify(err)
		if attempt >= c.maxRetries || !retryable(classified) {
			return zero, classified
		}
		if serr := c.clock.Sleep(ctx, backoffDelay(err, attempt, c.clock.Now())); serr != nil {
			return zero, serr
		}
	}
}

// invokeOnce paces and classifies one call without retrying. Send uses it:
// a retried send can duplicate a message whose response was lost, so an
// ambiguous outcome is reconciled by Message-ID instead (GMAIL_API_PLAN
// §8.1).
func invokeOnce[T any, C apiCall[T]](ctx context.Context, c *Client, cost int, call C) (T, error) {
	var zero T
	if err := c.pacer.Wait(ctx, cost); err != nil {
		return zero, err
	}
	v, err := call.Do()
	if err != nil {
		return zero, classify(err)
	}
	return v, nil
}

// --- thin typed operations (unexported: generated types stay in this package) ---

func (c *Client) profile(ctx context.Context) (*gmail.Profile, error) {
	return invoke(ctx, c, CostGetProfile, c.svc.Users.GetProfile(userMe).Context(ctx))
}

func (c *Client) listLabels(ctx context.Context) (*gmail.ListLabelsResponse, error) {
	return invoke(ctx, c, CostLabelsList, c.svc.Users.Labels.List(userMe).Context(ctx))
}

func (c *Client) listMessages(ctx context.Context, query string, labelIDs []string, max int64, pageToken string) (*gmail.ListMessagesResponse, error) {
	return c.listMessagesPage(ctx, listOpts{
		Query: query, LabelIDs: labelIDs, Max: max, PageToken: pageToken,
	})
}

// listOpts is the full messages.list parameter set; the adapter uses
// IncludeSpamTrash so the synthetic All Mail container is truthful.
type listOpts struct {
	Query            string
	LabelIDs         []string
	Max              int64
	PageToken        string
	IncludeSpamTrash bool
}

func (c *Client) listMessagesPage(ctx context.Context, o listOpts) (*gmail.ListMessagesResponse, error) {
	call := c.svc.Users.Messages.List(userMe).Context(ctx)
	if o.Query != "" {
		call = call.Q(o.Query)
	}
	if len(o.LabelIDs) > 0 {
		call = call.LabelIds(o.LabelIDs...)
	}
	if o.Max > 0 {
		call = call.MaxResults(o.Max)
	}
	if o.PageToken != "" {
		call = call.PageToken(o.PageToken)
	}
	if o.IncludeSpamTrash {
		call = call.IncludeSpamTrash(true)
	}
	return invoke(ctx, c, CostMessagesList, call)
}

// messageFormat is the subset of gmail's format parameter API mode uses.
type messageFormat string

const (
	formatMetadata messageFormat = "metadata"
	formatRaw      messageFormat = "raw"
)

func (c *Client) getMessage(ctx context.Context, id string, format messageFormat, metadataHeaders ...string) (*gmail.Message, error) {
	call := c.svc.Users.Messages.Get(userMe, id).Format(string(format)).Context(ctx)
	if len(metadataHeaders) > 0 {
		call = call.MetadataHeaders(metadataHeaders...)
	}
	return invoke(ctx, c, CostMessagesGet, call)
}

func (c *Client) modifyMessage(ctx context.Context, id string, add, remove []string) (*gmail.Message, error) {
	req := &gmail.ModifyMessageRequest{AddLabelIds: add, RemoveLabelIds: remove}
	return invoke(ctx, c, CostMessagesModify, c.svc.Users.Messages.Modify(userMe, id, req).Context(ctx))
}

func (c *Client) listHistory(ctx context.Context, startHistoryID uint64, pageToken string) (*gmail.ListHistoryResponse, error) {
	call := c.svc.Users.History.List(userMe).StartHistoryId(startHistoryID).MaxResults(500).Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return invoke(ctx, c, CostHistoryList, call)
}

// watch registers a Cloud Pub/Sub push for the account (M13, FR-S.14). The
// topic must already exist and grant gmail-api-push@system.gserviceaccount.com
// publish; the response carries the watch's expiration, which the adapter
// renews against.
func (c *Client) watch(ctx context.Context, topic string) (*gmail.WatchResponse, error) {
	req := &gmail.WatchRequest{TopicName: topic}
	return invoke(ctx, c, CostWatch, c.svc.Users.Watch(userMe, req).Context(ctx))
}

// stop cancels the account's push watch (users.stop). It is called on
// graceful shutdown; Google also expires the watch on its own. It is a
// best-effort shutdown call, so it paces and classifies but does not retry.
func (c *Client) stop(ctx context.Context) error {
	if err := c.pacer.Wait(ctx, CostStop); err != nil {
		return err
	}
	return classify(c.svc.Users.Stop(userMe).Context(ctx).Do())
}

// now returns the client's current time (the injected clock in tests), so
// renewal scheduling is deterministic where the clock is faked.
func (c *Client) now() time.Time { return c.clock.Now() }

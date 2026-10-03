package gmailapi

import (
	"context"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"

	"github.com/CaffeinatedTech/jmap-bridge/test/fixturegmail"
)

// fakeClock is a deterministic Clock: Sleep advances the clock by exactly the
// requested duration and records it, so pacer and backoff behaviour is asserted
// without real waiting.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// newTestClient builds a client pointed at a fixture with pacing disabled (so
// only retry backoff shows up in the fake clock) and a static bearer token.
func newTestClient(t *testing.T, fx *fixturegmail.Server, mutate ...func(*Options)) *Client {
	t.Helper()
	opts := Options{
		Endpoint:            fx.URL(),
		HTTPClient:          oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: fx.Token()})),
		QuotaUnitsPerSecond: -1,
		Clock:               newFakeClock(),
	}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// neutralHeaders converts generated payload headers for the mapping helpers.
func neutralHeaders(hs []*gmail.MessagePartHeader) []Header {
	out := make([]Header, 0, len(hs))
	for _, h := range hs {
		out = append(out, Header{Name: h.Name, Value: h.Value})
	}
	return out
}

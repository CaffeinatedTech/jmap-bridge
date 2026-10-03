package gmailapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

func TestClassifyMapsProviderErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     error
		throttle bool
		retry    bool
	}{
		{"429", &googleapi.Error{Code: 429}, mailbackend.ErrThrottled, true, true},
		{"403 rate reason", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, mailbackend.ErrThrottled, true, true},
		{"403 quota message", &googleapi.Error{Code: 403, Message: "Quota exceeded for quota metric"}, mailbackend.ErrThrottled, true, true},
		{"401", &googleapi.Error{Code: 401, Message: "Invalid Credentials"}, mailbackend.ErrAuth, false, false},
		{"404", &googleapi.Error{Code: 404, Message: "not found"}, ErrNotFound, false, false},
		{"500", &googleapi.Error{Code: 500}, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Fatalf("classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if IsThrottled(got) != tc.throttle {
				t.Fatalf("IsThrottled = %v, want %v", IsThrottled(got), tc.throttle)
			}
			if retryable(got) != tc.retry {
				t.Fatalf("retryable = %v, want %v", retryable(got), tc.retry)
			}
		})
	}
}

func TestClassifyPermissionIsRefusal(t *testing.T) {
	err := classify(&googleapi.Error{Code: 403, Message: "Insufficient Permission"})
	if !mailbackend.IsRejected(err) {
		t.Fatalf("403 permission is not a RejectedError: %v", err)
	}
}

func TestClassifyContextIsFinal(t *testing.T) {
	if got := classify(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("cancel is not final: %v", got)
	}
	if retryable(classify(context.Canceled)) {
		t.Fatal("cancelled context is retryable")
	}
}

func TestBackoffHonoursRetryAfter(t *testing.T) {
	now := time.Unix(1000, 0)
	gerr := &googleapi.Error{Code: 429, Header: map[string][]string{"Retry-After": {"7"}}}
	if d := backoffDelay(gerr, 0, now); d != 7*time.Second {
		t.Fatalf("Retry-After seconds = %v, want 7s", d)
	}
	// HTTP-date form (http.TimeFormat is the GMT spelling ParseTime accepts).
	future := now.Add(3 * time.Second).UTC().Format(http.TimeFormat)
	gerr.Header.Set("Retry-After", future)
	if d := backoffDelay(gerr, 0, now); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("Retry-After date = %v, want ~3s", d)
	}
}

func TestBackoffCapsAndJitters(t *testing.T) {
	now := time.Unix(1000, 0)
	// A huge attempt count must clamp to the cap, never overflow.
	d := backoffDelay(&googleapi.Error{Code: 500}, 40, now)
	if d <= 0 || d > backoffCap {
		t.Fatalf("backoff = %v, want (0, %v]", d, backoffCap)
	}
}

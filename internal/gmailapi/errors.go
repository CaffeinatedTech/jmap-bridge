package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/CaffeinatedTech/jmap-bridge/internal/mailbackend"
)

// ErrNotFound reports that the addressed object does not exist (HTTP 404).
// Callers decide the semantics: a 404 on messages.delete is already-gone (a
// successful no-op, FR-M.10), a 404 on messages.get means the message was
// removed between a list and a fetch. It is distinct from a refusal so the
// write path can tell "the server has no such object" from "the server said no".
var ErrNotFound = errors.New("gmailapi: object not found")

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsThrottled reports whether err is the backend-neutral throttle sentinel.
func IsThrottled(err error) bool { return errors.Is(err, mailbackend.ErrThrottled) }

// retryableError marks a transport failure or a 5xx that is worth retrying.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// classify maps a raw transport or API error into the backend-neutral error
// vocabulary (D-API-2): ErrThrottled for 429/rate-quota 403, ErrAuth for 401,
// ErrNotFound for 404, and mailbackend.RejectedError for every other understood
// refusal. A 5xx or a transport failure becomes a retryableError, so the retry
// loop keys off one predicate. The original *googleapi.Error is preserved in the
// chain for Retry-After parsing.
func classify(err error) error {
	if err == nil {
		return nil
	}
	// A context that the caller cancelled or that ran out on our side is final:
	// retrying would only burn the same wall.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch {
		case gerr.Code == http.StatusUnauthorized:
			return fmt.Errorf("%w: %s", mailbackend.ErrAuth, apiMessage(gerr))
		case isThrottle(gerr):
			return fmt.Errorf("%w: %s", mailbackend.ErrThrottled, apiMessage(gerr))
		case gerr.Code == http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, apiMessage(gerr))
		case gerr.Code >= 500:
			return &retryableError{fmt.Errorf("gmailapi: server error %d: %s", gerr.Code, apiMessage(gerr))}
		default:
			return &mailbackend.RejectedError{Text: "gmailapi: " + apiMessage(gerr), Err: err}
		}
	}
	// Everything else is a transport failure (dial, reset, timeout): retryable.
	return &retryableError{err}
}

// retryable reports whether classify's result is worth another attempt.
func retryable(err error) bool {
	if errors.Is(err, mailbackend.ErrThrottled) {
		return true
	}
	var r *retryableError
	return errors.As(err, &r)
}

// isThrottle reports whether a Google error is a rate/quota refusal. Google
// signals these with 429, or with a 403 whose typed reason or message names the
// rate limit (the reason set has shifted over the years, so the message is a
// documented fallback).
func isThrottle(gerr *googleapi.Error) bool {
	if gerr.Code == http.StatusTooManyRequests {
		return true
	}
	if gerr.Code != http.StatusForbidden {
		return false
	}
	for _, item := range gerr.Errors {
		switch item.Reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded",
			"dailyLimitExceeded", "downloadQuotaExceeded":
			return true
		}
	}
	msg := strings.ToLower(gerr.Message)
	return strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota")
}

// apiMessage prefers the structured message, falling back to the raw body so a
// refusal is never described by an empty string.
func apiMessage(gerr *googleapi.Error) string {
	if gerr.Message != "" {
		return gerr.Message
	}
	if gerr.Body != "" {
		return gerr.Body
	}
	return fmt.Sprintf("HTTP %d", gerr.Code)
}

// backoff bounds. The base doubles per retry with full jitter between half and
// the full interval; a provider Retry-After always wins over the computed value.
const (
	backoffBase = 500 * time.Millisecond
	backoffCap  = 30 * time.Second
)

// backoffDelay returns how long to wait before retrying err, honouring a
// Retry-After header (seconds or HTTP date) when the provider sent one.
func backoffDelay(err error, attempt int, now time.Time) time.Duration {
	if d, ok := retryAfter(err, now); ok {
		return min(d, backoffCap)
	}
	d := backoffBase << attempt
	if d <= 0 || d > backoffCap {
		d = backoffCap
	}
	// Full jitter in [d/2, d) keeps a fleet of accounts from retrying in lockstep.
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryAfter parses a provider Retry-After header, if one is present. Both the
// delta-seconds and HTTP-date forms are accepted (RFC 9110 §10.2.3).
func retryAfter(err error, now time.Time) (time.Duration, bool) {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Header == nil {
		return 0, false
	}
	raw := strings.TrimSpace(gerr.Header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if secs, perr := strconv.Atoi(raw); perr == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, perr := http.ParseTime(raw); perr == nil {
		if d := when.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

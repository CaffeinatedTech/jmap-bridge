package imapdrv

import (
	"context"
	"errors"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// Throttle handling (FR-S.12, learned in the M4 live gate): Google's
// IMAP servers answer [THROTTLED] when a command was not completed
// because the account hit its rate limit — and the code rides on OK as
// readily as on NO. An OK [THROTTLED] that we mistook for success made
// the cache report labels Gmail never kept. Every command completion in
// this driver goes through waitCmd, which converts a qualified OK into
// ErrThrottled so no caller can mistake it for work done.

// ErrThrottled reports a [THROTTLED] response: the server refused to
// complete the command. Callers must not commit anything locally, and
// the sync engine backs off for the cooldown instead of retrying into
// the same wall.
var ErrThrottled = errors.New("imapdrv: server throttled the command ([THROTTLED]): not completed")

// IsThrottled reports whether err is (or wraps) a throttled response.
func IsThrottled(err error) bool {
	if errors.Is(err, ErrThrottled) {
		return true
	}
	var imapErr *imap.Error
	return errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCode("THROTTLED")
}

// throttledOK is the resp-text-code Google attaches to an OK whose
// command was not completed.
const throttledOK = "THROTTLED"

// waiter abstracts the per-command Wait plus the RespCode the fork
// exposes, so waitCmd can type-infer over every command shape.
type waiter[T any] interface {
	Wait(context.Context) (T, error)
	RespCode() string
}

// waitCmd awaits one command. A NO/BAD [THROTTLED] and an OK
// [THROTTLED] both become ErrThrottled; a plain OK's data returns
// untouched.
func waitCmd[T any](ctx context.Context, cmd waiter[T]) (T, error) {
	v, err := cmd.Wait(ctx)
	if err != nil {
		if IsThrottled(err) {
			return v, ErrThrottled
		}
		return v, err
	}
	if cmd.RespCode() == throttledOK {
		return v, ErrThrottled
	}
	return v, nil
}

// waitVoid awaits a plain *Command (one whose Wait returns only an
// error) with the same [THROTTLED] conversion as waitCmd.
func waitVoid(ctx context.Context, cmd *imapclient.Command) error {
	if err := cmd.Wait(ctx); err != nil {
		if IsThrottled(err) {
			return ErrThrottled
		}
		return err
	}
	if cmd.RespCode() == throttledOK {
		return ErrThrottled
	}
	return nil
}

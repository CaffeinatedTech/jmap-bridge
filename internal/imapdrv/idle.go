package imapdrv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IdleSession is one running IDLE command. Notifications arrive on the
// connection's Notes channel (FR-S.4): the session itself just holds
// the connection in IDLE and reports how it ended.
type IdleSession struct {
	cancel context.CancelFunc
	done   chan error
}

// StartIdle puts the connection into IDLE (RFC 2177) and returns once
// the server has accepted it — the moment another client's change can
// be observed (FR-S.4, FR-S.7). When the server does not advertise
// IDLE the library falls back to NOOP polling; readiness is then
// unavailable and StartIdle proceeds anyway, with polling latency
// (Options.IdlePollInterval).
func (c *Conn) StartIdle() (*IdleSession, error) {
	if c.idle != nil {
		return nil, errors.New("imapdrv: IDLE already active")
	}
	cmd := c.client.Idle(nil)

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 15*time.Second)
	err := cmd.WaitReady(readyCtx)
	cancelReady()
	if err != nil && !isIdleFallback(err) {
		return nil, fmt.Errorf("imapdrv: idle start: %w", err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	s := &IdleSession{cancel: cancel, done: make(chan error, 1)}
	go func() {
		s.done <- cmd.Wait(waitCtx)
	}()
	c.idle = s
	return s, nil
}

// IdleDone yields the error that ended the current IDLE (nil only for
// an orderly stop). A new IDLE may start after it is received.
func (s *IdleSession) IdleDone() <-chan error { return s.done }

// EndIdle sends DONE and waits for the tagged completion, so the
// connection is synchronised before the next command (RFC 2177).
func (c *Conn) EndIdle(s *IdleSession) error {
	if s == nil || c.idle != s {
		return nil
	}
	c.idle = nil
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("imapdrv: IDLE did not end within 15s")
	}
}

// isIdleFallback recognises the library's "no IDLE, polling instead"
// readiness refusal — not an error, just latency.
func isIdleFallback(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOOP polling")
}

package gmailapi

import (
	"context"
	"sync"
	"time"
)

// Clock is the time source the pacer and backoff use. It is an interface so
// tests can drive quota waits and retries deterministically instead of sleeping
// against the wall clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// Sleep waits for d or until ctx is done, whichever comes first.
	Sleep(ctx context.Context, d time.Duration) error
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Pacer is a token bucket that meters outbound requests at unitsPerSecond
// quota units, charging each call its method cost (§4.3). It is not safe for
// concurrent use by design — the engine serialises calls per account (PLAN §10)
// — but a mutex makes the accounting correct even if that changes.
type Pacer struct {
	rate  float64 // quota units per second
	clock Clock

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewPacer builds a pacer at unitsPerSecond; a non-positive rate disables
// pacing (every Wait returns immediately), which the fixture tests use to keep
// timings deterministic. A nil clock selects the real clock.
func NewPacer(unitsPerSecond int, clock Clock) *Pacer {
	if clock == nil {
		clock = realClock{}
	}
	if unitsPerSecond <= 0 {
		return &Pacer{rate: 0, clock: clock, last: clock.Now()}
	}
	return &Pacer{
		rate:   float64(unitsPerSecond),
		clock:  clock,
		tokens: float64(unitsPerSecond), // start with a one-second burst
		last:   clock.Now(),
	}
}

// Wait blocks until cost quota units are available, then deducts them. It
// returns the context error if the wait would outlive ctx. A cost larger than
// the bucket's one-second capacity is allowed, at the price of a longer wait.
func (p *Pacer) Wait(ctx context.Context, cost int) error {
	if p == nil || p.rate <= 0 || cost <= 0 {
		return ctx.Err()
	}
	need := float64(cost)

	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		now := p.clock.Now()
		if elapsed := now.Sub(p.last); elapsed > 0 {
			p.tokens += elapsed.Seconds() * p.rate
			p.last = now
		}
		capacity := p.rate
		if need > capacity {
			capacity = need
		}
		if p.tokens > capacity {
			p.tokens = capacity
		}
		if p.tokens >= need {
			p.tokens -= need
			return nil
		}
		wait := time.Duration((need - p.tokens) / p.rate * float64(time.Second))
		if wait <= 0 {
			wait = time.Millisecond
		}
		if err := p.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

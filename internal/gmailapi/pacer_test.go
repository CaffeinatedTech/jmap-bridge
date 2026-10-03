package gmailapi

import (
	"context"
	"testing"
	"time"
)

func TestPacerChargesMethodCosts(t *testing.T) {
	clk := newFakeClock()
	p := NewPacer(10, clk)
	ctx := context.Background()

	// A one-second burst is available immediately.
	if err := p.Wait(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 0 {
		t.Fatalf("initial burst waited: %v", clk.sleeps)
	}
	// The next single unit must wait 1/rate = 100 ms.
	if err := p.Wait(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 100*time.Millisecond {
		t.Fatalf("sleeps after 1 unit = %v, want [100ms]", clk.sleeps)
	}
	// A 25-unit call exceeds the bucket capacity and waits the whole debt.
	if err := p.Wait(ctx, 25); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 2 || clk.sleeps[1] != 2500*time.Millisecond {
		t.Fatalf("sleeps after 25 units = %v, want second 2.5s", clk.sleeps)
	}
}

func TestPacerDisabled(t *testing.T) {
	clk := newFakeClock()
	p := NewPacer(0, clk)
	if err := p.Wait(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 0 {
		t.Fatalf("disabled pacer waited: %v", clk.sleeps)
	}
}

func TestPacerHonoursContext(t *testing.T) {
	clk := newFakeClock()
	p := NewPacer(10, clk)
	// Drain the initial burst so the next call must wait.
	if err := p.Wait(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx, 5); err == nil {
		t.Fatal("pacer waited despite a cancelled context")
	}
}

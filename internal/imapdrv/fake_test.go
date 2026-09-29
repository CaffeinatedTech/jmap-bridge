package imapdrv

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/test/wireimap"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeIMAP wraps the shared wire-level fake for driver tests.
type fakeIMAP struct {
	*wireimap.Server
}

func startFakeIMAP(t *testing.T, caps string, handle func(tag, line string) []string) *fakeIMAP {
	return &fakeIMAP{Server: wireimap.Start(t, caps, handle)}
}

func dialFake(t testing.TB, f *fakeIMAP, cfg Config) *Conn {
	t.Helper()
	if cfg.Logger == nil {
		// Silence the driver's session-up log in test output.
		cfg.Logger = testLogger()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg.Host, cfg.Port = f.Host(), f.Port()
	c, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

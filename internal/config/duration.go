package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration decoded from a TOML string. The grammar is
// Go duration syntax ("5m", "1h30m", "0") plus a convenience "d" suffix
// for whole days ("30d"), which the README's configuration reference
// uses for prefetch_window.
type Duration time.Duration

// UnmarshalTOML decodes a TOML string as a duration.
func (d *Duration) UnmarshalTOML(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("duration must be a string like \"5m\" or \"30d\", got %T", v)
	}
	parsed, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q: expected whole days like \"30d\"", s)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

// String renders the duration in Go syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// Std returns the duration as time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

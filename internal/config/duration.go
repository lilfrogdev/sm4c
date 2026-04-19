package config

import (
	"fmt"
	"time"
)

// Duration is time.Duration with TOML (and any other encoding/text-based
// format) round-tripping. It exists because go-toml v2 does not know how
// to decode "5s" into time.Duration out of the box, and we want the on-
// disk format to be human-readable rather than a nanosecond integer.
//
// Use AsDuration to interoperate with stdlib time APIs.
type Duration time.Duration

// AsDuration returns the underlying time.Duration.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

// String satisfies fmt.Stringer and matches time.Duration.String.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText parses a Go duration string ("5s", "250ms", "1h30m").
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText emits a Go duration string. Kept for round-tripping in
// tests; sm4c does not currently write TOML configs.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

package cli

import (
	"testing"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tui"
)

// TestResolvePollIntervalZeroFallsBackToDefault pins the semantics
// the CLI-level helper applies before handing the cadence to the
// TUI: a zero duration means "unset", and unset should use the
// shipped default so operators who never edit sm4c.toml still get
// live sidebar refreshes. A negative duration is preserved as-is
// because that is the sentinel the TUI treats as "fetch once,
// never poll" — useful for snapshot-only CI runs.
func TestResolvePollIntervalZeroFallsBackToDefault(t *testing.T) {
	t.Parallel()
	if got := resolvePollInterval(0); got != tui.DefaultPollInterval {
		t.Fatalf("zero poll interval = %v; want %v", got, tui.DefaultPollInterval)
	}
}

func TestResolvePollIntervalPositivePassesThrough(t *testing.T) {
	t.Parallel()
	want := 2500 * time.Millisecond
	if got := resolvePollInterval(want); got != want {
		t.Fatalf("positive poll interval = %v; want %v", got, want)
	}
}

func TestResolvePollIntervalNegativePreserved(t *testing.T) {
	t.Parallel()
	want := -1 * time.Second
	if got := resolvePollInterval(want); got != want {
		t.Fatalf("negative poll interval = %v; want %v (caller opted out of polling)", got, want)
	}
}

package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
	"github.com/lilfrogdev/sm4c/internal/tui"
	"github.com/spf13/cobra"
)

// tui_test.go covers the CLI dispatch layer — NOT the Bubble Tea
// runtime itself (that lives in internal/tui and is tested there).
// What we verify here is:
//
//   1. runTUI runs preflight up-front and refuses to launch the TUI
//      when claude is missing, surfacing a readable error rather
//      than handing the terminal to a program that has nothing to
//      offer.
//
//   2. Bare `sm4c` with no args routes through runTUI (and NOT
//      through runLaunch's spawn path).
//
//   3. The runTUIProgram seam receives the initial-highlight hint
//      from openTUI so the launch path (tested indirectly via the
//      unit tests in internal/tui) opens the TUI focused on the
//      freshly-spawned session.
//
// All tests stub runTUIProgram so no real terminal, no real tmux,
// and no real claude are involved.

// TestBareSm4cRoutesToTUI verifies that invoking the root command
// with zero positional args goes through runTUI, not runLaunch.
func TestBareSm4cRoutesToTUI(t *testing.T) {
	t.Parallel()

	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--config", "/sm4c/tests/definitely/does/not/exist.toml"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected bare sm4c with bogus config to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "config") {
		t.Fatalf("error does not mention config load: %v", err)
	}
	// runLaunch-only error fragments: if any of these show up, we
	// took the wrong branch.
	for _, forbidden := range []string{"create claude window"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("bare sm4c incorrectly routed to runLaunch (error contained %q): %v", forbidden, err)
		}
	}
}

// TestRunTUIRefusesWhenClaudeMissing pins the fast-fail contract:
// when preflight cannot resolve claude, runTUI MUST error out with
// a message that names claude BEFORE the TUI is opened.
func TestRunTUIRefusesWhenClaudeMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := dir + "/sm4c.toml"
	if err := os.WriteFile(cfgPath, []byte(`
tmux_bin = "/bin/sh"
claude_bin = "/sm4c/tests/no/such/claude"
`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	calls := 0
	orig := runTUIProgram
	runTUIProgram = func(_ *cobra.Command, _ tmuxctl.OneShot, _ string, _ tui.Focus, _, _ time.Duration) (tui.Model, error) {
		calls++
		return tui.Model{}, nil
	}
	t.Cleanup(func() { runTUIProgram = orig })

	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--config", cfgPath})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected runTUI to fail when claude is missing")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Fatalf("error does not mention claude: %v", err)
	}
	if calls != 0 {
		t.Fatalf("runTUIProgram was called despite preflight failure (calls=%d)", calls)
	}
}

// TestRunTUIPassesEmptyInitialHighlight pins that bare `sm4c` (no
// positional args) calls runTUIProgram with an empty initial
// highlight string. The launch path (tested by TestLaunchPassesSpawnedWindowAsHighlight)
// is the only caller that passes a non-empty value; if this
// invariant flips, bare `sm4c` would start snapping to some stale
// window ID that no longer makes sense.
func TestRunTUIPassesEmptyInitialHighlight(t *testing.T) {
	// Not t.Parallel(): we swap package-level stubs.
	dir := t.TempDir()
	cfgPath := dir + "/sm4c.toml"
	if err := os.WriteFile(cfgPath, []byte(`
tmux_bin = "/bin/sh"
claude_bin = "/bin/sh"
`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	// `go test` detaches stdin, so the TTY gate would refuse to
	// open the TUI. Override the seam for this test.
	origTTY := interactiveStdin
	interactiveStdin = func() bool { return true }
	t.Cleanup(func() { interactiveStdin = origTTY })

	var seenHighlight string
	var seenFocus tui.Focus
	var seenCount int
	origTUI := runTUIProgram
	runTUIProgram = func(_ *cobra.Command, _ tmuxctl.OneShot, initialHighlight string, initialFocus tui.Focus, _, _ time.Duration) (tui.Model, error) {
		seenHighlight = initialHighlight
		seenFocus = initialFocus
		seenCount++
		// Returning a zero Model defaults Action() to ActionNone,
		// which ends the openTUI loop on the first iteration.
		return tui.Model{}, nil
	}
	t.Cleanup(func() { runTUIProgram = origTUI })

	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--config", cfgPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare sm4c returned error: %v", err)
	}
	if seenCount != 1 {
		t.Fatalf("runTUIProgram called %d times; want exactly 1", seenCount)
	}
	if seenHighlight != "" {
		t.Fatalf("bare sm4c called runTUIProgram with initialHighlight = %q; want empty", seenHighlight)
	}
	if seenFocus != tui.FocusSidebar {
		t.Fatalf("bare sm4c called runTUIProgram with initialFocus = %v; want FocusSidebar", seenFocus)
	}
}

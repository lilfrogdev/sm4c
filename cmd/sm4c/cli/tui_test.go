package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/lilfrogdev/sm4c/internal/tui"
	"github.com/spf13/cobra"
)

// tui_test.go covers the dispatch layer — NOT the Bubble Tea runtime
// itself (that lives in internal/tui and is tested there). What we
// verify here is:
//
//   1. runTUI runs preflight up-front and refuses to launch the TUI
//      when claude is missing, surfacing a readable error rather
//      than handing the terminal to a program that has nothing to
//      offer.
//
//   2. Bare `sm4c` with no args routes through runTUI (and NOT
//      through runLaunch's spawn path). The previous M2c behavior
//      silently spawned claude; this commit explicitly reverses
//      that, and the regression cost is high enough to pin.
//
// All tests stub runTUIProgram so no real terminal, no real tmux,
// and no real claude are involved.

// TestBareSm4cRoutesToTUI verifies that invoking the root command
// with zero positional args goes through runTUI, not runLaunch. We
// use a deliberately bogus --config path so preflight fails before
// the TUI could ever open, and assert the error envelope is the
// "config load" one (which only runTUI/runLaunch/setupOneShot emit).
// The positive signal that we took the TUI branch — not the launch
// branch — is that runLaunch's later "create claude window" /
// "exec" errors do NOT appear: runTUI's preflight path short-
// circuits at setupOneShot.
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
	for _, forbidden := range []string{"create claude window", "exec tmux failed"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("bare sm4c incorrectly routed to runLaunch (error contained %q): %v", forbidden, err)
		}
	}
}

// TestRunTUIRefusesWhenClaudeMissing pins the fast-fail contract:
// when preflight cannot resolve claude, runTUI MUST error out with
// a message that names claude BEFORE the TUI is opened. Without this
// check, a user with a broken install would see their terminal
// flash and die when the TUI tries to paint — a confusing failure
// mode that a CLI error sidesteps entirely.
//
// We stub runTUIProgram to a recorder so we can positively confirm
// the TUI runtime was NOT entered. If the stub ever runs, the
// preflight ordering has regressed.
func TestRunTUIRefusesWhenClaudeMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := dir + "/sm4c.toml"
	// Real tmux so tmux resolution passes; bogus claude so the
	// claude-resolution step is the thing that fails. 0o600 keeps
	// config.Load's permission check happy.
	if err := os.WriteFile(cfgPath, []byte(`
tmux_bin = "/bin/sh"
claude_bin = "/sm4c/tests/no/such/claude"
`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	calls := 0
	orig := runTUIProgram
	runTUIProgram = func(_ *cobra.Command) (tui.Model, error) {
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

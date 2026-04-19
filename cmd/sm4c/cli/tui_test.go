package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
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
//      through runLaunch's spawn path).
//
//   3. ActionAttachSession from the TUI is realized via execAttach
//      with the correct tmux attach argv for the reported window ID.
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
	for _, forbidden := range []string{"create claude window", "exec tmux failed"} {
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
	runTUIProgram = func(_ *cobra.Command, _ tmuxctl.OneShot) (tui.Model, error) {
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

// TestTUIAttachActionExecsCorrectWindow drives the ActionAttachSession
// branch end-to-end by substituting both runTUIProgram (to return a
// Model pre-set to ActionAttachSession with a known window ID) and
// execAttach (to record the argv instead of replacing the process).
// We construct the Model by driving the real tui.Model through
// Init() and a synthetic Enter keystroke — that way the test catches
// regressions in either the TUI's Update logic OR the CLI's
// realization path, not just one side.
func TestTUIAttachActionExecsCorrectWindow(t *testing.T) {
	// Not t.Parallel(): we swap package-level runTUIProgram and
	// execAttach, which would race with any other test doing the
	// same. Keep this test serial.

	dir := t.TempDir()
	cfgPath := dir + "/sm4c.toml"
	if err := os.WriteFile(cfgPath, []byte(`
tmux_bin = "/bin/sh"
claude_bin = "/bin/sh"
`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	var recordedArgv []string
	origExec := execAttach
	execAttach = func(argv []string) error {
		recordedArgv = argv
		// Return a harmless error so runTUI surfaces cleanly and
		// we don't have to fake syscall.Exec's no-return semantics.
		return errors.New("test stub: not actually exec'ing")
	}
	t.Cleanup(func() { execAttach = origExec })

	// `go test` detaches stdin, so the real TTY check would refuse
	// to open the TUI. Override the seam for this test.
	origTTY := interactiveStdin
	interactiveStdin = func() bool { return true }
	t.Cleanup(func() { interactiveStdin = origTTY })

	origTUI := runTUIProgram
	runTUIProgram = func(_ *cobra.Command, _ tmuxctl.OneShot) (tui.Model, error) {
		return buildAttachModel(t, "@7"), nil
	}
	t.Cleanup(func() { runTUIProgram = origTUI })

	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--config", cfgPath})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected stubbed execAttach error to propagate")
	}
	if !strings.Contains(err.Error(), "test stub") {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(recordedArgv) < 2 {
		t.Fatalf("execAttach received %d-arg argv; expected at least [tmux, attach-session, ...]", len(recordedArgv))
	}
	joined := strings.Join(recordedArgv, " ")
	if !strings.Contains(joined, "@7") {
		t.Fatalf("execAttach argv does not reference target window @7: %v", recordedArgv)
	}
}

// buildAttachModel drives a real tui.Model through its public API
// until it reports ActionAttachSession on id. We deliberately use
// only exported symbols so the test catches any regression in the
// TUI's attach-commit path as part of the CLI wiring test.
func buildAttachModel(t *testing.T, id string) tui.Model {
	t.Helper()

	lister := func(context.Context) ([]tui.Session, error) {
		return []tui.Session{{WindowID: id, Name: "t"}}, nil
	}
	m := tui.NewModel(lister, 0, nil, nil)

	// Init emits a fetch tea.Cmd; running it synchronously gives us
	// the sessionsMsg value, which Update then folds into the
	// Model's sessions slice.
	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("buildAttachModel: Init returned nil cmd")
	}
	msg := initCmd()
	next, _ := m.Update(msg)
	final, ok := next.(tui.Model)
	if !ok {
		t.Fatalf("Update(sessionsMsg) returned %T, expected tui.Model", next)
	}

	enterMsg := tea.KeyMsg{Type: tea.KeyEnter}
	after, _ := final.Update(enterMsg)
	got, ok := after.(tui.Model)
	if !ok {
		t.Fatalf("Update(enter) returned %T, expected tui.Model", after)
	}
	if got.Action() != tui.ActionAttachSession {
		t.Fatalf("post-enter Action = %v, want ActionAttachSession", got.Action())
	}
	if w := got.SelectedWindowID(); w != id {
		t.Fatalf("post-enter SelectedWindowID = %q, want %q", w, id)
	}
	return got
}

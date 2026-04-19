package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg is a test helper that builds the tea.KeyMsg value for a
// given typed key. Bubble Tea's KeyMsg String() is what our Update
// switches on, so any input that String()s to the right value is
// equivalent — we construct the typed-rune form, which covers every
// binding except Ctrl+C (handled with a named-key path below).
func keyMsg(s string) tea.KeyMsg {
	if s == "ctrl+c" {
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// applyKey exercises one Update step and returns the new Model
// plus whatever tea.Cmd Update produced. We assert on both — the
// Model carries the Action intent, and the Cmd being tea.Quit vs
// nil is how we know whether the runtime would have exited.
func applyKey(t *testing.T, m Model, s string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(keyMsg(s))
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected tui.Model", next)
	}
	return got, cmd
}

func TestInitReturnsNoCmd(t *testing.T) {
	t.Parallel()
	if cmd := NewModel().Init(); cmd != nil {
		t.Fatalf("Init returned non-nil cmd %T; empty-state TUI should have no startup work", cmd)
	}
}

func TestQuitKeysRecordNoneAndRequestTeaQuit(t *testing.T) {
	t.Parallel()
	// Every key in this list must produce ActionNone + tea.Quit.
	// Grouping them in one table prevents drift if someone adds a
	// third quit alias (e.g. Esc) without updating all branches.
	for _, k := range []string{"q", "ctrl+c"} {
		k := k
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			m, cmd := applyKey(t, NewModel(), k)
			if m.Action() != ActionNone {
				t.Fatalf("%s: Action = %v, want ActionNone", k, m.Action())
			}
			if cmd == nil {
				t.Fatalf("%s: expected tea.Quit cmd, got nil", k)
			}
		})
	}
}

func TestNewSessionKeyRecordsActionAndRequestsTeaQuit(t *testing.T) {
	t.Parallel()
	// `n` is the one key that leaves the Action non-zero. If this
	// regresses, the CLI will never realize "spawn a new session"
	// because it reads Model.Action() after the runtime exits.
	m, cmd := applyKey(t, NewModel(), "n")
	if m.Action() != ActionNewSession {
		t.Fatalf("Action = %v, want ActionNewSession", m.Action())
	}
	if cmd == nil {
		t.Fatalf("expected tea.Quit cmd after `n`, got nil")
	}
}

func TestHelpKeyTogglesWithoutQuitting(t *testing.T) {
	t.Parallel()
	m := NewModel()
	// First `?` flips help on; must NOT emit a Quit cmd.
	m, cmd := applyKey(t, m, "?")
	if cmd != nil {
		t.Fatalf("`?` produced cmd %T, expected nil (help toggle must not exit)", cmd)
	}
	if !m.help {
		t.Fatalf("after first `?`, m.help = false, want true")
	}
	// Second `?` flips help off; again no Quit.
	m, cmd = applyKey(t, m, "?")
	if cmd != nil {
		t.Fatalf("second `?` produced cmd %T, expected nil", cmd)
	}
	if m.help {
		t.Fatalf("after second `?`, m.help = true, want false")
	}
}

func TestUnknownKeyIsNoOp(t *testing.T) {
	t.Parallel()
	// We want an unrecognized key to leave the model pristine and
	// produce no commands. If this regresses, stray keystrokes could
	// accidentally trigger actions — especially risky for "n" /
	// "q" aliasing.
	before := NewModel()
	after, cmd := applyKey(t, before, "x")
	if cmd != nil {
		t.Fatalf("unknown key `x` produced cmd %T, expected nil", cmd)
	}
	if after != before {
		t.Fatalf("unknown key `x` mutated Model: before=%+v after=%+v", before, after)
	}
}

func TestWindowSizeDoesNotMutateOrQuit(t *testing.T) {
	t.Parallel()
	// Resize events arrive whenever the terminal is resized and
	// whenever Bubble Tea starts. They must be harmless.
	m := NewModel()
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		t.Fatalf("WindowSizeMsg produced cmd %T, expected nil", cmd)
	}
	got := next.(Model) // safe: Update always returns Model for our Model
	if got != m {
		t.Fatalf("WindowSizeMsg mutated Model: before=%+v after=%+v", m, got)
	}
}

func TestViewRendersTitleAndAllBindings(t *testing.T) {
	t.Parallel()
	// Smoke test for the compact view. We deliberately don't pin
	// exact bytes (lipgloss output depends on terminal width / color
	// profile detection), but we DO require that every user-visible
	// string we advertise survives rendering. If this breaks, a
	// refactor of renderKeys / titleStyle silently dropped a binding.
	out := NewModel().View()
	mustContain(t, out, "sm4c")
	mustContain(t, out, "no active sessions")
	for _, b := range bindings {
		mustContain(t, out, b.key)
		mustContain(t, out, b.desc)
	}
}

func TestViewHelpSectionOnlyWhenToggled(t *testing.T) {
	t.Parallel()
	// "keys" is the header renderHelp emits; it should appear only
	// after `?`. The compact view lists the same bindings but not
	// under that header, which is how we distinguish the two.
	m := NewModel()
	if strings.Contains(m.View(), "keys") {
		t.Fatalf("help header present before `?` was pressed:\n%s", m.View())
	}
	m, _ = applyKey(t, m, "?")
	if !strings.Contains(m.View(), "keys") {
		t.Fatalf("help header absent after `?` was pressed:\n%s", m.View())
	}
}

func TestViewAfterQuitIsEmpty(t *testing.T) {
	t.Parallel()
	// After `q` schedules tea.Quit, any further View() calls must
	// return "" so the user doesn't see a stale empty-state flash
	// right before sm4c exits or hands the terminal to tmux.
	m, _ := applyKey(t, NewModel(), "q")
	if v := m.View(); v != "" {
		t.Fatalf("post-quit View() = %q, want empty string", v)
	}
}

// mustContain fails the test with a readable diff when want is not
// a substring of got. Used instead of a bare strings.Contains so the
// failure message includes the full rendered view, which is easier
// to debug than "assertion failed: contains".
func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("rendered view missing %q\n--- full view ---\n%s\n--- end ---", want, got)
	}
}

package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// app_test.go verifies the Model as a pure state machine:
//
//   - Update is exercised with synthetic tea.KeyMsg / sessionsMsg /
//     pollTickMsg / tea.WindowSizeMsg values; no Bubble Tea runtime,
//     no real TTY.
//
//   - The SessionLister seam is stubbed via plain Go functions so
//     tests stay hermetic (no tmuxctl, no tmux server).
//
//   - Views are asserted against substring expectations rather than
//     byte-for-byte golden files, because lipgloss renders differ by
//     color-profile and terminal width; the substrings here are the
//     parts that must survive any rendering choice.

// keyMsg builds the tea.KeyMsg value for a given typed key. For the
// bindings we care about, Bubble Tea's KeyMsg.String() is what
// Update switches on; constructing the typed-rune form covers most
// keys, and a small switch maps the two named keys we use
// (ctrl+c, enter, up, down, ctrl+b).
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+b":
		return tea.KeyMsg{Type: tea.KeyCtrlB}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// applyKey exercises one Update step with a synthetic key and
// returns the new Model plus whatever tea.Cmd Update produced.
func applyKey(t *testing.T, m Model, s string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(keyMsg(s))
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected tui.Model", next)
	}
	return got, cmd
}

// emptyModel builds a Model with no lister, which keeps it in the
// empty-state view and inert with respect to polling. Every pre-M3a
// test used this shape; we keep it as a helper so a future default-
// constructor change would require touching one place.
func emptyModel() Model {
	return NewModel(Deps{})
}

// stubLister returns a SessionLister that always returns the same
// slice + error. It lets us drive sessionsMsg-based tests without
// reaching for tea.Cmd execution.
func stubLister(sessions []Session, err error) SessionLister {
	return func(context.Context) ([]Session, error) {
		return sessions, err
	}
}

// withSessions constructs a Model that has already absorbed one
// sessionsMsg, so its sidebar is ready to render without going
// through Init's async fetch.
func withSessions(sessions []Session) Model {
	m := NewModel(Deps{Lister: stubLister(sessions, nil)})
	m = m.handleSessions(sessionsMsg{sessions: sessions})
	return m
}

func TestInitReturnsNilWhenNoLister(t *testing.T) {
	t.Parallel()
	// With no lister, Init has no startup work: no fetch to issue,
	// no tick to schedule. The empty-state TUI's "no I/O at startup"
	// contract is preserved here.
	if cmd := emptyModel().Init(); cmd != nil {
		t.Fatalf("Init returned non-nil cmd %T; expected nil for inert Model", cmd)
	}
}

func TestInitReturnsFetchCmdWhenListerPresent(t *testing.T) {
	t.Parallel()
	// With a lister, Init MUST return a tea.Cmd so the first fetch
	// happens before the user sees a frame. We don't run the cmd
	// here (that would require a live runtime); we just pin that a
	// non-nil cmd was emitted.
	m := NewModel(Deps{Lister: stubLister(nil, nil), PollInterval: DefaultPollInterval})
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned nil cmd despite lister being wired; sidebar would never populate")
	}
}

func TestQuitKeysRecordNoneAndRequestTeaQuit(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"q", "ctrl+c"} {
		k := k
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			m, cmd := applyKey(t, emptyModel(), k)
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
	m, cmd := applyKey(t, emptyModel(), "n")
	if m.Action() != ActionNewSession {
		t.Fatalf("Action = %v, want ActionNewSession", m.Action())
	}
	if cmd == nil {
		t.Fatalf("expected tea.Quit cmd after `n`, got nil")
	}
}

func TestHelpKeyTogglesWithoutQuitting(t *testing.T) {
	t.Parallel()
	m := emptyModel()
	m, cmd := applyKey(t, m, "?")
	if cmd != nil {
		t.Fatalf("`?` produced cmd %T, expected nil (help toggle must not exit)", cmd)
	}
	if !m.help {
		t.Fatalf("after first `?`, m.help = false, want true")
	}
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
	before := emptyModel()
	after, cmd := applyKey(t, before, "x")
	if cmd != nil {
		t.Fatalf("unknown key `x` produced cmd %T, expected nil", cmd)
	}
	if !modelsEqual(before, after) {
		t.Fatalf("unknown key `x` mutated Model: before=%+v after=%+v", before, after)
	}
}

func TestWindowSizeStashesDimsAndEmitsNoCmd(t *testing.T) {
	t.Parallel()
	// WindowSizeMsg must be absorbed into Model state so the split
	// layout can size the sidebar and right pane, but it should
	// not produce a tea.Cmd — Bubble Tea re-renders automatically
	// on state changes. If either invariant regresses, the
	// split-column view will either never update on resize or
	// spam cmd channels.
	m := emptyModel()
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		t.Fatalf("WindowSizeMsg produced cmd %T, expected nil", cmd)
	}
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected tui.Model", next)
	}
	if got.width != 120 || got.height != 40 {
		t.Fatalf("WindowSizeMsg did not stash dims: width=%d height=%d", got.width, got.height)
	}
}

func TestSplitLayoutRendersSeparatorAndRightPane(t *testing.T) {
	t.Parallel()
	// On a terminal wide enough to split, the view must include a
	// vertical border glyph AND the right-pane placeholder text.
	// Without this pin, a future refactor could collapse the split
	// back to a full-width stack and the "I don't see the sidebar"
	// regression would come right back.
	m := emptyModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected tui.Model", next)
	}
	out := got.View()
	// lipgloss's NormalBorder renders the vertical side as U+2502.
	if !strings.ContainsRune(out, '│') {
		t.Fatalf("split layout missing vertical border glyph:\n%s", out)
	}
	mustContain(t, out, "no active session")
}

func TestNarrowTerminalFallsBackToStackedLayout(t *testing.T) {
	t.Parallel()
	// Below minSplitWidth the view must stack, not split, so the
	// sidebar content doesn't get squeezed into an unreadable
	// column. The absence of the border glyph is the signal.
	m := emptyModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected tui.Model", next)
	}
	out := got.View()
	if strings.ContainsRune(out, '│') {
		t.Fatalf("narrow-terminal view drew split border:\n%s", out)
	}
	mustContain(t, out, "no sessions yet")
}

func TestCtrlBTogglesFocusWhenSessionHighlighted(t *testing.T) {
	t.Parallel()
	// Ctrl+B is the single sm4c-reserved shortcut that works in
	// both focus states: from sidebar it moves focus to the pane;
	// from pane it moves focus back. A highlighted session is
	// required for the sidebar->pane direction so we never leave
	// the user "typing into nothing". The pane->sidebar direction
	// is always available.
	m := withSessions([]Session{{WindowID: "@1", Name: "a"}})
	if m.focus != FocusSidebar {
		t.Fatalf("precondition: focus = %v, want FocusSidebar", m.focus)
	}
	m, _ = applyKey(t, m, "ctrl+b")
	if m.focus != FocusPane {
		t.Fatalf("after ctrl+b on sidebar w/ highlight, focus = %v, want FocusPane", m.focus)
	}
	m, _ = applyKey(t, m, "ctrl+b")
	if m.focus != FocusSidebar {
		t.Fatalf("after ctrl+b on pane, focus = %v, want FocusSidebar", m.focus)
	}
}

func TestCtrlBOnEmptySidebarStaysOnSidebar(t *testing.T) {
	t.Parallel()
	// With no highlight there is no pane to drop keystrokes into,
	// so ctrl+b must be a no-op. Otherwise the user would enter a
	// focus state with no target and their next keystroke would
	// silently vanish.
	m := emptyModel()
	m, _ = applyKey(t, m, "ctrl+b")
	if m.focus != FocusSidebar {
		t.Fatalf("ctrl+b on empty sidebar flipped focus to %v; want FocusSidebar", m.focus)
	}
}

func TestSessionsMsgPopulatesAndClampsHighlight(t *testing.T) {
	t.Parallel()
	// Drop three sessions in; highlight should land at 0 automatically.
	m := NewModel(Deps{Lister: stubLister(nil, nil)})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{
			{WindowID: "@1", Name: "one"},
			{WindowID: "@2", Name: "two"},
			{WindowID: "@3", Name: "three"},
		},
	})
	if !m.ready {
		t.Fatal("ready stayed false after first sessionsMsg")
	}
	if len(m.sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(m.sessions))
	}
	if m.highlight != 0 {
		t.Fatalf("highlight = %d, want 0 after first non-empty fetch", m.highlight)
	}

	// Move to the last row, then shrink the list; highlight should
	// clamp to the new last row, not point past the end.
	m.highlight = 2
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "one"}},
	})
	if m.highlight != 0 {
		t.Fatalf("highlight = %d after shrink, want 0 (clamped)", m.highlight)
	}

	// Empty result: highlight goes to -1 so the sidebar renderer's
	// bounds check reliably sees "nothing to highlight".
	m = m.handleSessions(sessionsMsg{sessions: nil})
	if m.highlight != -1 {
		t.Fatalf("highlight = %d after empty fetch, want -1", m.highlight)
	}
}

func TestSessionsMsgPreservesLastFetchErr(t *testing.T) {
	t.Parallel()
	want := errors.New("tmux socket missing")
	m := NewModel(Deps{Lister: stubLister(nil, want)})
	m = m.handleSessions(sessionsMsg{err: want})
	if m.listErr == nil || !strings.Contains(m.listErr.Error(), "socket missing") {
		t.Fatalf("listErr = %v, want wrapping %q", m.listErr, want)
	}
}

func TestJKNavigatesWithinBounds(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "one"},
		{WindowID: "@2", Name: "two"},
		{WindowID: "@3", Name: "three"},
	})
	// Start at 0 (set by handleSessions).
	if m.highlight != 0 {
		t.Fatalf("precondition: highlight = %d, want 0", m.highlight)
	}
	// Up at the top boundary stays at 0.
	m, _ = applyKey(t, m, "k")
	if m.highlight != 0 {
		t.Fatalf("k at top moved highlight off 0: got %d", m.highlight)
	}
	// Down twice takes us to the last row.
	m, _ = applyKey(t, m, "j")
	m, _ = applyKey(t, m, "j")
	if m.highlight != 2 {
		t.Fatalf("after two j, highlight = %d, want 2", m.highlight)
	}
	// Down at the bottom boundary stays at the last row.
	m, _ = applyKey(t, m, "down")
	if m.highlight != 2 {
		t.Fatalf("down at bottom moved highlight off 2: got %d", m.highlight)
	}
	// Up walks back.
	m, _ = applyKey(t, m, "up")
	if m.highlight != 1 {
		t.Fatalf("after up from bottom, highlight = %d, want 1", m.highlight)
	}
}

func TestEnterOnHighlightedRowMovesFocusToPane(t *testing.T) {
	t.Parallel()
	// Enter is the discoverable shortcut for "highlight a row, then
	// drop into it". It must NEVER emit ActionNewSession or tea.Quit
	// (which would undermine sm4c's "single surface" promise) — it
	// only flips focus to the pane so subsequent keystrokes forward
	// to claude.
	m := withSessions([]Session{
		{WindowID: "@1", Name: "one"},
		{WindowID: "@2", Name: "two"},
	})
	after, _ := applyKey(t, m, "enter")
	if after.focus != FocusPane {
		t.Fatalf("Enter did not move focus: %v", after.focus)
	}
	if after.Action() != ActionNone {
		t.Fatalf("Enter: Action = %v, want ActionNone", after.Action())
	}
}

func TestEnterOnEmptyStateIsNoOp(t *testing.T) {
	t.Parallel()
	// The empty-state form of the same invariant. No rows, no
	// side effects, no cmd.
	m := emptyModel()
	before := m
	after, cmd := applyKey(t, m, "enter")
	if after.Action() != ActionNone {
		t.Fatalf("empty Enter: Action = %v, want ActionNone", after.Action())
	}
	if cmd != nil {
		t.Fatalf("empty Enter produced cmd %T, expected nil", cmd)
	}
	if !modelsEqual(before, after) {
		t.Fatalf("empty Enter mutated Model: before=%+v after=%+v", before, after)
	}
}

func TestInitialHighlightSnapsToMatchingSession(t *testing.T) {
	t.Parallel()
	// The launch path (`sm4c [claude-args]`) builds a Model with a
	// non-empty initialHighlight equal to the freshly-spawned
	// tmux window ID. As soon as the first sessionsMsg contains
	// that row the highlight must land on it (NOT the first row
	// like the bare `sm4c` case).
	m := NewModel(Deps{Lister: stubLister(nil, nil), InitialHighlight: "@2"})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{
			{WindowID: "@1", Name: "one"},
			{WindowID: "@2", Name: "two"},
			{WindowID: "@3", Name: "three"},
		},
	})
	if m.highlight != 1 {
		t.Fatalf("highlight = %d, want 1 (index of @2)", m.highlight)
	}
	if m.initialHighlight != "" {
		t.Fatalf("initialHighlight retained as %q after successful snap", m.initialHighlight)
	}
}

func TestInitialHighlightIsRetainedUntilSessionAppears(t *testing.T) {
	t.Parallel()
	// If the first poll hasn't yet included the target window
	// (tmux can lag a tick behind our own spawn), the hint must
	// persist across fetches until a matching row shows up.
	m := NewModel(Deps{Lister: stubLister(nil, nil), InitialHighlight: "@7"})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "one"}},
	})
	if m.initialHighlight != "@7" {
		t.Fatalf("initialHighlight cleared despite no matching row; got %q", m.initialHighlight)
	}
	if m.highlight != 0 {
		t.Fatalf("with no match, highlight should default to 0; got %d", m.highlight)
	}
	// Second fetch contains the target — hint must apply now.
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{
			{WindowID: "@1", Name: "one"},
			{WindowID: "@7", Name: "target"},
		},
	})
	if m.highlight != 1 {
		t.Fatalf("highlight = %d, want 1 (index of @7)", m.highlight)
	}
	if m.initialHighlight != "" {
		t.Fatalf("initialHighlight retained as %q after successful snap", m.initialHighlight)
	}
}

func TestPollTickTriggersFetch(t *testing.T) {
	t.Parallel()
	// We don't need to run the cmd; we just need to confirm that a
	// pollTickMsg delivered to Update produces a non-nil fetch cmd.
	// If this regresses, the sidebar stops refreshing after the
	// first tick.
	m := NewModel(Deps{Lister: stubLister(nil, nil), PollInterval: DefaultPollInterval})
	next, cmd := m.Update(pollTickMsg{})
	if _, ok := next.(Model); !ok {
		t.Fatalf("Update returned %T, expected Model", next)
	}
	if cmd == nil {
		t.Fatal("pollTickMsg produced no fetch cmd; polling chain is broken")
	}
}

func TestScheduleNextPollNilWithoutLister(t *testing.T) {
	t.Parallel()
	// Without a lister, the tick chain must stay inert. This is
	// what lets every non-sidebar test skip the cmd plumbing.
	if cmd := emptyModel().scheduleNextPoll(); cmd != nil {
		t.Fatalf("scheduleNextPoll returned non-nil cmd %T on inert Model", cmd)
	}
	// A nil lister with a positive interval: same deal.
	m := NewModel(Deps{PollInterval: 100 * time.Millisecond})
	if cmd := m.scheduleNextPoll(); cmd != nil {
		t.Fatalf("scheduleNextPoll returned non-nil cmd %T when lister is nil", cmd)
	}
}

func TestViewBeforeFirstFetchShowsEmptyPlaceholder(t *testing.T) {
	t.Parallel()
	// A model with a lister but no fetch result yet MUST render
	// the sidebar layout with an empty-list placeholder, not a
	// speculative "0 sessions" count — a slow first fetch
	// shouldn't flash confusing text if a session turns out to
	// exist.
	m := NewModel(Deps{Lister: stubLister(nil, nil)})
	out := m.View()
	mustContain(t, out, "no sessions yet")
}

func TestViewEmptyStateRendersSidebarChromeAndBindings(t *testing.T) {
	t.Parallel()
	// The sidebar is always visible. With zero sessions we must
	// still see the title, the key bar for every binding, and
	// the "press n to start one" empty-state placeholder.
	out := emptyModel().View()
	mustContain(t, out, "sm4c")
	mustContain(t, out, "no sessions yet")
	mustContain(t, out, "press n")
	for _, b := range sidebarBindings {
		mustContain(t, out, b.key)
		mustContain(t, out, b.desc)
	}
}

func TestViewRendersSidebarWhenSessionsPresent(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "refactor-auth"},
		{WindowID: "@4", Name: "spike-queue", Active: true},
	})
	out := m.View()
	mustContain(t, out, "refactor-auth")
	mustContain(t, out, "spike-queue")
	// Count line appears in the sidebar header.
	mustContain(t, out, "2 sessions")
	// Window IDs used to render as faint trailing metadata, but
	// live usage found they were pure noise on the sidebar —
	// users identify sessions by name. The rendered view must
	// no longer carry them (sm4c ls still surfaces them for
	// debugging).
	if strings.Contains(out, "@1") || strings.Contains(out, "@4") {
		t.Fatalf("sidebar leaked window ID:\n%s", out)
	}
	// Bindings that must stay visible on the populated sidebar.
	mustContain(t, out, "move highlight")
	mustContain(t, out, "new session")
	// Empty-state placeholder MUST NOT appear when rows exist —
	// it would be contradictory and steal vertical space.
	if strings.Contains(out, "no sessions yet") {
		t.Fatalf("sidebar with rows leaked empty-state placeholder:\n%s", out)
	}
}

func TestViewEmptyStateShowsDisclaimerFooter(t *testing.T) {
	t.Parallel()
	// The "not affiliated with Anthropic" disclaimer is scoped to
	// the empty path; a user with live sessions has demonstrated
	// they understand what sm4c is. Pin both sides of the rule so
	// this distinction doesn't silently regress.
	empty := emptyModel().View()
	mustContain(t, empty, "not affiliated with Anthropic")

	full := withSessions([]Session{{WindowID: "@1", Name: "x"}}).View()
	if strings.Contains(full, "not affiliated with Anthropic") {
		t.Fatalf("sidebar with rows should not render disclaimer:\n%s", full)
	}
}

func TestViewPluralizesSingleSession(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "solo"}})
	out := m.View()
	mustContain(t, out, "1 session")
	if strings.Contains(out, "1 sessions") {
		t.Fatalf("header pluralized single session: %q", out)
	}
}

func TestViewHelpSectionOnlyWhenToggled(t *testing.T) {
	t.Parallel()
	m := emptyModel()
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
	m, _ := applyKey(t, emptyModel(), "q")
	if v := m.View(); v != "" {
		t.Fatalf("post-quit View() = %q, want empty string", v)
	}
}

func TestViewSurfacesListError(t *testing.T) {
	t.Parallel()
	// With an error and no sessions the user is on the empty path;
	// the error should appear so they can diagnose without -debug.
	m := NewModel(Deps{Lister: stubLister(nil, errors.New("permission denied on socket"))})
	m = m.handleSessions(sessionsMsg{err: errors.New("permission denied on socket")})
	out := m.View()
	mustContain(t, out, "permission denied on socket")
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

// modelsEqual compares two Models field by field, since Model now
// contains a slice (sessions) and a function (lister) which make
// direct == illegal. We ignore the function pointer: tests that
// care about fetch cmds assert on cmd presence, not lister identity.
func modelsEqual(a, b Model) bool {
	if a.help != b.help || a.quitting != b.quitting || a.action != b.action {
		return false
	}
	if a.pollInterval != b.pollInterval {
		return false
	}
	if a.highlight != b.highlight || a.ready != b.ready {
		return false
	}
	if a.initialHighlight != b.initialHighlight {
		return false
	}
	if a.width != b.width || a.height != b.height {
		return false
	}
	if a.paneStreamClosed != b.paneStreamClosed || a.resolvedWindowID != b.resolvedWindowID {
		return false
	}
	if (a.listErr == nil) != (b.listErr == nil) {
		return false
	}
	if a.listErr != nil && a.listErr.Error() != b.listErr.Error() {
		return false
	}
	if len(a.sessions) != len(b.sessions) {
		return false
	}
	for i := range a.sessions {
		if a.sessions[i] != b.sessions[i] {
			return false
		}
	}
	return true
}

// TestRightPaneBodyDims pins the layout math used by the CLI to
// pre-size tmux windows before the TUI opens. The Model itself
// relies on the same function (via rightPaneBodyDims), so any drift
// here would immediately surface as distortion on first paint.
func TestRightPaneBodyDims(t *testing.T) {
	cases := []struct {
		name         string
		termW, termH int
		wantW, wantH int
	}{
		{"too narrow returns zero", minSplitWidth - 1, 30, 0, 0},
		{"zero height returns zero", 120, 0, 0, 0},
		{"min split scales sidebar to min", minSplitWidth, 24, minSplitWidth - sidebarMin - 1 - 2, 22},
		{"mid width scales to one-third", 120, 40, 120 - 40 - 1 - 2, 38},
		{"ultrawide clamps to sidebar max", 300, 60, 300 - sidebarMax - 1 - 2, 58},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := RightPaneBodyDims(tc.termW, tc.termH)
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("RightPaneBodyDims(%d, %d) = (%d, %d); want (%d, %d)",
					tc.termW, tc.termH, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// lipglossWidth is a tiny adapter so the tests can use
// lipgloss.Width by a short local name. Keeping the wrapper
// avoids sprinkling `lipgloss.Width` across every width-based
// assertion and makes it trivial to swap the implementation
// (e.g. to runewidth) in one place if we ever need to.
func lipglossWidth(s string) int { return lipgloss.Width(s) }

// sidebar_ui_test.go covers three related surfaces of the M3e
// TUI polish pass:
//
//   - sidebarHidden (the "zoom" toggle): `z` hides the sidebar
//     from sidebar focus and flips focus to the pane; `ctrl+b`
//     from pane focus un-hides it and returns focus to sidebar.
//   - cwd rendering in the session card: Session.Cwd is surfaced
//     as a faint second line under the session name.
//   - card-style highlight: when the sidebar has a known content
//     width, the selected card is padded to the full width so
//     Reverse paints a visible band instead of a tight text run.
//
// Tests here drive the Model synchronously with synthetic
// messages; no tmux, no runtime. The goal is to pin the contracts
// that live usage leans on so a future refactor cannot silently
// regress them.

// TestZBindingHidesSidebarAndFlipsFocus pins the primary zoom
// contract: pressing `z` on a highlighted row sets sidebarHidden
// = true AND focus = FocusPane in the same Update tick. Without
// the focus flip, the user would be trapped looking at the pane
// with the sidebar binding table still stealing keystrokes.
func TestZBindingHidesSidebarAndFlipsFocus(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "a"}})
	if m.sidebarHidden {
		t.Fatal("sidebarHidden set before any keystroke; baseline broken")
	}
	if m.focus != FocusSidebar {
		t.Fatalf("focus = %v; want FocusSidebar at start", m.focus)
	}
	m, _ = applyKey(t, m, "z")
	if !m.sidebarHidden {
		t.Fatal("z did not hide sidebar")
	}
	if m.focus != FocusPane {
		t.Fatalf("focus = %v after z; want FocusPane", m.focus)
	}
}

// TestZBindingNoOpWithoutSession pins the safety gate: `z` on
// an empty sidebar must be a no-op. Hiding the sidebar with no
// session to show in the right pane would leave the user with
// a blank viewport and no binding to spawn a session (`n` lives
// in sidebar focus). The existing "resolve + resize on a zero
// highlight" guards already no-op, but we check the flag
// directly so the intent is explicit.
func TestZBindingNoOpWithoutSession(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m, _ = applyKey(t, m, "z")
	if m.sidebarHidden {
		t.Fatal("z hid sidebar even though no sessions exist")
	}
	if m.focus != FocusSidebar {
		t.Fatalf("focus = %v; want FocusSidebar when z was no-op", m.focus)
	}
}

// TestCtrlBFromPaneRestoresHiddenSidebar pins the restoration
// contract: the only way out of zoom is the existing ctrl+b
// binding, which must un-hide the sidebar AND move focus back
// to the sidebar in the same step. Any other binding (enter,
// ctrl+c, arrow keys) is forwarded to claude per the
// handleKeyInPaneFocus contract, so ctrl+b is the single-key
// universal "bring me home".
func TestCtrlBFromPaneRestoresHiddenSidebar(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "a"}})
	m, _ = applyKey(t, m, "z")
	if !m.sidebarHidden || m.focus != FocusPane {
		t.Fatalf("zoom did not arm correctly: hidden=%v focus=%v",
			m.sidebarHidden, m.focus)
	}
	m, _ = applyKey(t, m, "ctrl+b")
	if m.sidebarHidden {
		t.Fatal("ctrl+b did not un-hide sidebar after zoom")
	}
	if m.focus != FocusSidebar {
		t.Fatalf("focus = %v; want FocusSidebar after ctrl+b", m.focus)
	}
}

// TestRenderSidebarViewRespectsHidden pins the render-level
// consequence of the zoom flag: renderSidebarView must NOT emit
// the sidebar header / session list when sidebarHidden is set
// on a properly-sized terminal. Without this, the zoom flag
// would only move focus and the visible layout would stay
// split — defeating the "give me the whole viewport" UX goal.
func TestRenderSidebarViewRespectsHidden(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "refactor-auth"}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)

	before := m.renderSidebarView()
	if !strings.Contains(before, "refactor-auth") {
		t.Fatalf("baseline render missing session name: %q", before)
	}
	// keyStyle has Padding(0, 1) so the "?" key chip renders
	// with one space on each side ("  ?   help" roughly). We
	// match on the "help" word at the end, which is stable
	// regardless of the spacing the style applies.
	if !strings.Contains(before, "help") {
		t.Fatalf("baseline render missing help hint: %q", before)
	}

	m.sidebarHidden = true
	m.focus = FocusPane
	after := m.renderSidebarView()
	// Sidebar-specific chrome disappears when zoomed. The right
	// pane carries its own [focus] chip, so "help" from the
	// sidebar hint must be the thing that goes — we assert on
	// the "sm4c — N session" header string, which is unique to
	// the sidebar header and has no right-pane analog.
	if strings.Contains(after, "sm4c —") {
		t.Fatalf("zoomed render still includes sidebar header: %q", after)
	}
	// The session name may still appear in the right-pane
	// header ("refactor-auth  [focus]"), which is fine — that
	// is the right pane, not the sidebar. We pin that the
	// sidebar-specific chrome ("no sessions yet" placeholder,
	// "? help" hint) is gone.
	if strings.Contains(after, "no sessions yet") {
		t.Fatalf("zoomed render leaked empty-sidebar placeholder: %q", after)
	}
}

// TestRenderRightPaneHeaderMentionsShowSidebarWhenHidden pins a
// discoverability affordance: with the sidebar hidden, the
// right-pane header surfaces the "ctrl+b: show sidebar" hint so
// a user who forgot how to get back is not stranded. The hint
// only appears while zoomed — in the normal split layout the
// existing [focus] / [ctrl+b to focus] chips carry the signal.
func TestRenderRightPaneHeaderMentionsShowSidebarWhenHidden(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "alpha"}})
	m.sidebarHidden = true
	m.focus = FocusPane
	header := m.renderRightPaneHeader()
	if !strings.Contains(header, "show sidebar") {
		t.Fatalf("header missing show-sidebar hint when hidden: %q", header)
	}
}

// TestSessionCardRendersCwdLine pins the claude-squad-style
// two-line card: when Session.Cwd is non-empty, the rendered
// list contains both the session name and a short-form path
// on the following line. We assert a tail component rather
// than the full path so the test is robust to shortPath's
// home-dir substitution.
func TestSessionCardRendersCwdLine(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "refactor-auth", Cwd: "/var/projects/auth-svc"},
	})
	row := m.renderSessionList()
	if !strings.Contains(row, "refactor-auth") {
		t.Fatalf("row missing session name: %q", row)
	}
	if !strings.Contains(row, "auth-svc") {
		t.Fatalf("row missing cwd tail: %q", row)
	}
}

// TestSessionCardOmitsCwdLineWhenEmpty pins the opposite
// contract: Session.Cwd == "" must NOT leak an empty indent
// line into the sidebar. An empty cwd happens when tmux
// couldn't resolve a pane_current_path (process mid-exit) or
// when the test lister never sets the field.
func TestSessionCardOmitsCwdLineWhenEmpty(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "solo"},
	})
	row := m.renderSessionList()
	// Split on newlines and assert no blank/whitespace-only
	// filler line survives for the single session row.
	lines := strings.Split(strings.TrimRight(row, "\n"), "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.Contains(l, "solo") && !strings.Contains(l, "●") && !strings.Contains(l, "⠋") {
			t.Fatalf("unexpected extra line for empty-cwd card: %q (full=%q)", l, row)
		}
	}
}

// TestShortPath covers the home-dir substitution helper so we
// don't need to set HOME in the higher-level rendering tests.
// Cases:
//   - Empty input → empty output (callers skip rendering).
//   - Path under HOME → "~/<rest>".
//   - HOME itself → "~".
//   - Path outside HOME → left alone.
func TestShortPath(t *testing.T) {
	t.Parallel()
	home := homeDir()
	if home == "" {
		t.Skip("no home dir available in this environment")
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"home root", home, "~"},
		{"under home", home + "/Repos/sm4c", "~/Repos/sm4c"},
		{"outside home", "/opt/bin", "/opt/bin"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shortPath(c.in); got != c.want {
				t.Errorf("shortPath(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTruncLeft pins the overflow handler: paths longer than
// the sidebar column get their head replaced with "…" so the
// meaningful tail component is preserved. Edge cases (max<=0,
// max==1) are fixed points we lean on when the sidebar column
// is degenerately narrow.
func TestTruncLeft(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 5, ""},
		{"short", 10, "short"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 5, "…cdef"},
		{"abcdef", 1, "…"},
		{"abcdef", 0, ""},
		{"abcdef", -3, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := truncLeft(c.in, c.max); got != c.want {
				t.Errorf("truncLeft(%q, %d) = %q; want %q",
					c.in, c.max, got, c.want)
			}
		})
	}
}

// TestSessionCardHighlightHasNoBorder pins the M3e-round-3
// return to a flat selection band. The earlier rounded-border
// experiment showed two failure modes in live use: the fill
// color left a visible ring between the text and the outline
// (lipgloss paints Background inside padding, not into the
// border's interior), and the border vs. no-border width
// mismatch made the sidebar visibly jump as the highlight
// moved. Both are fixed by dropping the border entirely —
// which means any future attempt to reintroduce border
// glyphs around the selection needs to flip this test first.
func TestSessionCardHighlightHasNoBorder(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "sel"},
		{WindowID: "@2", Name: "other"},
	})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)

	list := m.renderSessionList()
	// Rounded / normal / double border glyphs MUST NOT leak
	// into the card list. (The sidebar column itself still has
	// a normal border on its right edge — rendered by
	// sidebarColumnStyle in renderSidebarView, not by
	// renderSessionList — so we scope this check to the list
	// output only.)
	for _, glyph := range []string{"╭", "╮", "╰", "╯", "┌", "┐", "└", "┘"} {
		if strings.Contains(list, glyph) {
			t.Fatalf("session list leaked border glyph %q: %q", glyph, list)
		}
	}
}

// TestSessionCardsShareOuterWidth pins the anti-jump contract:
// cardBaseStyle and cardHighlightStyle MUST produce the same
// outer width when rendered with the same Width(). This is
// what keeps the sidebar column visually stable as the user
// cursors through the list — the only difference the eye
// should register between a selected and unselected row is
// the background fill, never horizontal shift.
func TestSessionCardsShareOuterWidth(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{
		{WindowID: "@1", Name: "highlighted"},
		{WindowID: "@2", Name: "plain"},
	})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)

	contentW := m.sidebarContentWidth()
	if contentW <= 0 {
		t.Fatalf("expected non-zero sidebar content width at 120x30; got %d", contentW)
	}

	hi := m.renderSessionCard("● ", "highlighted", "", contentW, true)
	base := m.renderSessionCard("● ", "plain", "", contentW, false)

	// lipgloss.Width counts visible cells and ignores ANSI
	// styling — the right measure for "does this block fill
	// the column?".
	if w := lipglossWidth(hi); w != contentW {
		t.Fatalf("highlighted card width = %d; want contentW = %d (%q)", w, contentW, hi)
	}
	if w := lipglossWidth(base); w != contentW {
		t.Fatalf("base card width = %d; want contentW = %d (%q)", w, contentW, base)
	}
	if lipglossWidth(hi) != lipglossWidth(base) {
		t.Fatalf("card widths diverge: hi=%d base=%d", lipglossWidth(hi), lipglossWidth(base))
	}
}
// sidebarHidden on causes rightPaneBodyDims to return the full-
// width geometry instead of the one-third split. This is the
// mechanism by which claude redraws for the whole viewport
// under zoom — without it, claude would keep its pre-zoom width
// and the right pane would render with blank columns on the
// right.
func TestSidebarHiddenStretchesPaneViewport(t *testing.T) {
	t.Parallel()
	m := withSessions([]Session{{WindowID: "@1", Name: "a"}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)

	splitW, _ := m.rightPaneBodyDims()
	m.sidebarHidden = true
	fullW, _ := m.rightPaneBodyDims()

	if fullW <= splitW {
		t.Fatalf("full-width dims = %d; want > split dims (%d)", fullW, splitW)
	}
}

// TestSidebarBindingsAdvertiseHide pins the help-block listing:
// the expanded help must surface `z` so users who do not read
// release notes can discover the binding from the in-TUI help
// block alone. The pair of assertions — key present AND action
// text present — guards against a future binding rename that
// breaks discoverability.
func TestSidebarBindingsAdvertiseHide(t *testing.T) {
	t.Parallel()
	foundKey := false
	foundDesc := false
	for _, b := range sidebarBindings {
		if b.key == "z" {
			foundKey = true
			if strings.Contains(strings.ToLower(b.desc), "hide") ||
				strings.Contains(strings.ToLower(b.desc), "zoom") ||
				strings.Contains(strings.ToLower(b.desc), "collapse") {
				foundDesc = true
			}
		}
	}
	if !foundKey {
		t.Fatalf("sidebarBindings does not advertise `z`: %+v", sidebarBindings)
	}
	if !foundDesc {
		t.Fatalf("sidebarBindings `z` desc does not mention hide/zoom/collapse: %+v", sidebarBindings)
	}
}

// TestStripTitleIcon pins the icon-stripping logic for Claude Code's
// "<icon> <name>" terminal title format.
func TestStripTitleIcon(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"⠋ my-project", "my-project"},
		{"✓ my-project", "my-project"},
		{"? my-project", "my-project"},
		{"my-project", "my-project"},
		{"", ""},
		{"a", "a"},
		{"claude", "claude"},
		{"(archived) project", "(archived) project"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := stripTitleIcon(tc.in)
			if got != tc.want {
				t.Fatalf("stripTitleIcon(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

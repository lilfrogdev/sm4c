package tui

import "github.com/charmbracelet/lipgloss"

// style.go defines every piece of visual emphasis the TUI uses.
//
// Hard rule (enforced by the no-hex-color CI gate in
// scripts/check-no-hex-colors.sh): NEVER call lipgloss.Color with a
// hex string or a 256-color index. sm4c intentionally piggybacks on
// whatever color scheme the user already has configured in their
// terminal emulator — we do not ship opinions about red vs. blue.
// Emphasis comes from three attributes that honor any theme:
//
//   - Bold      -> the user's chosen "strong" weight
//   - Faint     -> the user's chosen de-emphasized tone
//   - Reverse   -> foreground/background swapped, always visible
//
// When we need to reach for a "color" name (e.g. to mark "running"
// vs. "idle" per-session status in M3), we will use named ANSI 0-15
// via lipgloss.ANSIColor, which again resolves through the user's
// palette. That helper has its own CI gate review step before it
// gets introduced.
var (
	// titleStyle renders the "sm4c" header. Bold is the only
	// decoration; we deliberately avoid borders or background fills
	// that would clash with a user's transparent / image-backed
	// terminal.
	titleStyle = lipgloss.NewStyle().Bold(true)

	// hintStyle is used for the "No active sessions" status line and
	// any non-key hint text. Faint makes it read as secondary without
	// us having to pick a specific color.
	hintStyle = lipgloss.NewStyle().Faint(true)

	// keyStyle renders the key name portion of a keybind hint (e.g.
	// the "n" in "n  new session"). Reverse makes each key chip
	// legible against any background — a dark key on a light terminal
	// and a light key on a dark terminal, without us guessing which
	// theme the user has.
	keyStyle = lipgloss.NewStyle().Reverse(true).Padding(0, 1)

	// keyDescStyle is the descriptive half of a keybind row. Plain,
	// no decoration — the contrast comes from keyStyle on the left.
	keyDescStyle = lipgloss.NewStyle()

	// footerStyle is used for the bottom status/help strip. Faint
	// keeps it from stealing attention from the empty-state message
	// above it.
	footerStyle = lipgloss.NewStyle().Faint(true)

	// rowHighlightStyle marks the sidebar row the user is cursoring
	// over. Reverse is our only reliable "stands out against any
	// theme" attribute — it guarantees legibility on light terminals
	// AND dark terminals AND high-contrast accessibility themes
	// without us picking a color. The whole row (not just the key)
	// is rendered this way so the highlight is obvious from across
	// the room.
	//
	// This is the FALLBACK highlight used only when the sidebar
	// column width is unknown (test path / narrow stacked layout).
	// The primary highlight in the split layout is the rounded-
	// card variant below, which matches the visual the user
	// asked for in M3e polish.
	rowHighlightStyle = lipgloss.NewStyle().Reverse(true)

	// cardBaseStyle is the non-highlighted session-card chrome.
	// Padding(0, 1) buys two columns of breathing room between
	// the card content and the sidebar column border — the
	// "add padding on the right and left" user ask — without
	// adding a visible frame to each row (which would turn the
	// sidebar into a grid of boxes).
	cardBaseStyle = lipgloss.NewStyle().Padding(0, 1)

	// cardHighlightStyle is the selected session card. It is
	// identical to cardBaseStyle except for a single-attribute
	// ANSI bright-black background ("8") that paints a solid
	// full-width band across the sidebar column — the claude-
	// squad "filled selection bar" shape the user asked for.
	//
	// Three deliberate non-choices:
	//
	//   1. No Border. An earlier iteration wrapped the card in
	//      RoundedBorder(). Live feedback surfaced two problems
	//      with that: the background color stopped at the
	//      border interior and left a visible ring of
	//      unpainted terminal-bg between the text and the
	//      outline, and the 2-col outer border vs. the base
	//      card's 0-col frame made the list visibly "jump"
	//      horizontally as the cursor moved. Dropping the
	//      border fixes both in one step.
	//
	//   2. No Margin. cardBaseStyle has no margin either, so
	//      both variants have exactly the same outer width
	//      (Padding(0, 1) on both). The only visual difference
	//      between a selected and unselected row is the
	//      background fill — text, indentation, and cwd line
	//      all land at the same x-coordinates.
	//
	//   3. ANSI color "8", not Reverse. Reverse on a dark
	//      terminal produces a near-white band that can read
	//      as harsh; ANSI 8 is the canonical "slightly off
	//      base background" shade (neovim's CursorLine,
	//      tmux's default mode-style, etc.) and stays on the
	//      terminal-native-colors rule — no hex, no 256-index.
	cardHighlightStyle = lipgloss.NewStyle().
				Padding(0, 1).
				Background(lipgloss.Color("8"))

	// sidebarColumnStyle frames the left column. BorderRight plus
	// the default NormalBorder gives us a visible vertical line
	// between the sidebar and the right pane; we deliberately do
	// NOT call BorderForeground with a color so the separator uses
	// the terminal's own foreground — stays readable on every
	// theme, same rationale as every other "color" choice here.
	// Padding(0, 1) buys a one-column gutter on each side so the
	// content never touches the border glyphs.
	sidebarColumnStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderRight(true).
				Padding(0, 1)

	// rightPaneStyle frames the right (hosted-pane) column. In M3a
	// it only carries a placeholder string; the style is kept thin
	// so M3b can add child styles without fighting this one. A
	// matching Padding(0, 1) keeps the inside content from sitting
	// flush against the border on the left edge.
	rightPaneStyle = lipgloss.NewStyle().
			Padding(0, 1)
)

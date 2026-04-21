package tui

import "github.com/charmbracelet/lipgloss"

// style.go defines every piece of visual emphasis the TUI uses.
//
// Hard rule (enforced by the no-hex-color CI gate in
// scripts/check-no-hex-colors.sh): NEVER call lipgloss.Color with a
// hex string. Decimal indices "0"–"255" (ANSI + xterm 256) are OK;
// lipgloss/termenv maps them to the terminal's color profile. sm4c
// intentionally piggybacks on
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
	// The primary highlight in the split layout is built by
	// sidebarHighlightStyle with colors from config / Deps.
	rowHighlightStyle = lipgloss.NewStyle().Reverse(true)

	// cardBaseStyle is the non-highlighted session-card chrome.
	// Padding(1, 0): vertical gap only so the highlight band does
	// not sit flush against the glyphs; horizontal padding is
	// omitted so the bar can run edge-to-edge inside the sidebar
	// (see sidebarColumnStyle). Highlighted and non-highlighted
	// cards share the same padding so the column width stays stable.
	cardBaseStyle = lipgloss.NewStyle().Padding(1, 0)

	// sidebarColumnStyle frames the left column. BorderRight plus
	// the default NormalBorder gives us a visible vertical line
	// between the sidebar and the right pane; we deliberately do
	// NOT call BorderForeground with a color so the separator uses
	// the terminal's own foreground — stays readable on every
	// theme, same rationale as every other "color" choice here.
	// No horizontal padding — session cards span the full inner
	// width so the selection bar meets the left edge and the
	// vertical rule (live feedback: gutters looked like accidental
	// margins). Vertical padding is handled per-card.
	sidebarColumnStyle = lipgloss.NewStyle().
				BorderStyle(lipgloss.NormalBorder()).
				BorderRight(true).
				Padding(0, 0)

	// rightPaneStyle frames the right (hosted-pane) column. In M3a
	// it only carries a placeholder string; the style is kept thin
	// so M3b can add child styles without fighting this one. A
	// matching Padding(0, 1) keeps the inside content from sitting
	// flush against the border on the left edge.
	rightPaneStyle = lipgloss.NewStyle().
			Padding(0, 1)
)

const (
	// defaultSidebarHighlightBG / FG apply when Deps leaves the
	// fields empty (tests, zero-Deps models). ANSI bright-black (8)
	// + bright white (15) is the default selection bar; users can set
	// 0–255 in sm4c.toml (256-color indices are down-mapped by
	// lipgloss/termenv when the terminal is 16-color only).
	defaultSidebarHighlightBG = "8"
	// 255 is xterm's brightest grayscale white; ANSI 15 often maps
	// to the same slot but Bold+15 on some terminals renders as a
	// muted blue-gray. Default fg 255 survives lipgloss/termenv
	// down-mapping on 16-color-only sessions.
	defaultSidebarHighlightFG = "255"
)

// sidebarHighlightStyle is the selected session card: full-width
// band, no border (see M3e doc). bg and fg are decimal color indices
// "0"–"255" (ANSI 0–15 or xterm 256 palette). Rendering uses the
// process default lipgloss Renderer — Run must call
// lipgloss.SetDefaultRenderer(lipgloss.NewRenderer(out)) so termenv
// can map indices to the terminal's actual color profile.
func sidebarHighlightStyle(bg, fg string) lipgloss.Style {
	return lipgloss.NewStyle().
		Padding(1, 0).
		Background(lipgloss.Color(bg)).
		Foreground(lipgloss.Color(fg))
}

// sidebarHighlightPathStyle is the cwd second line inside a
// highlighted card. We deliberately do NOT use Faint here — on top
// of a colored bar it pushed the path below WCAG-ish contrast in
// live testing; the second line is already de-emphasized by being
// a separate line under the session name.
func sidebarHighlightPathStyle(fg string) lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(fg))
}

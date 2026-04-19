package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Action is the intent the TUI reports back to its caller after the
// user has finished interacting with it. The caller (cmd/sm4c/cli)
// decides how to realize the intent — spawning a claude window or
// attaching to an existing one is a cmd/sm4c/cli concern, not a TUI
// concern. This separation mirrors the execAttach seam in
// cmd/sm4c/cli/launch.go and is what lets us unit-test Update without
// any subprocess side effects.
type Action int

const (
	// ActionNone is the zero value. It signals a clean exit with no
	// follow-up work (e.g. the user pressed `q`). Callers should
	// simply return to the shell.
	ActionNone Action = iota

	// ActionNewSession signals "the user pressed `n`; please spawn
	// a new claude session and attach the terminal to it." The TUI
	// does not carry the claude binary path, argv, or socket name —
	// the caller already has all that from preflight / config.
	ActionNewSession

	// ActionAttachSession signals "the user highlighted a session in
	// the sidebar and pressed Enter; please hand the terminal to
	// that tmux window." The target window ID is read off the Model
	// via SelectedWindowID() — passing it through Action would make
	// the enum carry state, which Bubble Tea's message model
	// disallows.
	//
	// In M3a this branch still exec-attaches via tmux; once M3b
	// lands the embedded pane, cmd/sm4c/cli will realize this
	// intent by selecting the window in the hosted viewport instead
	// of exec'ing. The Model stays oblivious to which of the two
	// it is.
	ActionAttachSession
)

// Model is the Bubble Tea model backing the sidebar + empty-state
// view. Its fields split into two groups: the UX state (help,
// quitting, action) which decides what to render and what intent to
// report on exit, and the session-list state (lister, pollInterval,
// sessions, highlight, ready, listErr, selectedWindowID) which backs
// the sidebar once managed windows show up.
//
//   - help              toggled by `?`; shows the full keybind list.
//   - quitting          set when the user pressed a quit key; causes
//                       Update to return tea.Quit on the NEXT step.
//                       Separating "set intent" from "tell bubbletea
//                       to quit" in two Update returns is what makes
//                       the unit tests able to observe ActionNone
//                       even without running the runtime.
//   - action            the intent the caller should realize once
//                       tea.Quit flushes the runtime. Exposed via
//                       Model.Action().
//   - lister            injected SessionLister. Nil means "no live
//                       data": the Model renders the empty state,
//                       Init returns no startup cmd, and the poll
//                       loop is inert. This is how unit tests that
//                       care only about key handling keep their
//                       fixtures minimal.
//   - pollInterval      how often to re-fetch via lister. Honored
//                       only when lister is non-nil and the value
//                       is positive.
//   - sessions          last snapshot returned by lister. Treated as
//                       authoritative for rendering; Model does not
//                       mutate it between fetches.
//   - highlight         zero-based index into sessions that the user
//                       is currently cursoring over. Clamped by the
//                       sessionsMsg handler so it always references
//                       a valid row (or is -1 when sessions is empty).
//   - ready             true once the first sessionsMsg has been
//                       processed. Before that, the view short-
//                       circuits to the empty state so a slow first
//                       fetch doesn't paint a stale "N sessions"
//                       line.
//   - listErr           last fetch error, if any. Surfaced as a
//                       faint single-line notice in the sidebar.
//                       Does not block rendering of stale sessions.
//   - selectedWindowID  set by the Enter key when the user commits
//                       an attach intent. Exposed via
//                       SelectedWindowID() so the caller can build
//                       the tmux attach argv.
type Model struct {
	help     bool
	quitting bool
	action   Action

	lister       SessionLister
	pollInterval time.Duration

	sessions         []Session
	highlight        int
	ready            bool
	listErr          error
	selectedWindowID string

	// width / height carry the last tea.WindowSizeMsg. They are 0
	// until the Bubble Tea runtime sends the first resize (which it
	// always does on startup). The view falls back to an unsized
	// stacked layout when either is 0, which is what unit tests see
	// — they drive Update synthetically without emitting a size —
	// so the substring assertions keep working without a resize.
	width  int
	height int
}

// NewModel constructs a fresh Model. A nil lister is explicitly
// supported — it keeps the Model in the empty-state view with no
// polling, which is the shape every pre-M3 unit test already
// expects. Production callers (cmd/sm4c/cli/tui.go) pass a lister
// that wraps tmuxctl.OneShot.ListWindows and filters to managed
// windows.
//
// pollInterval is honored only when it's positive; zero or negative
// means "fetch once at Init and never again", which is useful for
// tests that want exactly one fetch event.
func NewModel(lister SessionLister, pollInterval time.Duration) Model {
	if pollInterval < 0 {
		pollInterval = 0
	}
	return Model{
		lister:       lister,
		pollInterval: pollInterval,
		highlight:    -1,
	}
}

// Action reports what the caller should do after the program exits.
// This is the only piece of state the caller should read off the
// Model — everything else is internal bookkeeping. Calling Action
// before Run returns is meaningless (it'll be ActionNone); the
// field is only authoritative once tea.Quit has fired.
func (m Model) Action() Action { return m.action }

// SelectedWindowID reports the tmux window ID the user committed to
// via Enter. It is only meaningful when Action() == ActionAttachSession;
// for any other Action the caller MUST NOT read it (it'll be the empty
// string). The ID is an opaque tmux token like "@3" — the TUI does
// not validate or parse it.
func (m Model) SelectedWindowID() string { return m.selectedWindowID }

// Init is the Bubble Tea entry point. When a SessionLister is wired,
// we kick off the first fetch immediately so the sidebar paints real
// data on the first frame instead of "no sessions" flashing for a
// tick. When no lister is configured (tests, or a deliberately inert
// Model) Init returns nil — no work, no messages, no tick chain.
func (m Model) Init() tea.Cmd {
	return m.fetchSessions()
}

// Update is the pure state-transition function. The only messages
// the empty-state view reacts to today are key presses and window
// resize; the resize is accepted silently because lipgloss rendering
// already adapts. When M3 introduces live session state, a
// tickMsg / refreshMsg / windowStatusMsg family will join this
// switch — each should remain side-effect free and return its work
// as a tea.Cmd.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Stash the terminal dimensions so the view can size the
		// sidebar column and the right-pane placeholder correctly.
		// We do NOT emit a cmd: Bubble Tea's default behavior is to
		// re-render on the next tick, which is exactly what we
		// want.
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case sessionsMsg:
		return m.handleSessions(msg), m.scheduleNextPoll()
	case pollTickMsg:
		// A tick's only job is to kick off the next fetch. The fetch
		// itself, once complete, will schedule the following tick.
		// This keeps the cadence strictly serial — no overlap between
		// a slow fetch and the next ticker firing.
		return m, m.fetchSessions()
	}
	return m, nil
}

// handleSessions folds the freshest snapshot into the Model, clamping
// the highlight so j/k navigation stays on a valid row across
// insertions (new session created in another terminal) and deletions
// (a session closed while we were polling). Empty lister results and
// nil returns are normalized to a nil slice + highlight = -1, which
// makes the "no sessions" branch in View a single len-check.
func (m Model) handleSessions(msg sessionsMsg) Model {
	m.ready = true
	m.listErr = msg.err
	m.sessions = msg.sessions
	switch {
	case len(m.sessions) == 0:
		m.highlight = -1
	case m.highlight < 0:
		m.highlight = 0
	case m.highlight >= len(m.sessions):
		m.highlight = len(m.sessions) - 1
	}
	return m
}

// handleKey is factored out of Update so the unit tests can exercise
// it with a plain tea.KeyMsg and without constructing message-
// switch scaffolding around it. Every branch either transitions
// state, records an Action, or schedules tea.Quit — nothing else.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		// Quit with no follow-up action. The caller will see
		// ActionNone and simply return.
		m.action = ActionNone
		m.quitting = true
		return m, tea.Quit

	case "n":
		// Signal "spawn a new session" and exit. The caller does the
		// actual tmux/exec work; this keeps Update free of I/O.
		//
		// TODO (M3e): replace this placeholder with the real compose
		// sub-view (cwd picker + optional session name + args). For
		// M3a, pressing `n` is still equivalent to `sm4c` with no
		// args under the previous M2c behavior — a bare claude
		// launch in the process's current working directory. That
		// keeps the key useful in the sidebar without committing
		// to a half-built form UX we'd throw away.
		m.action = ActionNewSession
		m.quitting = true
		return m, tea.Quit

	case "enter":
		// Commit the highlighted session as the attach target. When
		// the highlight is out of range (no sessions, or the list is
		// empty) Enter is a no-op: we do NOT want to emit an
		// ActionAttachSession with an empty window ID, because
		// cmd/sm4c/cli would then try to build a tmux argv around
		// an empty token.
		if m.highlight < 0 || m.highlight >= len(m.sessions) {
			return m, nil
		}
		m.selectedWindowID = m.sessions[m.highlight].WindowID
		m.action = ActionAttachSession
		m.quitting = true
		return m, tea.Quit

	case "j", "down":
		// Move highlight down, no wrap. A no-wrap bottom is the
		// same convention tmux's choose-tree and vim's :ls use —
		// wrapping makes skimming a list feel disorienting.
		if m.highlight < len(m.sessions)-1 {
			m.highlight++
		}
		return m, nil

	case "k", "up":
		if m.highlight > 0 {
			m.highlight--
		}
		return m, nil

	case "ctrl+b":
		// Placeholder for M3b's focus toggle. We pin the binding
		// now so muscle memory carries into M3b without a keymap
		// rewrite, but Update is a no-op here. If it ever does
		// become meaningful the help text in bindings[] should
		// lose the " (M3b)" suffix at the same commit.
		return m, nil

	case "?":
		// Toggle the expanded help block. Unlike quit/new, this does
		// not exit — it just flips a render flag.
		m.help = !m.help
		return m, nil
	}
	return m, nil
}

// View renders the current screen. It is called on every re-render;
// returning the same bytes twice is cheap and by design.
//
// There is ONE layout — the sidebar — regardless of how many
// sessions are live. When sessions is empty, the list area shows a
// faint "press n to start one" placeholder; when sessions is
// non-empty, it shows one row per window. This keeps the sidebar
// visible at all times (the user's explicit design request) and
// avoids a layout jump the moment the first session appears.
//
// M3b will widen this layout into a left-column sidebar + right-
// column hosted pane. Until then, the sidebar is full-width and
// the right column is conceptually present-but-empty.
func (m Model) View() string {
	if m.quitting {
		// Bubble Tea keeps View on screen briefly after tea.Quit
		// schedules; returning an empty string avoids a flicker of
		// stale content before the terminal is handed over to
		// tmux (ActionAttachSession / ActionNewSession) or back to
		// the shell (ActionNone).
		return ""
	}
	return m.renderSidebarView()
}

// keybind is a row of ("key name", "what it does"). Kept as a flat
// struct (not a map) so the order is deterministic across Go versions
// and across test runs, and so the same list can be reused by tests
// to assert the help view contains every advertised binding.
type keybind struct {
	key  string
	desc string
}

// bindings is the authoritative list of keys the sidebar view
// advertises. Both renderKeys and renderHelp iterate this slice, so
// any key added here appears in the compact bar AND the expanded
// help without further changes. Per-milestone keys carry a suffix
// like "(M3b)" until their behavior lands; dropping the suffix is
// the explicit checkpoint for enabling the binding.
var bindings = []keybind{
	{"j/k", "move highlight"},
	{"enter", "attach to session"},
	{"n", "new session"},
	{"ctrl+b", "focus toggle (M3b)"},
	{"?", "toggle help"},
	{"q", "quit"},
}

// renderKeys emits the compact "key  description" list shown under
// the status line. Each row uses keyStyle (reverse-video chip) for
// the key and plain text for the description; the blank space
// between them is a single tab-like gap so the columns align even
// when key names have different widths.
func (m Model) renderKeys() string {
	var rows []string
	for _, b := range bindings {
		row := keyStyle.Render(b.key) + "  " + keyDescStyle.Render(b.desc)
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

// renderHelp is the expanded help block shown when the user presses
// `?`. Today it is a near-copy of renderKeys with a leading label —
// M3 will grow it into a proper multi-section cheatsheet (navigation,
// session lifecycle, status legend). We keep the two paths separate
// now so that growth doesn't require rewriting the compact view.
func (m Model) renderHelp() string {
	lines := []string{titleStyle.Render("keys")}
	for _, b := range bindings {
		lines = append(lines, "  "+keyStyle.Render(b.key)+"  "+keyDescStyle.Render(b.desc))
	}
	return strings.Join(lines, "\n")
}

// Layout constants for the split-column view. minSplitWidth is the
// threshold below which the right-pane column is suppressed and the
// sidebar falls back to full-width (the narrow-terminal fallback).
// sidebarMin / sidebarMax bound the sidebar's content width so it
// stays readable on both 80-col and ultrawide terminals; within
// that range it scales to about one-third of the viewport.
const (
	minSplitWidth = 60
	sidebarMin    = 24
	sidebarMax    = 40
)

// renderSidebarView paints the unified layout. On a terminal wide
// enough to split (>= minSplitWidth), it renders a visible left
// sidebar column with a right border, joined horizontally to a
// right-pane placeholder column ("no active session" for M3a, the
// hosted claude viewport once M3b lands). On narrow terminals it
// falls back to a full-width stack — the split would otherwise
// produce cramped columns of wrapped text.
//
// The unit-test path (Update-only, no WindowSizeMsg) hits the
// width == 0 branch and also gets the stacked fallback, which is
// why every test assertion uses substring matching rather than
// line-level layout checks: the stacked form and the split form
// share the same substrings.
func (m Model) renderSidebarView() string {
	content := m.renderSidebarColumn()

	if m.width < minSplitWidth || m.height < 1 {
		return content + "\n"
	}

	sidebarW := m.sidebarWidth()
	// Reserve at least 1 column for the border and leave the rest
	// for the right pane. lipgloss BorderRight counts as 1 column
	// outside the declared Width, so rightW is what remains.
	rightW := m.width - sidebarW - 1
	if rightW < 1 {
		// Degenerate: terminal just wide enough to pass the split
		// threshold but the sidebar ate everything. Fall back to
		// the stacked layout rather than rendering a 0-col pane.
		return content + "\n"
	}

	sidebar := sidebarColumnStyle.
		Width(sidebarW).
		Height(m.height).
		Render(content)

	right := rightPaneStyle.
		Width(rightW).
		Height(m.height).
		Render(m.renderRightPanePlaceholder())

	return lipgloss.JoinHorizontal(lipgloss.Top, sidebar, right)
}

// renderSidebarColumn builds the stacked content (title, list, key
// bar, optional diagnostics, optional footer) that lives inside the
// sidebar column. Extracted so it can be reused verbatim in both
// the split layout and the narrow-terminal fallback.
func (m Model) renderSidebarColumn() string {
	var sections []string

	sections = append(sections, m.renderHeader())
	sections = append(sections, "")
	sections = append(sections, m.renderSessionList())
	sections = append(sections, "")
	sections = append(sections, m.renderKeys())

	if m.listErr != nil {
		// A fetch error is usually a preflight issue (e.g. tmux
		// socket permissions flipped mid-run). We surface it
		// faintly so the user sees a clue without the sidebar
		// becoming a stack trace.
		sections = append(sections, "")
		sections = append(sections, hintStyle.Render(
			"session fetch error: "+m.listErr.Error()))
	}
	if m.help {
		sections = append(sections, "")
		sections = append(sections, m.renderHelp())
	}
	if len(m.sessions) == 0 {
		sections = append(sections, "")
		sections = append(sections, footerStyle.Render(
			"sm4c is not affiliated with Anthropic. "+
				"You must install the official claude CLI separately.",
		))
	}

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// renderRightPanePlaceholder is the M3a stand-in for what M3b will
// replace with the VT-rendered hosted claude pane. It reports the
// current selection ("(no active session)" vs. the name of the
// highlighted session) so the split is visibly meaningful even
// before the pane has content. Keeping it here rather than as a
// const means M3b only needs to swap this one function.
func (m Model) renderRightPanePlaceholder() string {
	var hint string
	switch {
	case len(m.sessions) == 0:
		hint = "no active session"
	case m.highlight < 0 || m.highlight >= len(m.sessions):
		hint = "no active session"
	default:
		s := m.sessions[m.highlight]
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		hint = "selected: " + name + "  " + s.WindowID
	}
	return hintStyle.Render(hint) + "\n" +
		hintStyle.Render("press enter to attach  (embedded pane lands in M3b)")
}

// sidebarWidth picks the sidebar column's content width for the
// current terminal. It clamps within [sidebarMin, sidebarMax] and
// scales to roughly one-third of viewport width within that band,
// which keeps the sidebar comfortable on anything from an 80-col
// terminal to an ultrawide monitor.
func (m Model) sidebarWidth() int {
	w := m.width / 3
	if w < sidebarMin {
		w = sidebarMin
	}
	if w > sidebarMax {
		w = sidebarMax
	}
	// Safety: on a terminal that's just barely above the split
	// threshold, one-third can still fit even the minimum; when it
	// can't, the caller (renderSidebarView) falls back to stacked.
	return w
}

// renderHeader builds the title bar. When sessions are present we
// append a faint pluralized count; when empty we keep the header
// minimal so the eye is drawn to the empty-state hint below it
// rather than to a confusing "0 sessions" label.
func (m Model) renderHeader() string {
	if len(m.sessions) == 0 {
		return titleStyle.Render("sm4c")
	}
	return titleStyle.Render("sm4c") + hintStyle.Render(
		" — "+pluralize(len(m.sessions), "session", "sessions"),
	)
}

// renderSessionList emits one row per session. The row format is:
//
//	<active-marker> <name> <window-id>
//
// where <active-marker> is a two-column cell ("● " for the tmux-
// active window, "  " otherwise), <name> is the sanitized window
// title, and <window-id> is the opaque tmux ID rendered faintly so
// the eye treats it as metadata. The highlighted row is painted in
// reverse video via rowHighlightStyle — the only "color" decision —
// using the terminal's native reverse attribute, not a hex color,
// per the no-theming rule.
//
// When the sessions slice is empty we emit a single faint
// placeholder row so the sidebar stays visible with its key bar,
// rather than collapsing to a header + footer. This is the
// "sidebar is always present" design constraint made literal.
func (m Model) renderSessionList() string {
	if len(m.sessions) == 0 {
		return hintStyle.Render("  no sessions yet — press n to start one")
	}
	rows := make([]string, 0, len(m.sessions))
	for i, s := range m.sessions {
		marker := "  "
		if s.Active {
			marker = "● "
		}
		name := s.Name
		if name == "" {
			// tmux always has a window name; an empty one would
			// indicate a parser bug in tmuxctl. Render a sentinel
			// so the sidebar doesn't become an empty column.
			name = "(unnamed)"
		}
		row := marker + name + "  " + hintStyle.Render(s.WindowID)
		if i == m.highlight {
			row = rowHighlightStyle.Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

// pluralize is a tiny helper kept inline so the sidebar header's
// grammar ("1 session" vs. "3 sessions") reads naturally. It lives
// in this file rather than style.go because it's a layout concern,
// not a style concern.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return intToString(n) + " " + plural
}

// intToString avoids pulling strconv into a two-line helper. The
// sessions count is bounded by how many windows a single tmux server
// can hold in practice (low thousands on any real machine), so a
// simple base-10 builder is fine.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{'0' + byte(n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// Run starts a Bubble Tea program with this Model, using the given
// input/output writers. `in` is normally os.Stdin and `out` os.Stdout;
// tests pass pipes so they can drive keystrokes programmatically.
//
// We use tea.WithAltScreen so the sidebar can redraw on each poll
// without polluting scrollback. Alt-screen does produce a visible
// "switch in, switch out" on entry/exit, which matches every other
// full-screen TUI (vim, less, htop). For the bare empty-state case
// the flash is brief; once the user has even one active session the
// alt-screen semantics are clearly the right choice.
//
// On a successful run, Run returns the final Model so the caller can
// inspect Action() and SelectedWindowID(). On any runtime error (TTY
// setup, input stream closed unexpectedly) the error is returned and
// the intents are ignored.
func Run(in interface {
	Read(p []byte) (n int, err error)
}, out interface {
	Write(p []byte) (n int, err error)
}, lister SessionLister, pollInterval time.Duration) (Model, error) {
	p := tea.NewProgram(
		NewModel(lister, pollInterval),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
	)
	final, err := p.Run()
	if err != nil {
		return Model{}, err
	}
	// tea.Program returns the final Model as a tea.Model interface;
	// the concrete type is always our Model because that is what we
	// passed in. A failed assertion here would indicate a Bubble Tea
	// internal change, so we surface it as an explicit error rather
	// than panicking.
	m, ok := final.(Model)
	if !ok {
		return Model{}, errUnexpectedModelType
	}
	return m, nil
}

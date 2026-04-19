package tui

import (
	"strings"

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
)

// Model is the Bubble Tea model backing the empty-state view. It is
// deliberately anemic in this milestone; M3 will grow it into a real
// sidebar + per-session status machine. Every field here has a plan
// for when it becomes useful:
//
//   - help      toggled by `?`; shows the full keybind list.
//   - quitting  set when the user pressed a quit key; causes Update
//               to return tea.Quit on the NEXT step. Separating
//               "set intent" from "tell bubbletea to quit" in two
//               Update returns is what makes the unit tests able to
//               observe ActionNone even without running the runtime.
//   - action    the intent the caller should realize once tea.Quit
//               flushes the runtime. Exposed via Model.Action().
type Model struct {
	help     bool
	quitting bool
	action   Action
}

// NewModel constructs a fresh Model. We don't take any dependencies
// yet (no tmuxctl handle, no config) because the empty-state TUI has
// no live data to render. When M3 adds the sidebar, it'll accept a
// snapshot of managed windows and a refresh channel.
func NewModel() Model {
	return Model{}
}

// Action reports what the caller should do after the program exits.
// This is the only piece of state the caller should read off the
// Model — everything else is internal bookkeeping. Calling Action
// before Run returns is meaningless (it'll be ActionNone); the
// field is only authoritative once tea.Quit has fired.
func (m Model) Action() Action { return m.action }

// Init is the Bubble Tea entry point. We have no startup work — no
// timers, no subprocess probes, nothing to stream — so we return nil.
// (tea.Cmd of nil is the canonical "do nothing" value.)
func (m Model) Init() tea.Cmd { return nil }

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
		// Current view is centered and self-adapting; we don't need
		// to stash width/height until we add multi-column layout.
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
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
		// TODO (M3): this is the placeholder flow. The real design
		// is a small sub-view that prompts for a target working
		// directory and an optional session name (claude's `-n`),
		// then realizes it. Until M3 lands, `n` here is equivalent
		// to the shell shortcut `sm4c` with no args — i.e. a bare
		// claude launch. That keeps the behavior useful without
		// committing to a half-built form-field UX we'd throw away.
		m.action = ActionNewSession
		m.quitting = true
		return m, tea.Quit

	case "?":
		// Toggle the expanded help block. Unlike quit/new, this does
		// not exit — it just flips a render flag.
		m.help = !m.help
		return m, nil
	}
	return m, nil
}

// View renders the empty-state screen. It is called on every
// re-render; returning the same bytes twice is cheap and by design
// (no animations, no transitions).
func (m Model) View() string {
	if m.quitting {
		// Bubble Tea keeps View on screen briefly after tea.Quit
		// schedules; returning an empty string avoids a flicker of
		// stale content before the terminal is handed over to
		// tmux (ActionNewSession) or back to the shell
		// (ActionNone).
		return ""
	}

	var sections []string

	sections = append(sections, titleStyle.Render("sm4c"))
	sections = append(sections, hintStyle.Render("no active sessions"))
	sections = append(sections, "")
	sections = append(sections, m.renderKeys())

	if m.help {
		sections = append(sections, "")
		sections = append(sections, m.renderHelp())
	}

	sections = append(sections, "")
	sections = append(sections, footerStyle.Render(
		"sm4c is not affiliated with Anthropic. "+
			"You must install the official claude CLI separately.",
	))

	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// keybind is a row of ("key name", "what it does"). Kept as a flat
// struct (not a map) so the order is deterministic across Go versions
// and across test runs, and so the same list can be reused by tests
// to assert the help view contains every advertised binding.
type keybind struct {
	key  string
	desc string
}

// bindings is the authoritative list of keys the empty-state view
// advertises. When M3 adds more, they go here so the render paths
// and the help view stay in sync automatically.
var bindings = []keybind{
	{"n", "new session"},
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

// Run starts a Bubble Tea program with this Model, using the given
// input/output writers. `in` is normally os.Stdin and `out` os.Stdout;
// tests pass pipes so they can drive keystrokes programmatically.
//
// We intentionally do NOT use tea.WithAltScreen: the empty-state view
// is a one-pager and using alt-screen would produce a confusing
// "terminal flashed, then flashed back" effect on exit. When M3 adds
// the sidebar / pane layout, that decision will be revisited in the
// same commit as the layout change.
//
// On a successful run, Run returns the final Model so the caller can
// inspect Action(). On any runtime error (TTY setup, input stream
// closed unexpectedly) the error is returned and Action is ignored.
func Run(in interface {
	Read(p []byte) (n int, err error)
}, out interface {
	Write(p []byte) (n int, err error)
}) (Model, error) {
	p := tea.NewProgram(NewModel(), tea.WithInput(in), tea.WithOutput(out))
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

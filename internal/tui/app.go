package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Action is the intent the TUI reports back to its caller after the
// user has finished interacting with it. The caller (cmd/sm4c/cli)
// decides how to realize the intent — spawning a claude window is a
// cmd/sm4c/cli concern, not a TUI concern. This separation is what
// lets us unit-test Update without any subprocess side effects.
//
// Note: an "attach" action intentionally does not exist. The whole
// point of sm4c is that the user never leaves the TUI to reach a
// session; attaching is instead modeled as "highlight the row, let
// the right pane render the session, route keystrokes when focus
// lands there." Until input routing ships (M3c), the right pane is
// read-only, but there is no shortcut that execs into tmux — that
// escape hatch would undermine the single-surface design.
type Action int

const (
	// ActionNone is the zero value. It signals a clean exit with no
	// follow-up work (e.g. the user pressed `q`). Callers should
	// simply return to the shell.
	ActionNone Action = iota

	// ActionNewSession signals "the user pressed `n`; please spawn
	// a new claude session." The TUI does not carry the claude
	// binary path, argv, or socket name — the caller already has
	// all that from preflight / config. After spawning, the caller
	// is expected to re-enter the TUI with the new window as the
	// initial highlight so the user lands on the freshly-created
	// session; that re-entry loop lives in cmd/sm4c/cli/tui.go.
	ActionNewSession
)

// Model is the Bubble Tea model backing the sidebar view. Its
// fields split into two groups: the UX state (help, quitting,
// action) which decides what to render and what intent to report on
// exit, and the session-list state (lister, pollInterval, sessions,
// highlight, ready, listErr, initialHighlight) which backs the
// sidebar once managed windows show up.
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
//   - initialHighlight  optional tmux window ID the Model should
//                       snap the highlight to as soon as the first
//                       sessionsMsg contains a matching row. Used
//                       by the launch path so `sm4c [claude-args]`
//                       opens the TUI with the freshly-spawned
//                       session pre-selected. Cleared once applied
//                       so later navigation isn't overridden on the
//                       next poll.
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
	initialHighlight string

	// width / height carry the last tea.WindowSizeMsg. They are 0
	// until the Bubble Tea runtime sends the first resize (which it
	// always does on startup). The view falls back to an unsized
	// stacked layout when either is 0, which is what unit tests see
	// — they drive Update synthetically without emitting a size —
	// so the substring assertions keep working without a resize.
	width  int
	height int

	// paneEvents / paneResolver are the M3b.1 dependencies. Both are
	// optional (nil means "no pane preview"); the right pane degrades
	// to a static "preview unavailable" line in that case. See
	// panes.go for the dependency contracts.
	paneEvents   <-chan PaneEvent
	paneResolver ActivePaneResolver

	// paneStreamClosed records that the upstream channel has been
	// closed (the tmuxctl.Client terminated). Once set, the right
	// pane shows a "preview disconnected" hint and the Model stops
	// arming further reads.
	paneStreamClosed bool

	// paneByWindow caches the ActivePaneResolver answer for each
	// highlighted window ID. Populated by paneResolvedMsg; cleared
	// by sessionsMsg when the owning window disappears. Keeping
	// the mapping per-window (rather than per-session-index) means
	// a session being inserted above the highlighted one does not
	// invalidate the cached pane ID.
	paneByWindow map[string]string

	// paneErrByWindow stores the last resolver error per window so
	// the right pane can show "(pane lookup failed: …)" for the
	// highlighted selection without hiding live data for other
	// sessions.
	paneErrByWindow map[string]error

	// paneTerminals holds a VT emulator for every pane the stream
	// has delivered bytes for. Keyed by pane ID (as produced by
	// ActivePaneResolver / %output). Emulators are created lazily on
	// first write at the current right-pane body dimensions; there
	// is no eviction because the number of live panes is bounded by
	// the number of sm4c-managed windows (typically tens).
	paneTerminals map[string]*paneTerminal

	// paneViewW / paneViewH track the dimensions every pane
	// emulator has been sized to. They are derived from the
	// right-pane body geometry on each tea.WindowSizeMsg, and any
	// newly-minted paneTerminal is constructed at these dims so a
	// pane that first appears after a resize is not stuck at the
	// default 80x24.
	paneViewW int
	paneViewH int

	// resolvedWindowID is the last window ID we issued a resolver
	// call for. Used to debounce: if j/k navigates to a row whose
	// pane we already resolved, we skip the resolver round-trip.
	resolvedWindowID string
}

// NewModel constructs a fresh Model. Every dependency is optional:
//
//   - A nil SessionLister keeps the Model in the empty-state view
//     with no polling, which is the shape every unit test that does
//     not care about session data already expects.
//   - A nil PaneEventStream / ActivePaneResolver disables the M3b.1
//     pane preview; the right pane shows a "(pane preview
//     unavailable)" hint instead of live bytes.
//   - An empty initialHighlight lets the Model pick the first row
//     (index 0) as the default highlight once sessions arrive. A
//     non-empty value is interpreted as a tmux window ID; the Model
//     snaps the highlight to that row on the first sessionsMsg that
//     contains it, then forgets the hint.
//
// Production callers (cmd/sm4c/cli/tui.go) pass a lister that wraps
// tmuxctl.OneShot.ListWindows, a stream backed by
// tmuxctl.Client.Events(), and a resolver backed by
// tmuxctl.OneShot.ActivePane.
//
// pollInterval is honored only when it's positive; zero or negative
// means "fetch once at Init and never again", which is useful for
// tests that want exactly one fetch event.
func NewModel(
	lister SessionLister,
	pollInterval time.Duration,
	paneStream PaneEventStream,
	paneResolver ActivePaneResolver,
	initialHighlight string,
) Model {
	if pollInterval < 0 {
		pollInterval = 0
	}
	m := Model{
		lister:           lister,
		pollInterval:     pollInterval,
		highlight:        -1,
		initialHighlight: initialHighlight,
		paneResolver:     paneResolver,
		paneByWindow:     make(map[string]string),
		paneErrByWindow:  make(map[string]error),
		paneTerminals:    make(map[string]*paneTerminal),
		paneViewW:        defaultPaneWidth,
		paneViewH:        defaultPaneHeight,
	}
	if paneStream != nil {
		m.paneEvents = paneStream()
	}
	return m
}

// Action reports what the caller should do after the program exits.
// This is the only piece of state the caller should read off the
// Model — everything else is internal bookkeeping. Calling Action
// before Run returns is meaningless (it'll be ActionNone); the
// field is only authoritative once tea.Quit has fired.
func (m Model) Action() Action { return m.action }

// Init is the Bubble Tea entry point. It kicks off both concurrent
// streams the Model depends on:
//
//   - The session fetch (sessionsMsg chain), so the sidebar paints
//     real data on the first frame rather than flashing "no sessions"
//     for one tick.
//   - The pane event reader (paneDataMsg / paneStreamClosedMsg chain),
//     so the right pane starts buffering bytes as soon as tmux emits
//     them.
//
// Either dependency may be nil, in which case its chain is simply
// absent. This is what keeps the unit tests — which run with no
// lister, no stream, no resolver — from needing to drive message
// plumbing they do not care about.
func (m Model) Init() tea.Cmd {
	fetch := m.fetchSessions()
	pane := m.waitForPaneEvent()
	switch {
	case fetch != nil && pane != nil:
		return tea.Batch(fetch, pane)
	case fetch != nil:
		return fetch
	case pane != nil:
		return pane
	}
	return nil
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
		// sidebar column and the right-pane correctly. We also
		// recompute the right-pane body geometry and resize every
		// active VT emulator to match — claude draws for whatever
		// grid tmux tells it about, and keeping the emulator sized
		// to the visible area is what keeps wrapping / cursor
		// positioning honest. We do NOT emit a cmd: Bubble Tea's
		// default behavior is to re-render on the next tick, which
		// is exactly what we want.
		m.width = msg.Width
		m.height = msg.Height
		m.syncPaneViewport()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case sessionsMsg:
		next := m.handleSessions(msg)
		return next, tea.Batch(next.scheduleNextPoll(), next.resolveHighlightedPaneIfNeeded())
	case pollTickMsg:
		// A tick's only job is to kick off the next fetch. The fetch
		// itself, once complete, will schedule the following tick.
		// This keeps the cadence strictly serial — no overlap between
		// a slow fetch and the next ticker firing.
		return m, m.fetchSessions()
	case paneDataMsg:
		return m.handlePaneData(msg), m.waitForPaneEvent()
	case paneStreamClosedMsg:
		// The upstream stream terminated (tmuxctl.Client exited, or
		// the CLI layer closed the channel on TUI teardown). We do
		// NOT re-arm — a closed channel would spin a hot loop — and
		// we flip the flag so the right pane can explain what the
		// user is seeing.
		m.paneStreamClosed = true
		m.paneEvents = nil
		return m, nil
	case paneResolvedMsg:
		return m.handlePaneResolved(msg), nil
	}
	return m, nil
}

// handleSessions folds the freshest snapshot into the Model, clamping
// the highlight so j/k navigation stays on a valid row across
// insertions (new session created in another terminal) and deletions
// (a session closed while we were polling). Empty lister results and
// nil returns are normalized to a nil slice + highlight = -1, which
// makes the "no sessions" branch in View a single len-check.
//
// We also prune paneByWindow / paneErrByWindow entries for windows
// that are no longer in the snapshot. The corresponding VT
// emulators in paneTerminals are retained: a window closing does
// not necessarily mean its pane is gone from tmux's point of view
// within this tick, and the next stream event would simply re-
// populate them.
func (m Model) handleSessions(msg sessionsMsg) Model {
	m.ready = true
	m.listErr = msg.err
	m.sessions = msg.sessions
	// If the caller asked us to snap to a specific window (e.g. the
	// launch path just spawned a new claude window and wants the TUI
	// to open on it), try that first. On success we clear the hint
	// so subsequent polls don't keep overriding the user's
	// navigation. A hint that doesn't match any row in this
	// snapshot is retained; the next fetch may carry it.
	if m.initialHighlight != "" && len(m.sessions) > 0 {
		for i, s := range m.sessions {
			if s.WindowID == m.initialHighlight {
				m.highlight = i
				m.initialHighlight = ""
				break
			}
		}
	}
	switch {
	case len(m.sessions) == 0:
		m.highlight = -1
	case m.highlight < 0:
		m.highlight = 0
	case m.highlight >= len(m.sessions):
		m.highlight = len(m.sessions) - 1
	}
	// Prune per-window caches. We build a small set of still-live
	// window IDs rather than iterating sessions inside the loop,
	// because sessions can grow into the low hundreds once users
	// run many concurrent workspaces.
	alive := make(map[string]struct{}, len(m.sessions))
	for _, s := range m.sessions {
		alive[s.WindowID] = struct{}{}
	}
	for wid := range m.paneByWindow {
		if _, ok := alive[wid]; !ok {
			delete(m.paneByWindow, wid)
		}
	}
	for wid := range m.paneErrByWindow {
		if _, ok := alive[wid]; !ok {
			delete(m.paneErrByWindow, wid)
		}
	}
	return m
}

// handlePaneData feeds a chunk of bytes into the VT emulator for
// its pane. Unknown pane IDs are accepted: the stream carries data
// for every pane on the sm4c socket, not just the one we are
// previewing, so we happily feed them and let the render path
// decide what to show. Emulators are minted lazily at the current
// paneViewW / paneViewH so a pane that first appears after a
// terminal resize is not stuck at the default 80x24.
func (m Model) handlePaneData(msg paneDataMsg) Model {
	if msg.paneID == "" {
		return m
	}
	term, ok := m.paneTerminals[msg.paneID]
	if !ok {
		term = newPaneTerminal(m.paneViewW, m.paneViewH)
		m.paneTerminals[msg.paneID] = term
	}
	term.write(msg.data)
	return m
}

// syncPaneViewport recomputes the right-pane body dimensions from
// the current window size and resizes every existing emulator to
// match. Called from the WindowSizeMsg handler. When the window is
// too narrow for a split layout or we have not yet seen a size, we
// leave the defaults in place; the fallback dims are still a sane
// 80x24 so rendering works under tests that never emit a size.
func (m *Model) syncPaneViewport() {
	w, h := m.rightPaneBodyDims()
	if w < 1 || h < 1 {
		return
	}
	if w == m.paneViewW && h == m.paneViewH {
		return
	}
	m.paneViewW = w
	m.paneViewH = h
	for _, t := range m.paneTerminals {
		t.resize(w, h)
	}
}

// rightPaneBodyDims returns the interior dimensions of the right
// pane body — width and height of the region where the VT emulator
// should draw. Derived from the current WindowSizeMsg: a narrow
// terminal (or one we have not sized yet) returns zeros and the
// caller keeps the defaults. The numbers account for the one-col
// padding lipgloss applies inside rightPaneStyle and for the header
// + blank separator line renderRightPane prepends.
func (m Model) rightPaneBodyDims() (int, int) {
	if m.width < minSplitWidth || m.height < 1 {
		return 0, 0
	}
	sidebarW := m.sidebarWidth()
	rightW := m.width - sidebarW - 1
	if rightW < 1 {
		return 0, 0
	}
	// rightPaneStyle has Padding(0, 1) — 1 col on each side.
	bodyW := rightW - 2
	if bodyW < 1 {
		return 0, 0
	}
	// Header + blank separator = 2 lines consumed before body.
	bodyH := m.height - 2
	if bodyH < 1 {
		return 0, 0
	}
	return bodyW, bodyH
}

// handlePaneResolved records the (windowID, paneID) mapping the
// resolver returned. On error we stash the error so the right pane
// can surface it, and we intentionally do NOT clear any previously
// resolved pane ID: a transient resolver failure should not blank
// out an already-working preview.
func (m Model) handlePaneResolved(msg paneResolvedMsg) Model {
	if msg.windowID == "" {
		return m
	}
	if msg.err != nil {
		m.paneErrByWindow[msg.windowID] = msg.err
		return m
	}
	delete(m.paneErrByWindow, msg.windowID)
	if msg.paneID != "" {
		m.paneByWindow[msg.windowID] = msg.paneID
	}
	return m
}

// resolveHighlightedPaneIfNeeded triggers an ActivePaneResolver call
// when the highlighted session changed and we do not yet have a
// cached pane ID for it. Called from both the sessions-update path
// and the key-handling path. Returns nil when no resolution is
// necessary or possible (no resolver, no highlight, already known).
func (m *Model) resolveHighlightedPaneIfNeeded() tea.Cmd {
	if m.paneResolver == nil {
		return nil
	}
	if m.highlight < 0 || m.highlight >= len(m.sessions) {
		return nil
	}
	wid := m.sessions[m.highlight].WindowID
	if wid == "" || wid == m.resolvedWindowID {
		return nil
	}
	if _, known := m.paneByWindow[wid]; known {
		m.resolvedWindowID = wid
		return nil
	}
	m.resolvedWindowID = wid
	return m.resolveActivePane(wid)
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
		// Signal "spawn a new session" and exit the Bubble Tea
		// runtime so the CLI layer can do the tmux round-trip
		// without holding the raw-mode terminal. The CLI then re-
		// enters the TUI with the new window ID as initial
		// highlight — from the user's perspective the sidebar
		// briefly blinks and redraws with the new row pre-selected.
		//
		// TODO (M3e): replace this one-shot exit with an in-TUI
		// compose sub-view (cwd picker + optional session name +
		// args). Today pressing `n` spawns a bare claude in the
		// process's current working directory, which matches the
		// previous M2c behavior and lets us ship the re-entry
		// loop before the compose UI is built.
		m.action = ActionNewSession
		m.quitting = true
		return m, tea.Quit

	case "enter":
		// Reserved for M3c input routing (focus the right pane so
		// keystrokes flow into claude). Today it is a deliberate
		// no-op — there is no exec-into-tmux shortcut in sm4c by
		// design: the whole TUI's premise is that you never leave
		// it to reach a session. Consuming the key here means
		// Bubble Tea's default Enter handling (which just submits
		// the current input line) never fires either, keeping the
		// sidebar completely quiet while you navigate.
		return m, nil

	case "j", "down":
		// Move highlight down, no wrap. A no-wrap bottom is the
		// same convention tmux's choose-tree and vim's :ls use —
		// wrapping makes skimming a list feel disorienting.
		if m.highlight < len(m.sessions)-1 {
			m.highlight++
		}
		return m, m.resolveHighlightedPaneIfNeeded()

	case "k", "up":
		if m.highlight > 0 {
			m.highlight--
		}
		return m, m.resolveHighlightedPaneIfNeeded()

	case "ctrl+b":
		// Placeholder for M3c's focus toggle (VSCode-style: move
		// focus between sidebar and the right pane so keystrokes
		// flow into claude). The binding is reserved now so the
		// muscle memory carries over once input routing lands.
		// Until then this is a deliberate no-op; when M3c enables
		// it, drop the "(coming in M3c)" suffix from bindings[].
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
// M3b.2 swaps the current raw-bytes right pane for a VT-emulated
// screen grid; M3c adds input routing so focus can move from sidebar
// to right pane and keystrokes flow into claude. Layout does not
// change across those milestones — only what the right column renders
// and what it does with key events.
func (m Model) View() string {
	if m.quitting {
		// Bubble Tea keeps View on screen briefly after tea.Quit
		// schedules; returning an empty string avoids a flicker of
		// stale content before control returns to cmd/sm4c/cli
		// (which either spawns a new session via ActionNewSession
		// and re-enters the TUI, or returns to the shell on
		// ActionNone).
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
	{"n", "new session"},
	{"ctrl+b", "focus right pane (coming in M3c)"},
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
		Render(m.renderRightPane())

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

// renderRightPane paints the right-hand column: a live VT-emulated
// preview of the highlighted session's tmux pane, with a fallback
// for every state that has no preview to show (no lister, no
// sessions, stream down, pane not yet resolved).
//
// Starting in M3b.2, the body is the output of a charmbracelet/x/vt
// emulator fed with the pane's raw bytes, so ANSI styling, cursor
// motion, and line wrapping all survive the handoff — the preview
// looks identical to a native tmux attach within the emulator's
// grid. The VT parser absorbs every control sequence before we
// render, so a stray OSC / APC / DCS escape from the pane cannot
// corrupt the outer terminal.
//
// The emulator is sized from paneViewW / paneViewH (kept in sync
// with the right-pane body geometry on every tea.WindowSizeMsg),
// so what comes out of Render already fits the column.
func (m Model) renderRightPane() string {
	header := m.renderRightPaneHeader()
	body := m.renderRightPaneBody()
	return header + "\n\n" + body
}

// renderRightPaneHeader is the first line of the right pane. It
// names the currently highlighted session (or explains why the
// preview is inactive) and carries a compact status like "preview
// ready" / "resolving…" so the user always knows what state they
// are in.
func (m Model) renderRightPaneHeader() string {
	if len(m.sessions) == 0 {
		return hintStyle.Render("no active session")
	}
	if m.highlight < 0 || m.highlight >= len(m.sessions) {
		return hintStyle.Render("no active session")
	}
	s := m.sessions[m.highlight]
	name := s.Name
	if name == "" {
		name = "(unnamed)"
	}
	line := titleStyle.Render(name) + hintStyle.Render("  "+s.WindowID)
	return line
}

// renderRightPaneBody is the body beneath the header. It returns
// either the VT-emulated screen of the selected pane or a
// state-appropriate hint line. Keeping every branch in one place
// makes it easy to reason about what the user will see in each
// configuration.
func (m Model) renderRightPaneBody() string {
	if len(m.sessions) == 0 || m.highlight < 0 || m.highlight >= len(m.sessions) {
		return hintStyle.Render("press n to start a new session")
	}
	s := m.sessions[m.highlight]
	wid := s.WindowID

	if m.paneStreamClosed {
		return hintStyle.Render("preview disconnected — tmux control channel closed")
	}
	if m.paneResolver == nil && m.paneEvents == nil {
		return hintStyle.Render("pane preview unavailable") + "\n" +
			hintStyle.Render("(input routing lands in M3c)")
	}
	if err, ok := m.paneErrByWindow[wid]; ok && err != nil {
		return hintStyle.Render("pane lookup failed: " + err.Error())
	}
	paneID, ok := m.paneByWindow[wid]
	if !ok {
		return hintStyle.Render("resolving pane…")
	}
	term, ok := m.paneTerminals[paneID]
	if !ok || term == nil || !term.written {
		return hintStyle.Render("waiting for output  " + paneID)
	}
	return term.render()
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
// inspect Action(). On any runtime error (TTY setup, input stream
// closed unexpectedly) the error is returned and the intents are
// ignored.
//
// initialHighlight is an optional tmux window ID: when non-empty,
// the first sessionsMsg that contains a matching row snaps the
// highlight to that row instead of the default first-row behavior.
// This is how `sm4c [claude-args]` opens the TUI already focused on
// the freshly-spawned session without any exec-into-tmux shortcut.
func Run(
	in interface {
		Read(p []byte) (n int, err error)
	},
	out interface {
		Write(p []byte) (n int, err error)
	},
	lister SessionLister,
	pollInterval time.Duration,
	paneStream PaneEventStream,
	paneResolver ActivePaneResolver,
	initialHighlight string,
) (Model, error) {
	p := tea.NewProgram(
		NewModel(lister, pollInterval, paneStream, paneResolver, initialHighlight),
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

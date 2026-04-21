package tui

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Focus describes which column of the TUI owns keystrokes. M3c
// introduced the two-state model: by default the sidebar is
// focused and every keystroke is interpreted as a sm4c navigation
// shortcut; once the user presses ctrl+b (or Enter on a highlighted
// row) the pane takes focus and keystrokes are forwarded to the
// tmux pane of the highlighted session via send-keys. Ctrl+b
// toggles back.
//
// Kept as an int (not a string enum) so zero is a valid value
// (FocusSidebar), which is what every unit-test fixture relies
// on for its zero-Deps Model to boot into the expected state.
type Focus int

const (
	// FocusSidebar routes every keystroke through sm4c's own
	// binding table (j/k navigation, n for new session, q/ctrl+c
	// to quit, etc.). This is the default on bare `sm4c` so the
	// user can browse sessions before committing to one.
	FocusSidebar Focus = iota

	// FocusPane forwards every keystroke (except ctrl+b, which
	// toggles focus back) to the highlighted session's active
	// tmux pane via the KeySender seam. In this mode ctrl+c is
	// forwarded to claude (interrupting whatever it is doing)
	// rather than quitting sm4c, matching every other terminal
	// multiplexer's convention: to quit the outer shell, first
	// leave the inner program.
	FocusPane
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

// Deps bundles every external seam the Model relies on. Keeping
// them in a single struct (rather than positional arguments to
// NewModel / Run) is the scalable shape as milestones accrete
// dependencies: M3b.1 added PaneStream + PaneResolver, M3b.3 adds
// PaneCapturer + WindowResizer, and M3c will add input seams too.
// Every field is optional — the Model degrades gracefully when any
// combination is nil, which is the contract the unit-test fixtures
// rely on to stay minimal.
//
//   - Lister            SessionLister that drives the sidebar.
//     Nil keeps the empty-state view and skips
//     all polling.
//   - PollInterval      cadence for Lister. Zero / negative means
//     "fetch once at Init and never again".
//     Ignored when Lister is nil.
//   - PaneStream        factory returning a PaneEvent channel. Nil
//     disables live %output; the right pane shows
//     a static "preview unavailable" line.
//   - PaneResolver      async mapper windowID -> paneID. Nil
//     disables pane resolution; without a pane
//     ID, the stream's events are never tied to
//     a highlighted session.
//   - PaneCapturer      async mapper paneID -> initial screen.
//     Nil disables M3b.3 backfill; the emulator
//     still boots on the first live chunk.
//   - WindowResizer     async tmux `resize-window` seam. Nil
//     disables M3b.3 viewport sync; wrapping will
//     drift after a terminal resize until the
//     user switches sessions.
//   - InitialHighlight  tmux window ID to snap to on the first
//     sessionsMsg containing it. Empty defaults
//     to row 0 behavior.
//   - KeySender         async tmux `send-keys -H` seam. Nil
//     disables input routing (pane focus becomes
//     a read-only spectator view); the TUI keeps
//     the focus toggle so the UX is testable
//     without a live tmux.
//   - InitialFocus      focus state on entry. Zero value is
//     FocusSidebar so existing tests remain
//     unchanged; the launch path (sm4c [args])
//     and the "n" re-entry loop set FocusPane
//     so the newly-spawned session is
//     immediately typable.
type Deps struct {
	Lister           SessionLister
	PollInterval     time.Duration
	PaneStream       PaneEventStream
	PaneResolver     ActivePaneResolver
	PaneCapturer     PaneCapturer
	WindowResizer    WindowResizer
	WindowCloser     WindowCloser
	KeySender        KeySender
	InitialHighlight string
	InitialFocus     Focus

	// SilenceThreshold is retained for backward-compatible config
	// unmarshalling but is no longer used at runtime; status is now
	// driven entirely by Claude Code lifecycle hooks. Callers that
	// still pass it will see no effect.
	SilenceThreshold time.Duration

	// HookFifoPath is the path to the named pipe Claude Code hook
	// scripts write event lines to. When non-empty the TUI creates
	// the FIFO (if absent) and reads hook events to drive session
	// status indicators. Empty means hooks are disabled and all
	// sessions appear as StatusQuiet/StatusIdle. The path is
	// propagated to claude sessions via the SM4C_HOOK_FIFO
	// environment variable on the tmux socket.
	HookFifoPath string

	// SidebarHighlightBG and SidebarHighlightFG are decimal color
	// index strings "0"–"255" (ANSI 0–15 or xterm 256 palette).
	// Empty strings mean use style.go defaults (gray + bright
	// white). Plumbed from config.toml by the CLI; lipgloss maps
	// indices using the renderer set in Run.
	SidebarHighlightBG string
	SidebarHighlightFG string
}

// Model is the Bubble Tea model backing the sidebar view. Its
// fields split into two groups: the UX state (help, quitting,
// action) which decides what to render and what intent to report on
// exit, and the session-list state (lister, pollInterval, sessions,
// highlight, ready, listErr, initialHighlight) which backs the
// sidebar once managed windows show up.
//
//   - help              toggled by `?`; shows the full keybind list.
//   - quitting          set when the user pressed a quit key; causes
//     Update to return tea.Quit on the NEXT step.
//     Separating "set intent" from "tell bubbletea
//     to quit" in two Update returns is what makes
//     the unit tests able to observe ActionNone
//     even without running the runtime.
//   - action            the intent the caller should realize once
//     tea.Quit flushes the runtime. Exposed via
//     Model.Action().
//   - lister            injected SessionLister. Nil means "no live
//     data": the Model renders the empty state,
//     Init returns no startup cmd, and the poll
//     loop is inert. This is how unit tests that
//     care only about key handling keep their
//     fixtures minimal.
//   - pollInterval      how often to re-fetch via lister. Honored
//     only when lister is non-nil and the value
//     is positive.
//   - sessions          last snapshot returned by lister. Treated as
//     authoritative for rendering; Model does not
//     mutate it between fetches.
//   - highlight         zero-based index into sessions that the user
//     is currently cursoring over. Clamped by the
//     sessionsMsg handler so it always references
//     a valid row (or is -1 when sessions is empty).
//   - ready             true once the first sessionsMsg has been
//     processed. Before that, the view short-
//     circuits to the empty state so a slow first
//     fetch doesn't paint a stale "N sessions"
//     line.
//   - listErr           last fetch error, if any. Surfaced as a
//     faint single-line notice in the sidebar.
//     Does not block rendering of stale sessions.
//   - initialHighlight  optional tmux window ID the Model should
//     snap the highlight to as soon as the first
//     sessionsMsg contains a matching row. Used
//     by the launch path so `sm4c [claude-args]`
//     opens the TUI with the freshly-spawned
//     session pre-selected. Cleared once applied
//     so later navigation isn't overridden on the
//     next poll.
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

	// sidebarHighlightBG / FG mirror Deps (or defaults); used when
	// building the selection-band lipgloss style in renderSessionCard.
	sidebarHighlightBG string
	sidebarHighlightFG string

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

	// paneCapturer is the M3b.3 backfill seam. When non-nil, the
	// first paneResolvedMsg for a pane kicks off a capture-pane
	// round-trip; the result is fed into the emulator before any
	// live bytes so switching to a session shows its current
	// screen immediately. Nil disables backfill (tests and
	// degraded environments).
	paneCapturer PaneCapturer

	// windowResizer is the M3b.3 viewport-sync seam. When non-nil,
	// terminal resizes and highlight changes issue a resize-window
	// so tmux's pane grid matches the TUI's right-pane viewport.
	// Nil disables the sync (wrapping may drift, but the TUI
	// still renders correctly).
	windowResizer WindowResizer

	// windowCloser is the close-session seam. When non-nil, the
	// `x` binding in sidebar focus asks for confirmation and, on
	// `y`, calls through to terminate the highlighted session's
	// tmux window. Nil disables the binding (tests and degraded
	// environments where we cannot mutate the socket). See the
	// pendingCloseWindow field for the confirmation state.
	windowCloser WindowCloser

	// pendingCloseWindow is the tmux window ID the user has armed
	// for close via `x` but not yet confirmed. While non-empty:
	//   - the sidebar renders a "close test-sesh? y/n" hint
	//     instead of the normal key bar, so the intent is visible;
	//   - the next sidebar-focus keystroke disambiguates:
	//       * `y` / `Y`    -> dispatch closeManagedWindow
	//       * anything else -> cancel (clear the field; no close)
	// Cleared when the corresponding windowClosedMsg comes back,
	// when the user cancels, or when a sessionsMsg reveals the
	// target window is already gone (stale confirmation).
	pendingCloseWindow string

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

	// paneCapturing records panes that have a CapturePane round-
	// trip in flight. While set for a pane, incoming paneDataMsg
	// bytes are appended to panePending[paneID] instead of
	// flushed into the emulator — this preserves the "capture
	// first, then live" ordering the VT parser needs.
	paneCapturing map[string]bool

	// paneCaptured is a per-pane "we have attempted a capture"
	// marker. Once set, a pane never re-enters the capturing
	// state, even if the attempt returned an error. One failed
	// backfill is enough; live %output paints the eventual state.
	paneCaptured map[string]bool

	// panePending buffers live bytes that arrived while a pane's
	// capture was in flight. It is bounded by paneBackfillBuffer
	// per pane (a hostile claude cannot exhaust memory during a
	// slow tmux round-trip); overflow is dropped tail-first, same
	// as the bridge channel's backpressure posture.
	panePending map[string][]byte

	// sizedFor records the last (width, height) each window was
	// resized to via WindowResizer. We re-emit a resize only when
	// the tuple changes, so rapid j/k navigation or a debounced
	// stream of tea.WindowSizeMsg events does not spam tmux with
	// duplicate resize-window commands.
	sizedFor map[string][2]int

	// forceResizePending is a one-shot flag that tells the next
	// resizeHighlightedWindow call to fire a wiggle resize
	// (see forceResizeManagedWindow) instead of the usual
	// debounced single resize. It is set by handleWindowClosed
	// because tmux no-ops a resize-window whose target dims
	// match the pane's current dims — and after a close the
	// surviving pane's current dims usually DO match what the
	// TUI was showing pre-close, so the post-close resize is a
	// no-op and no SIGWINCH reaches claude. No SIGWINCH means
	// no redraw, which means the cursor our emulator inherited
	// from the (now-stale) capture-pane backfill points at
	// whatever row ended the capture — almost always the bottom.
	// Subsequent echo bytes from claude therefore land at the
	// bottom of the preview instead of inside claude's input
	// box. The wiggle guarantees SIGWINCH, guarantees a full
	// claude redraw, and guarantees cursor re-positioning
	// before the next keystroke. Cleared to false the moment a
	// wiggle is emitted so a subsequent j/k navigation does
	// NOT double-pay the wiggle cost.
	forceResizePending bool

	// focus is the M3c focus state. FocusSidebar (zero value)
	// means keystrokes hit sm4c's own binding table; FocusPane
	// means they are forwarded to the highlighted session's
	// active pane via keySender. See the Focus docstring for
	// the full semantics.
	focus Focus

	// sidebarHidden collapses the sidebar column so the right
	// pane takes the entire viewport — the "zoom" mode from
	// tmux-land. It is always paired with FocusPane while hidden,
	// because the only way out is ctrl+b (which re-shows the
	// sidebar and pulls focus back). A single flag captures the
	// whole UX: renderSidebarView reads it to skip drawing the
	// sidebar column, and rightPaneBodyDims reads it to stretch
	// the emulator grid across the full width. Toggling the flag
	// therefore implies a resize on the highlighted window so
	// tmux (and claude) observe the new grid immediately.
	//
	// We intentionally do NOT persist sidebarHidden across
	// re-launches of sm4c: each TUI session starts with the
	// sidebar visible so a first-time observer is never handed
	// a blank pane with no obvious way back to the session list.
	sidebarHidden bool

	// pendingFocus records a FocusPane request that could not be
	// honored yet because no session was available at startup
	// (e.g. `sm4c [args]` dropped us into the TUI before the
	// first sessionsMsg arrived). The next sessionsMsg that
	// produces a valid highlight consumes the request.
	pendingFocus bool

	// keySender is the M3c input-routing seam. When non-nil and
	// focus == FocusPane, each tea.KeyMsg (except ctrl+b) is
	// translated by keyMsgToBytes and forwarded via this sender
	// to the active pane of the highlighted session. Nil makes
	// pane focus a spectator mode (the toggle still works so the
	// UX is testable without a live tmux server).
	keySender KeySender

	// skipCaptureWindow is the tmux window ID for which the first
	// pane resolution should skip the capture-pane backfill round-
	// trip. It is set from Deps.InitialHighlight and consumed
	// (cleared) the first time handlePaneResolved sees a matching
	// windowID.
	//
	// Why this exists: the `n`/spawn re-entry loop in cmd/sm4c/cli
	// re-opens the TUI pointed at a freshly-created claude window.
	// Claude is still booting at that moment — its initial draw is
	// not yet complete, and tmux's grid holds a transient mix of
	// "default 80x24 content" and "post-SIGWINCH half-redraw". A
	// capture-pane call in that window lands a half-baked snapshot
	// into the VT emulator; claude's subsequent live %output bytes
	// paint over some cells but not others (most TUI frameworks
	// emit cursor-positioned writes, not CSI 2J clears, on a
	// steady-state redraw), so the captured ghosts linger until
	// the user nudges the terminal (which triggers a clean full
	// redraw).
	//
	// Existing sessions the user navigates to mid-TUI-session are
	// unaffected: they are not marked here, so capture runs as a
	// normal backfill. Only the spawn-entry window opts out, and
	// only for its very first pane resolution. The live stream
	// fills the emulator from a clean slate within ~1 claude
	// frame, which is indistinguishable from the captured path
	// for a settled session but correct for a still-booting one.
	skipCaptureWindow string

	// paneStatuses holds the hook-driven status record for each tmux
	// window. Keyed by WINDOW ID (e.g. "@42") so the derived glyph
	// survives pane-ID churn during close-window storms. Populated
	// by applyHookEvent when hook scripts write events to the FIFO.
	paneStatuses map[string]paneStatus

	// paneToWindow is the reverse of paneByWindow: given a pane
	// ID (the key %output events and TMUX_PANE carry), find the
	// owning window ID (the key paneStatuses uses). Maintained
	// alongside paneByWindow in handlePaneResolved.
	paneToWindow map[string]string

	// hookEvents is the channel delivering hookMsg values from the
	// FIFO listener started in NewModel. Nil when no HookFifoPath
	// was configured or the listener failed to start.
	hookEvents <-chan hookMsg

	// statusFrame is the animation counter consumed by
	// statusGlyph when a pane is Working. Advanced on each
	// statusFrameTickMsg; wraps implicitly because statusGlyph
	// takes it modulo len(spinnerFrames).
	statusFrame int

	// statusTickArmed records that a statusFrameTickMsg is
	// currently in flight. We use it to ensure we never have
	// more than one ticker outstanding at a time — otherwise a
	// Quiet→Working transition that races a still-in-flight
	// tick would double the animation cadence (and double it
	// again on the next race, etc.). Set when we schedule a
	// tick; cleared when the tick arrives.
	statusTickArmed bool

}

// paneBackfillBuffer bounds panePending per pane. 64 KiB is
// comfortably larger than any realistic burst of %output during
// a tmux capture-pane round-trip (sub-100 ms on a healthy socket
// means tens of KB at worst), and it keeps a pathological case —
// capturer wedged, %output flooding — from growing memory without
// bound. Beyond this point we drop pending tail bytes; the VT
// emulator's eventual state will still reflect the most recent
// screen on the next live chunk.
const paneBackfillBuffer = 64 * 1024

// NewModel constructs a fresh Model from the given Deps. Every
// field of Deps is optional; see the Deps doc for the semantics
// of each nil case. Production callers (cmd/sm4c/cli/tui.go) fill
// Lister from tmuxctl.OneShot.ListWindows, PaneStream from
// tmuxctl.Client.Events(), PaneResolver from
// tmuxctl.OneShot.ActivePane, PaneCapturer from
// tmuxctl.OneShot.CapturePane, and WindowResizer from
// tmuxctl.OneShot.ResizeWindow. Unit tests pass a zero Deps{} or
// a sparsely-filled one; whatever seams are nil produce a no-op
// equivalent so the substring assertions keep working.
func NewModel(deps Deps) Model {
	pollInterval := deps.PollInterval
	if pollInterval < 0 {
		pollInterval = 0
	}
	hlBG := deps.SidebarHighlightBG
	if hlBG == "" {
		hlBG = defaultSidebarHighlightBG
	}
	hlFG := deps.SidebarHighlightFG
	if hlFG == "" {
		hlFG = defaultSidebarHighlightFG
	}
	m := Model{
		lister:            deps.Lister,
		pollInterval:      pollInterval,
		highlight:         -1,
		initialHighlight:  deps.InitialHighlight,
		skipCaptureWindow: deps.InitialHighlight,
		paneResolver:      deps.PaneResolver,
		paneCapturer:      deps.PaneCapturer,
		windowResizer:     deps.WindowResizer,
		windowCloser:      deps.WindowCloser,
		keySender:         deps.KeySender,
		paneByWindow:      make(map[string]string),
		paneErrByWindow:   make(map[string]error),
		paneTerminals:     make(map[string]*paneTerminal),
		paneCapturing:     make(map[string]bool),
		paneCaptured:      make(map[string]bool),
		panePending:       make(map[string][]byte),
		sizedFor:          make(map[string][2]int),
		paneStatuses:      make(map[string]paneStatus),
		paneToWindow:      make(map[string]string),
		paneViewW:         defaultPaneWidth,
		paneViewH:         defaultPaneHeight,
		focus:              FocusSidebar,
		pendingFocus:       deps.InitialFocus == FocusPane,
		sidebarHighlightBG: hlBG,
		sidebarHighlightFG: hlFG,
	}
	if deps.PaneStream != nil {
		m.paneEvents = deps.PaneStream()
	}
	if deps.HookFifoPath != "" {
		ch, err := startHookListener(deps.HookFifoPath)
		if err != nil {
			debugf("hook listener failed: %v", err)
		} else {
			m.hookEvents = ch
		}
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
	cmds := make([]tea.Cmd, 0, 3)
	if c := m.fetchSessions(); c != nil {
		cmds = append(cmds, c)
	}
	if c := m.waitForPaneEvent(); c != nil {
		cmds = append(cmds, c)
	}
	if c := m.waitForHookEvent(); c != nil {
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil
	}
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// Update is the pure state-transition function. The only messages
// the empty-state view reacts to today are key presses and window
// resize; the resize is accepted silently because lipgloss rendering
// already adapts. When M3 introduces live session state, a
// tickMsg / refreshMsg / windowStatusMsg family will join this
// switch — each should remain side-effect free and return its work
// as a tea.Cmd.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	debugf("Update msg=%T", msg)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Stash the terminal dimensions so the view can size the
		// sidebar column and the right-pane correctly. We also
		// recompute the right-pane body geometry and resize every
		// active VT emulator to match — claude draws for whatever
		// grid tmux tells it about, and keeping the emulator sized
		// to the visible area is what keeps wrapping / cursor
		// positioning honest.
		//
		// On geometry change we ALSO tell tmux to resize the
		// highlighted session's window so the upstream pane grid
		// matches our emulator. That round-trip is async via a
		// tea.Cmd so the resize itself never blocks the render.
		prevW, prevH := m.paneViewW, m.paneViewH
		m.width = msg.Width
		m.height = msg.Height
		m.syncPaneViewport()
		if m.paneViewW != prevW || m.paneViewH != prevH {
			return m, m.resizeHighlightedWindow()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case sessionsMsg:
		next := m.handleSessions(msg)
		return next, tea.Batch(
			next.scheduleNextPoll(),
			next.resolveHighlightedPaneIfNeeded(),
			next.resizeHighlightedWindow(),
		)
	case pollTickMsg:
		// A tick's only job is to kick off the next fetch. The fetch
		// itself, once complete, will schedule the following tick.
		// This keeps the cadence strictly serial — no overlap between
		// a slow fetch and the next ticker firing.
		return m, m.fetchSessions()
	case paneDataMsg:
		// Feed bytes into the VT emulator and re-arm the reader.
		// Pane data no longer drives status — that is handled by
		// hookMsg events from Claude Code lifecycle hooks.
		next := m.handlePaneData(msg)
		return next, next.waitForPaneEvent()
	case statusFrameTickMsg:
		// Advance the animation frame and, if any pane is
		// still Working, schedule the next tick. The frame
		// counter is modulo-applied inside statusGlyph, so
		// plain integer increment is fine; wrap-around at
		// math.MaxInt is measured in years of uninterrupted
		// animation. statusTickArmed is flipped to false FIRST
		// so scheduleStatusTick's own armed-guard lets the
		// next tick through.
		m.statusTickArmed = false
		m.statusFrame++
		if tick := m.scheduleStatusTick(); tick != nil {
			m.statusTickArmed = true
			return m, tick
		}
		return m, nil
	case hookMsg:
		// A Claude Code lifecycle hook fired. Transition the pane's
		// status and re-arm the hook reader. Arm the animation ticker
		// when the event is Working (spinner needed).
		//
		// On Done we explicitly clear statusTickArmed before calling
		// scheduleStatusTick. Without this there is a race: the ticker
		// that was running for the previous Working state leaves
		// statusTickArmed = true; if UserPromptSubmit arrives before the
		// next statusFrameTickMsg clears it, scheduleStatusTick returns
		// nil ("already armed") and the spinner never starts for the new
		// round.
		m.applyHookEvent(msg)
		if msg.event == hookEventDone {
			m.statusTickArmed = false
		}
		cmds := []tea.Cmd{m.waitForHookEvent()}
		if tick := m.scheduleStatusTick(); tick != nil {
			m.statusTickArmed = true
			cmds = append(cmds, tick)
		}
		return m, tea.Batch(cmds...)
	case hookStreamClosedMsg:
		m.hookEvents = nil
		return m, nil
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
		next, cmd := m.handlePaneResolved(msg)
		return next, cmd
	case paneCaptureMsg:
		return m.handlePaneCapture(msg), nil
	case windowResizedMsg:
		// The WindowResizer round-trip completed. Success or
		// failure, we have nothing to do here — the emulator is
		// already sized locally; if the tmux-side resize failed
		// the user will see drift and can recover by switching
		// sessions. We deliberately do not surface per-resize
		// errors in the sidebar; they are too chatty during a
		// flaky tmux state to be useful.
		_ = msg
		return m, nil
	case keysSentMsg:
		return m.handleKeysSent(msg), nil
	case windowClosedMsg:
		return m.handleWindowClosed(msg)
	}
	// Forward unrecognised byte-sequence messages to the focused pane when
	// in pane focus mode. bubbletea delivers enhanced terminal sequences it
	// cannot name — such as Shift+Enter (ESC[13;2u) from kitty keyboard
	// protocol — as its unexported unknownCSISequenceMsg type, which is a
	// named []byte. We use reflect to extract the bytes rather than
	// importing an unexported type; the Kind/Elem check is tight enough
	// that no other bubbletea message type (all structs or exported scalars)
	// passes through. macOS Terminal.app users will not see this path since
	// that terminal sends plain 0x0d for Shift+Enter; users on
	// iTerm2 / WezTerm / kitty / Alacritty get Shift+Enter routed correctly.
	if m.focus == FocusPane {
		val := reflect.ValueOf(msg)
		if val.IsValid() && val.Kind() == reflect.Slice && val.Type().Elem().Kind() == reflect.Uint8 {
			raw := val.Bytes()
			if len(raw) > 0 && m.highlight >= 0 && m.highlight < len(m.sessions) {
				paneID, ok := m.paneByWindow[m.sessions[m.highlight].WindowID]
				if ok && paneID != "" {
					m.notePaneKeystroke(paneID)
					cmds := []tea.Cmd{m.sendKeysToPane(paneID, raw)}
					if tick := m.scheduleStatusTick(); tick != nil {
						m.statusTickArmed = true
						cmds = append(cmds, tick)
					}
					return m, tea.Batch(cmds...)
				}
			}
		}
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
	debugf("sessions count=%d err=%v", len(msg.sessions), msg.err)
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
	// Focus invariants: FocusPane only makes sense while a session
	// is highlighted. If polling dropped every session (the user
	// closed the last one externally), revert to the sidebar so
	// the user can press `n` or `q` without a confusing "typing
	// into nothing" state. If startup requested FocusPane via
	// InitialFocus, consume the request once a valid highlight
	// is available.
	if m.focus == FocusPane && (m.highlight < 0 || m.highlight >= len(m.sessions)) {
		m.focus = FocusSidebar
	}
	if m.pendingFocus && m.highlight >= 0 && m.highlight < len(m.sessions) {
		m.focus = FocusPane
		m.pendingFocus = false
	}
	// Prune per-window caches. We build a small set of still-live
	// window IDs rather than iterating sessions inside the loop,
	// because sessions can grow into the low hundreds once users
	// run many concurrent workspaces.
	alive := make(map[string]struct{}, len(m.sessions))
	for _, s := range m.sessions {
		alive[s.WindowID] = struct{}{}
	}
	// A stale pending close (target window vanished externally
	// between `x` and the next poll) is cleared so the y/n
	// prompt cannot land on a session that no longer exists.
	if m.pendingCloseWindow != "" {
		if _, ok := alive[m.pendingCloseWindow]; !ok {
			m.pendingCloseWindow = ""
		}
	}
	for wid, paneID := range m.paneByWindow {
		if _, ok := alive[wid]; !ok {
			delete(m.paneByWindow, wid)
			delete(m.paneToWindow, paneID)
		}
	}
	for wid := range m.paneErrByWindow {
		if _, ok := alive[wid]; !ok {
			delete(m.paneErrByWindow, wid)
		}
	}
	for wid := range m.sizedFor {
		if _, ok := alive[wid]; !ok {
			delete(m.sizedFor, wid)
		}
	}
	for wid := range m.paneStatuses {
		if _, ok := alive[wid]; !ok {
			delete(m.paneStatuses, wid)
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
//
// If a CapturePane round-trip is currently in flight for this
// pane (M3b.3 backfill), the chunk is buffered in panePending
// instead of flushed into the emulator — this preserves the
// "capture first, then live" ordering the VT parser depends on.
// The handlePaneCapture handler flushes the buffer once the
// capture arrives.
func (m Model) handlePaneData(msg paneDataMsg) Model {
	debugf("paneData pane=%s bytes=%d", msg.paneID, len(msg.data))
	if msg.paneID == "" {
		return m
	}
	if m.paneCapturing[msg.paneID] {
		m.appendPending(msg.paneID, msg.data)
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

// notePaneKeystroke is called when the user sends a keystroke into a pane.
// It clears Done/Waiting glyphs back to Idle — the "user acknowledged and
// is now responding" signal. The spinner (Working) is not set here; it
// arrives via the UserPromptSubmit hook once Claude Code processes the
// submission. statusTickArmed is reset on Done (in the hookMsg handler)
// so the next Working transition always arms the tick cleanly.
//
// everHadHook is preserved so the pane shows Idle (·) rather than
// snapping back to Quiet (·) when the state is cleared.
func (m Model) notePaneKeystroke(paneID string) {
	if paneID == "" {
		return
	}
	windowID := m.paneToWindow[paneID]
	if windowID == "" {
		return
	}
	ps := m.paneStatuses[windowID]
	// Don't clear Working: if UserPromptSubmit already fired, a trailing
	// keystroke echo must not clobber the spinner.
	if ps.hookState == hookEventWorking {
		return
	}
	ps.hookState = hookEventNone
	m.paneStatuses[windowID] = ps
}

// appendPending queues bytes in panePending[paneID] while capture
// is in flight. The buffer is bounded by paneBackfillBuffer per
// pane; bytes beyond that are dropped (tail-first, so we keep the
// earliest post-capture frame) rather than stalling the stream
// reader — the VT emulator's eventual state will be correct once
// the next live chunk lands and overwrites the row.
func (m *Model) appendPending(paneID string, data []byte) {
	if len(data) == 0 {
		return
	}
	cur := m.panePending[paneID]
	remaining := paneBackfillBuffer - len(cur)
	if remaining <= 0 {
		return
	}
	if len(data) > remaining {
		data = data[:remaining]
	}
	m.panePending[paneID] = append(cur, data...)
}

// handlePaneCapture flushes a completed CapturePane round-trip
// into the pane's emulator:
//
//  1. Clear the capturing flag and mark the pane captured (so a
//     second attempt is never issued, even on failure — one
//     backfill per pane lifetime).
//  2. If capture succeeded with non-empty data, create the
//     emulator (if missing) and write the captured bytes. This
//     is what paints claude's current screen into the preview.
//  3. Flush any bytes that arrived while the capture was in
//     flight, preserving the "capture first, then live" order.
//  4. Drop the pending buffer.
//
// A capture error is absorbed silently: the user sees "waiting
// for output" until a live chunk arrives, which is acceptable
// for a one-shot backfill. We do not surface an error hint
// because a capture failure on one pane should not blank out
// the preview for other panes.
func (m Model) handlePaneCapture(msg paneCaptureMsg) Model {
	debugf("paneCapture pane=%s bytes=%d err=%v", msg.paneID, len(msg.data), msg.err)
	paneID := msg.paneID
	if paneID == "" {
		return m
	}
	delete(m.paneCapturing, paneID)
	m.paneCaptured[paneID] = true

	if msg.err == nil && len(msg.data) > 0 {
		term, ok := m.paneTerminals[paneID]
		if !ok {
			term = newPaneTerminal(m.paneViewW, m.paneViewH)
			m.paneTerminals[paneID] = term
		}
		term.write(normalizeCaptureEOL(msg.data))
	}
	if pending, ok := m.panePending[paneID]; ok && len(pending) > 0 {
		term, ok2 := m.paneTerminals[paneID]
		if !ok2 {
			term = newPaneTerminal(m.paneViewW, m.paneViewH)
			m.paneTerminals[paneID] = term
		}
		term.write(pending)
	}
	delete(m.panePending, paneID)
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
//
// When the sidebar is zoomed away (sidebarHidden), the right pane
// takes the full terminal width instead of the one-third split.
// Both paths converge on rightPaneInteriorDims so the padding/
// header math stays in one place.
func (m Model) rightPaneBodyDims() (int, int) {
	if m.sidebarHidden {
		return rightPaneBodyDimsFullWidth(m.width, m.height)
	}
	return RightPaneBodyDims(m.width, m.height)
}

// handlePaneResolved records the (windowID, paneID) mapping the
// resolver returned. On error we stash the error so the right pane
// can surface it, and we intentionally do NOT clear any previously
// resolved pane ID: a transient resolver failure should not blank
// out an already-working preview.
//
// On success, if PaneCapturer is wired and this pane has not yet
// been captured (or a capture is not already in flight), we return
// a capture cmd so the right pane shows the current screen of the
// session the user just focused, rather than waiting for the next
// live chunk. This is the M3b.3 "switching to a session shows its
// current state immediately" user-facing promise.
func (m Model) handlePaneResolved(msg paneResolvedMsg) (Model, tea.Cmd) {
	debugf("paneResolved window=%s pane=%s err=%v", msg.windowID, msg.paneID, msg.err)
	if msg.windowID == "" {
		return m, nil
	}
	if msg.err != nil {
		m.paneErrByWindow[msg.windowID] = msg.err
		return m, nil
	}
	delete(m.paneErrByWindow, msg.windowID)
	if msg.paneID == "" {
		return m, nil
	}
	m.paneByWindow[msg.windowID] = msg.paneID
	m.paneToWindow[msg.paneID] = msg.windowID

	if m.paneCapturer == nil {
		return m, nil
	}
	if m.paneCaptured[msg.paneID] || m.paneCapturing[msg.paneID] {
		return m, nil
	}
	// Freshly-spawned-session opt-out: the CLI passed the new
	// window's ID through Deps.InitialHighlight. Suppress the
	// capture-pane for that window on its very first resolve so
	// the emulator stays empty until claude's live %output
	// paints it from scratch. See skipCaptureWindow's docstring
	// for the full rationale (tl;dr: captured bytes from a
	// still-booting claude ghost-layer under the live redraw).
	if m.skipCaptureWindow != "" && msg.windowID == m.skipCaptureWindow {
		m.skipCaptureWindow = ""
		m.paneCaptured[msg.paneID] = true
		return m, nil
	}
	m.paneCapturing[msg.paneID] = true
	return m, m.captureActivePane(msg.paneID)
}

// resizeHighlightedWindow returns a tea.Cmd that asks the
// WindowResizer to size the currently-highlighted window to the
// right-pane viewport dimensions. No-op when no resizer is wired,
// no session is highlighted, the viewport is too narrow to split,
// or the window has already been sized to these exact dims (the
// last case debounces rapid j/k or repeated WindowSizeMsg events).
//
// This is the single choke-point for "keep tmux's pane grid in
// sync with our viewport" — the WindowSizeMsg handler, the
// sessionsMsg handler, and handleKey all funnel through here,
// so there is exactly one place where the debounce policy lives.
func (m *Model) resizeHighlightedWindow() tea.Cmd {
	if m.windowResizer == nil {
		return nil
	}
	if m.highlight < 0 || m.highlight >= len(m.sessions) {
		return nil
	}
	wid := m.sessions[m.highlight].WindowID
	if wid == "" {
		return nil
	}
	w, h := m.paneViewW, m.paneViewH
	if w < 1 || h < 1 {
		return nil
	}
	// Force-redraw one-shot. handleWindowClosed sets this flag
	// because a plain same-size resize-window post-close is a
	// tmux no-op that fails to SIGWINCH claude; see the
	// forceResizePending docstring for the full rationale. We
	// consume the flag unconditionally (whether or not the
	// debounce would normally fire) so the wiggle reaches
	// tmux even when sizedFor already had the right dims
	// cached from an earlier resize.
	if m.forceResizePending {
		m.forceResizePending = false
		m.sizedFor[wid] = [2]int{w, h}
		return m.forceResizeManagedWindow(wid, w, h)
	}
	prev, seen := m.sizedFor[wid]
	if seen && prev[0] == w && prev[1] == h {
		return nil
	}
	m.sizedFor[wid] = [2]int{w, h}
	// First-ever resize for this window: wiggle instead of a
	// plain resize. Rationale (M3e polish round 3): if the CLI
	// pre-sized the tmux window to these exact dims before
	// launching the TUI, tmux will treat our matching-dim
	// resize-window as a no-op and skip the SIGWINCH claude
	// needs to re-flow its layout. The user-visible failure is
	// "typing lands at the bottom" — claude still thinks it's
	// running at tmux's default grid while sm4c has already
	// resized underneath it. A wiggle (W, H+1)→(W, H) is
	// guaranteed to change grid state at least once and
	// therefore guaranteed to SIGWINCH. Subsequent resizes for
	// the same window take the plain path below, so the wiggle
	// cost is paid once per session per window, not on every
	// flick through the sidebar.
	if !seen {
		return m.forceResizeManagedWindow(wid, w, h)
	}
	return m.resizeManagedWindow(wid, w, h)
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

// handleKey dispatches a key press based on the current focus.
// Sidebar focus runs sm4c's own binding table (navigation, new
// session, help, quit); pane focus forwards every keystroke to
// the highlighted session's active pane via the KeySender seam,
// except for ctrl+b which is the single sm4c-reserved shortcut
// in pane focus (toggles focus back to the sidebar). Factored
// out of Update so the unit tests can exercise it with a plain
// tea.KeyMsg and no message-switch scaffolding.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.focus == FocusPane {
		return m.handleKeyInPaneFocus(msg)
	}
	return m.handleKeyInSidebarFocus(msg)
}

// handleKeyInSidebarFocus implements the sm4c binding table: j/k
// navigation, n for new session, ? for help, q / ctrl+c to quit,
// ctrl+b (and enter on a highlighted row) to move focus to the
// pane. This is the only path that can emit ActionNewSession or
// tea.Quit — pane focus never triggers a TUI-level lifecycle
// event because every keystroke there belongs to claude.
func (m Model) handleKeyInSidebarFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Close-session confirmation state takes precedence over every
	// other binding: once `x` has armed a close, the user's next
	// keystroke is either the y/Y confirmation or an implicit
	// cancel. This is deliberately NOT a modal sub-view — it is a
	// single-keystroke handoff that keeps the rest of the sidebar
	// visible, so the user never loses context on what they are
	// about to close.
	if m.pendingCloseWindow != "" {
		target := m.pendingCloseWindow
		m.pendingCloseWindow = ""
		switch msg.String() {
		case "y", "Y":
			return m, m.closeManagedWindow(target)
		}
		// Any other key cancels. We intentionally swallow the key
		// rather than re-process it (e.g. `n` after a cancel does
		// NOT create a new session) to avoid a chain where a
		// mis-type produces an unrelated side effect.
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c", "q":
		// Quit with no follow-up action. The caller will see
		// ActionNone and simply return. Note: live claude
		// sessions are deliberately LEFT RUNNING on the sm4c
		// tmux socket — quit is non-destructive, a re-launched
		// `sm4c` rejoins the same server and finds the sessions
		// intact. Users who want to close a session do so from
		// within claude (/exit).
		m.action = ActionNone
		m.quitting = true
		return m, tea.Quit

	case "n":
		// Signal "spawn a new session" and exit the Bubble Tea
		// runtime so the CLI layer can do the tmux round-trip
		// without holding the raw-mode terminal. The CLI then re-
		// enters the TUI with the new window ID as initial
		// highlight and FocusPane so the user can type
		// immediately into the freshly-spawned session.
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

	case "enter", "ctrl+b":
		// Both keys move focus to the pane. Enter is a discoverable
		// shortcut ("highlight then press Enter to drop in"); ctrl+b
		// is the VSCode-style toggle that pairs with its same-name
		// binding in the pane focus branch. We gate on "there is a
		// highlightable session": pressing either on an empty
		// sidebar is a no-op so we never leave the user typing
		// into nothing.
		if m.highlight < 0 || m.highlight >= len(m.sessions) {
			return m, nil
		}
		m.focus = FocusPane
		return m, tea.Batch(
			m.resolveHighlightedPaneIfNeeded(),
			m.resizeHighlightedWindow(),
		)

	case "j", "down":
		// Move highlight down, no wrap. A no-wrap bottom is the
		// same convention tmux's choose-tree and vim's :ls use —
		// wrapping makes skimming a list feel disorienting.
		if m.highlight < len(m.sessions)-1 {
			m.highlight++
		}
		return m, tea.Batch(
			m.resolveHighlightedPaneIfNeeded(),
			m.resizeHighlightedWindow(),
		)

	case "k", "up":
		if m.highlight > 0 {
			m.highlight--
		}
		return m, tea.Batch(
			m.resolveHighlightedPaneIfNeeded(),
			m.resizeHighlightedWindow(),
		)

	case "x":
		// Arm a close on the currently-highlighted session. We
		// gate on "closer wired AND a real highlightable row";
		// either missing piece makes this a no-op. The actual
		// kill-window call does not fire yet — it runs after the
		// next keystroke confirms via `y`.
		if m.windowCloser == nil {
			return m, nil
		}
		if m.highlight < 0 || m.highlight >= len(m.sessions) {
			return m, nil
		}
		wid := m.sessions[m.highlight].WindowID
		if wid == "" {
			return m, nil
		}
		m.pendingCloseWindow = wid
		return m, nil

	case "z":
		// "Zoom": hide the sidebar and move focus into the pane
		// so the right pane gets the full viewport. This is the
		// tmux <prefix>-z convention, adapted to sm4c's single-
		// window layout. Two gates:
		//
		//   - A session must be highlightable. Without one, the
		//     zoomed view would be a blank right pane with no way
		//     to spawn a session (the `n` binding lives in the
		//     sidebar). We no-op instead of trapping the user.
		//   - paneResolver must be wired. On the test / headless
		//     path we have no pane to focus into; hiding the
		//     sidebar there would hide the only visible UI.
		//
		// The sidebar is restored via ctrl+b from pane focus,
		// which also pulls focus back. Keeping the restoration
		// path on the existing ctrl+b binding means users do not
		// need to learn a second shortcut — and pairs naturally
		// with the "ctrl+b always gets you to the sidebar"
		// mental model.
		if m.highlight < 0 || m.highlight >= len(m.sessions) {
			return m, nil
		}
		m.sidebarHidden = true
		m.focus = FocusPane
		// The viewport geometry changed: the right pane just
		// grew to full width. Push the new dims into the pane
		// emulator and tell tmux so claude redraws for the new
		// grid. Without this, the emulator keeps drawing into
		// the smaller pre-zoom width and the pane looks
		// truncated until the next real WindowSizeMsg.
		m.syncPaneViewport()
		return m, tea.Batch(
			m.resolveHighlightedPaneIfNeeded(),
			m.resizeHighlightedWindow(),
		)

	case "?":
		// Toggle the expanded help block. Unlike quit/new, this does
		// not exit — it just flips a render flag.
		m.help = !m.help
		return m, nil
	}
	return m, nil
}

// handleKeyInPaneFocus forwards the keystroke to the highlighted
// session's active pane. ctrl+b is the single sm4c-reserved
// shortcut here; everything else — including ctrl+c — is routed
// to claude via send-keys. That matches the convention of every
// terminal multiplexer and gives the user the intuitive escape
// sequence: "press ctrl+b to leave claude's input, then q to
// quit sm4c".
//
// The keystroke is dropped without error in a handful of
// recoverable cases: no session highlighted, no pane resolved
// yet, no sender wired, or keyMsgToBytes could not translate the
// keypress (e.g. a bubbletea-specific meta key we have not
// mapped). A dropped keystroke is always preferable to sending
// the wrong bytes to claude.
func (m Model) handleKeyInPaneFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+b" {
		m.focus = FocusSidebar
		// If the sidebar was zoomed away, ctrl+b is the single
		// restoration path: pull the sidebar back AND shrink the
		// right pane to re-accommodate it. We re-run the viewport
		// sync and push a fresh resize to tmux so claude redraws
		// for the reduced grid instead of clipping at the old
		// full-width geometry. The same flow runs on every ctrl+b
		// whether or not the sidebar was hidden — the resize is a
		// no-op via syncPaneViewport's prev==curr guard when
		// dimensions did not actually change, so we pay nothing
		// extra in the common case.
		if m.sidebarHidden {
			m.sidebarHidden = false
			m.syncPaneViewport()
			return m, tea.Batch(
				m.resolveHighlightedPaneIfNeeded(),
				m.resizeHighlightedWindow(),
			)
		}
		return m, nil
	}
	if m.highlight < 0 || m.highlight >= len(m.sessions) {
		return m, nil
	}
	paneID, ok := m.paneByWindow[m.sessions[m.highlight].WindowID]
	if !ok || paneID == "" {
		return m, nil
	}
	data := keyMsgToBytes(msg)
	if len(data) == 0 {
		return m, nil
	}
	// Bracketed paste: when bubbletea signals that this KeyMsg originated
	// from a clipboard paste (msg.Paste == true), wrap the content with
	// the standard ESC[200~ / ESC[201~ markers before forwarding. Without
	// them the target pane treats every embedded newline as "submit prompt"
	// rather than "literal newline". bubbletea enables bracketed paste
	// mode by default, so pastes arrive here with Paste == true rather
	// than as individual KeyRunes events.
	if msg.Paste {
		wrapped := make([]byte, 0, len(data)+12)
		wrapped = append(wrapped, "\x1b[200~"...)
		wrapped = append(wrapped, data...)
		wrapped = append(wrapped, "\x1b[201~"...)
		data = wrapped
	}
	// notePaneKeystroke clears any Done/Waiting glyph back to Idle so the
	// user gets immediate acknowledgement that their input was registered.
	// The spinner (Working) is NOT set here — it arrives via the
	// UserPromptSubmit hook. statusTickArmed is reset when Done fires (in
	// the hookMsg handler) so the hook-driven Working transition always
	// arms the tick cleanly without needing to arm it here.
	m.notePaneKeystroke(paneID)
	return m, m.sendKeysToPane(paneID, data)
}

// handleKeysSent reacts to the KeySender round-trip finishing.
// The only error we care about is "pane gone" — that means the
// session closed between our forward and tmux's receive, and
// staying in pane focus would confuse the user. Every other
// error is absorbed silently; one dropped keystroke on a
// transient tmux hiccup is preferable to flashing an error line
// on every Enter.
func (m Model) handleKeysSent(msg keysSentMsg) Model {
	if msg.err == nil {
		return m
	}
	if errors.Is(msg.err, errPaneGone) {
		m.focus = FocusSidebar
		return m
	}
	return m
}

// handleWindowClosed folds the result of a WindowCloser round-trip
// back into the Model. On success (or on an already-gone window,
// which we treat as success-shaped) we drop any cached per-pane
// state for the departed window so the right pane snaps away
// cleanly, and we immediately re-fetch the session list so the
// sidebar row disappears without waiting for the next poll tick.
//
// We also invalidate the capture state of every SURVIVING pane.
// When tmux kills the active window of the attached session, it
// switches the control client to a different window — and that
// window-switch can dribble a partial redraw into the %output
// stream (cursor repositioning + row-targeted rewrites without
// a CSI 2J clear), which gets layered on top of the pre-close
// emulator contents and produces the "mostly-right but with
// stale rows peeking through" symptom. Forcing a re-capture on
// each survivor's next resolve pulls a fresh, authoritative
// snapshot from tmux's grid, which paints over the mixed state
// in one shot. We do NOT preemptively re-resolve here: the
// fetchSessions round-trip we are about to kick off will
// naturally call resolveHighlightedPaneIfNeeded for the
// newly-promoted highlight, and the non-highlighted survivors
// will pick up their re-capture the next time the user
// navigates to them.
//
// A genuine failure (tmux reachable but kill-window failed for an
// unexpected reason — e.g. permissions on a shared socket) is
// surfaced as the listErr hint; the user can try again. We do
// NOT re-arm pendingCloseWindow: one `x` press is one close
// attempt, to keep the UX predictable across flaky sockets.
func (m Model) handleWindowClosed(msg windowClosedMsg) (tea.Model, tea.Cmd) {
	if msg.windowID == "" {
		return m, nil
	}
	if msg.err != nil && !errors.Is(msg.err, errPaneGone) {
		// We don't have a direct tmuxctl.ErrNoSuchWindow import,
		// but the adapter in cmd/sm4c/cli/tui.go folds that case
		// into a nil error before returning here — so any non-nil
		// err on this path is a real problem worth surfacing.
		m.listErr = msg.err
		return m, nil
	}
	// Drop cached per-pane state so the right pane stops trying
	// to render a ghost of the closed session while we wait for
	// the sessionsMsg refresh to land.
	closedPaneID := ""
	if paneID, ok := m.paneByWindow[msg.windowID]; ok {
		closedPaneID = paneID
		delete(m.paneTerminals, paneID)
		delete(m.paneCapturing, paneID)
		delete(m.paneCaptured, paneID)
		delete(m.panePending, paneID)
		delete(m.paneToWindow, paneID)
	}
	delete(m.paneByWindow, msg.windowID)
	delete(m.paneErrByWindow, msg.windowID)
	delete(m.paneStatuses, msg.windowID)
	if m.skipCaptureWindow == msg.windowID {
		m.skipCaptureWindow = ""
	}
	// Invalidate survivors. Clearing paneByWindow forces
	// resolveHighlightedPaneIfNeeded to re-issue an ActivePane
	// round-trip, which feeds back into handlePaneResolved and
	// re-arms captureActivePane for the surviving window that
	// the user is about to see highlighted.
	for wid, paneID := range m.paneByWindow {
		if paneID == closedPaneID {
			// Defensive: a resurrected stale entry for the
			// closed pane would spoil the invalidation loop.
			delete(m.paneByWindow, wid)
			continue
		}
		delete(m.paneTerminals, paneID)
		delete(m.paneCapturing, paneID)
		delete(m.paneCaptured, paneID)
		delete(m.panePending, paneID)
		delete(m.paneToWindow, paneID)
		// paneStatuses is intentionally NOT cleared here.
		// Status is keyed by window ID and reflects a rolling
		// byte-stream timeline; tmux's partial redraw on
		// window-close does not invalidate whether the
		// surviving claude is Working or Idle. Leaving the
		// record in place keeps the sidebar glyph stable
		// across a close-session event — its recompute via
		// statusForWindow does not depend on paneByWindow.
		delete(m.paneByWindow, wid)
	}
	// Nuke sizedFor entirely so the post-close resize pass
	// re-emits a resize for every surviving window the user
	// might land on next (j/k past the new top), and arm the
	// force-redraw one-shot so the next resize is the wiggle
	// form (see forceResizeManagedWindow). The wiggle is the
	// only resize shape that guarantees SIGWINCH on a pane
	// whose current dims already match our target — which is
	// the common case on close, because paneViewW/paneViewH
	// do not change when a window disappears. Without the
	// wiggle, tmux no-ops the same-size resize, claude never
	// redraws, and the surviving pane's emulator keeps the
	// stale cursor left at the bottom of the grid by the
	// pre-close capture-pane backfill — with the result that
	// the user's next keystrokes echo into the bottom rows of
	// the preview instead of inside claude's input box.
	m.sizedFor = make(map[string][2]int)
	m.forceResizePending = true
	// Reset the resolve latch too, otherwise
	// resolveHighlightedPaneIfNeeded will short-circuit on the
	// next sessionsMsg because it thinks the highlight window is
	// already resolved.
	m.resolvedWindowID = ""
	return m, m.fetchSessions()
}

// errPaneGone is the sentinel KeySender implementations should
// wrap (via fmt.Errorf with %w, or by returning directly) when
// the target pane has disappeared between keystroke and forward.
// We declare it at the TUI level — not in tmuxctl — because the
// TUI must not import tmuxctl, and bubbling a concrete error
// type would violate that boundary. The CLI layer's KeySender
// adapter maps tmuxctl.ErrNoSuchPane to this sentinel.
var errPaneGone = errors.New("tui: pane gone")

// ErrPaneGone exposes errPaneGone to the CLI layer so the
// KeySender adapter it wires into Deps can translate
// tmuxctl.ErrNoSuchPane without the TUI package importing
// tmuxctl. Tests use the same symbol to synthesize pane-gone
// responses from stub senders.
func ErrPaneGone() error { return errPaneGone }

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

// sidebarBindings is the authoritative list of keys the sidebar
// view advertises when focus is on the sidebar. renderHelp
// iterates this slice for that focus, so any key added here
// appears in the expanded help block.
var sidebarBindings = []keybind{
	{"j/k", "move highlight"},
	{"enter", "focus session"},
	{"ctrl+b", "focus session"},
	{"n", "new session"},
	{"x", "close session"},
	{"z", "hide sidebar"},
	{"?", "toggle help"},
	{"q", "quit"},
}

// paneBindings is the list shown when focus is on the pane. In
// this mode every keystroke goes to claude; the only sm4c-
// reserved shortcut is ctrl+b (back to the sidebar, also
// un-hides the sidebar if it was collapsed via `z`). The help
// block spells out that q / ctrl+c now forward to claude so the
// user is not surprised.
var paneBindings = []keybind{
	{"ctrl+b", "back to sidebar"},
	{"(any)", "typed into claude"},
}

// bindingsForFocus returns the binding table that matches the
// current focus. Kept as a single choke-point so renderHelp
// and any future view that wants to advertise the active
// bindings stay consistent.
func (m Model) bindingsForFocus() []keybind {
	if m.focus == FocusPane {
		return paneBindings
	}
	return sidebarBindings
}

// renderCloseConfirm returns the one-line confirmation prompt
// shown while pendingCloseWindow is armed. Returns "" when no
// close is pending so the caller can fall back to the standard
// key bar. The name is looked up from the current sessions slice
// rather than cached on the Model so the prompt always reflects
// the latest rename state (claude's /rename writes through via
// list-windows polling).
func (m Model) renderCloseConfirm() string {
	if m.pendingCloseWindow == "" {
		return ""
	}
	name := m.pendingCloseWindow
	for _, s := range m.sessions {
		if s.WindowID == m.pendingCloseWindow {
			if s.Name != "" {
				name = s.Name
			}
			break
		}
	}
	prompt := "close " + name + "?"
	return m.chip().Render(" y ") + "  " +
		keyDescStyle.Render(prompt+" (any other key cancels)")
}

// renderHelp is the expanded help block shown at the bottom of
// the sidebar when the user presses `?`. Each binding row uses
// keyStyle (reverse-video chip) for the key and plain text for
// the description; a two-space gap aligns keys across rows
// even when key names have different widths. The leading
// "keys" title doubles as the "this is the help block" marker
// for tests.
//
// Pre-M3d polish this block was a near-copy of a now-removed
// renderKeys, which rendered the same list unconditionally in
// the compact area under the status line. Hiding the list
// behind `?` removed the permanent vertical cost while
// preserving every advertised binding for users who ask for
// them. If the help grows into a multi-section cheatsheet
// (navigation, session lifecycle, status legend), that growth
// can land here without reopening the "should this be visible
// by default?" question.
func (m Model) renderHelp() string {
	lines := []string{titleStyle.Render("keys")}
	for _, b := range m.bindingsForFocus() {
		lines = append(lines, "  "+m.chip().Render(b.key)+"  "+keyDescStyle.Render(b.desc))
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

// RightPaneBodyDims returns the interior dimensions of the right
// pane for a given terminal size, using the same layout math the
// Model applies internally on each tea.WindowSizeMsg. It is exposed
// so the CLI can pre-size a freshly-spawned tmux window to the
// same viewport the TUI is about to claim, eliminating the race
// between claude drawing at tmux's default 80x24 grid and the
// TUI's async resize-window round-trip. Without pre-sizing, the
// emulator briefly captures and re-lays-out content at the old
// size, which shows up as visible distortion until the user
// forces a terminal resize.
//
// Returns (0, 0) if the terminal is too narrow to split (same
// stacked-fallback threshold the renderer uses) or the computed
// body region is non-positive. Callers should treat (0, 0) as
// "no pre-sizing hint" and skip the resize round-trip.
//
// This function assumes the sidebar is VISIBLE. The zoomed
// (sidebar-hidden) layout is internal to the Model and flows
// through rightPaneBodyDimsFullWidth below; callers outside this
// package (the CLI pre-sizer) always see the sidebar-visible
// geometry because a freshly-launched TUI never starts zoomed.
func RightPaneBodyDims(termW, termH int) (int, int) {
	if termW < minSplitWidth || termH < 1 {
		return 0, 0
	}
	sidebarW := termW / 3
	if sidebarW < sidebarMin {
		sidebarW = sidebarMin
	}
	if sidebarW > sidebarMax {
		sidebarW = sidebarMax
	}
	rightW := termW - sidebarW - 1
	if rightW < 1 {
		return 0, 0
	}
	return rightPaneInteriorDims(rightW, termH)
}

// rightPaneBodyDimsFullWidth returns the right-pane interior
// dimensions when the sidebar is collapsed (zoom mode). It is
// the sibling of RightPaneBodyDims for the sidebar-hidden path.
// Kept internal because no out-of-package caller has a reason
// to pre-size for the zoomed layout — zoom is always entered
// interactively, never at launch.
func rightPaneBodyDimsFullWidth(termW, termH int) (int, int) {
	if termW < 1 || termH < 1 {
		return 0, 0
	}
	return rightPaneInteriorDims(termW, termH)
}

// rightPaneInteriorDims converts "outer right-pane column width"
// into "body width × body height" by subtracting the padding and
// header chrome that rightPaneStyle / renderRightPane each add.
// Single source of truth so the visible and the zoomed layouts
// cannot drift on future edits.
func rightPaneInteriorDims(colW, termH int) (int, int) {
	// rightPaneStyle has Padding(0, 1) — 1 col on each side.
	bodyW := colW - 2
	if bodyW < 1 {
		return 0, 0
	}
	// Header + blank separator = 2 lines consumed before body.
	bodyH := termH - 2
	if bodyH < 1 {
		return 0, 0
	}
	return bodyW, bodyH
}

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
	// Zoom path: sidebar is collapsed, the right pane takes the
	// entire viewport. We deliberately skip the sidebar column AND
	// its right border here instead of rendering a 0-wide column,
	// because lipgloss would still paint the border glyph of a
	// zero-width bordered style and leave a dangling line on the
	// left edge. Zoom is the only mode where we render the right
	// pane alone; every other path flows through the split layout
	// below.
	if m.sidebarHidden {
		if m.width < 1 || m.height < 1 {
			// No geometry yet (test path / mid-launch). Fall
			// back to the stacked sidebar render so the user
			// is not looking at literally nothing while bubbletea
			// boots.
			return m.renderSidebarColumn() + "\n"
		}
		return rightPaneStyle.
			Width(m.width).
			Height(m.height).
			Render(m.renderRightPane())
	}

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

// renderSidebarColumn builds the stacked content that lives inside
// the sidebar column. Layout:
//
//	┌──────────────────────┐
//	│ header               │  ← sm4c / N sessions
//	│                      │
//	│ session list         │  ← status glyph + name rows
//	│                      │
//	│ (list err, if any)   │  ← faint one-liner on fetch failures
//	│                      │
//	│   (vertical filler)  │  ← stretches to fill the column height
//	│                      │
//	│ help block / footer  │  ← anchored at the bottom
//	└──────────────────────┘
//
// The footer is one of three mutually-exclusive states:
//
//  1. close prompt        — while pendingCloseWindow is armed
//  2. full bindings list  — while m.help is true (expanded help)
//  3. "? help" hint       — default
//
// The pre-M3d layout showed every binding at all times in a
// compact key bar directly under the session list. Live usage
// surfaced two problems: the bar crowded the session list even
// when the user was not looking at bindings, and the bindings
// themselves were too subtle to teach new users. Hiding the
// full list behind `?` gets the sidebar out of the way for the
// common case (users already know j/k/n/x) while making the
// help experience more deliberate and more legible when the
// user actually wants it. The `?` hint stays visible at the
// bottom so the discovery path is preserved.
//
// Extracted so it can be reused verbatim in both the split
// layout and the narrow-terminal fallback.
func (m Model) renderSidebarColumn() string {
	top := m.renderSidebarTop()
	bottom := m.renderSidebarBottom()

	// Vertical filler: push the bottom block to the actual
	// bottom of the sidebar column. We target m.height (the
	// viewport height, kept current by the WindowSizeMsg
	// handler). When m.height is 0 — which happens in tests
	// that never emit a size and on the narrow-terminal
	// fallback path where lipgloss does not pad for us — we
	// fall back to a single-blank-line separator so the
	// content still reads correctly even without a pin at
	// the bottom of the viewport.
	if m.height <= 0 {
		if top == "" {
			return bottom
		}
		if bottom == "" {
			return top
		}
		return top + "\n\n" + bottom
	}
	topH := 0
	if top != "" {
		topH = lipgloss.Height(top)
	}
	bottomH := 0
	if bottom != "" {
		bottomH = lipgloss.Height(bottom)
	}
	// One blank line separator above the bottom block so the
	// footer never butts up against the session list or the
	// fetch-error line even when the sidebar is short enough
	// that the filler would otherwise collapse to zero rows.
	filler := m.height - topH - bottomH - 1
	if filler < 1 {
		filler = 1
	}
	parts := []string{top}
	if filler > 0 {
		parts = append(parts, strings.Repeat("\n", filler-1))
	}
	if bottom != "" {
		parts = append(parts, bottom)
	}
	return strings.Join(parts, "\n")
}

// renderSidebarTop builds the always-anchored-to-the-top portion
// of the sidebar: header, session list, and the fetch-error
// line (when present). Extracted from renderSidebarColumn so
// the height math that positions renderSidebarBottom has a
// clean single-call surface to measure.
func (m Model) renderSidebarTop() string {
	sections := []string{
		m.renderHeader(),
		"",
		m.renderSessionList(),
	}
	if m.listErr != nil {
		// A fetch error is usually a preflight issue (e.g. tmux
		// socket permissions flipped mid-run). We surface it
		// faintly so the user sees a clue without the sidebar
		// becoming a stack trace.
		sections = append(sections, "")
		sections = append(sections, hintStyle.Render(
			"session fetch error: "+m.listErr.Error()))
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// renderSidebarBottom builds the bottom-anchored portion of the
// sidebar. The three mutually-exclusive layouts are documented
// on renderSidebarColumn. The empty-state disclaimer ("sm4c is
// not affiliated with Anthropic…") is folded into this block
// so first-time users see it at the natural "where am I?"
// glance point — the bottom of the sidebar — rather than
// scrolling off the top.
func (m Model) renderSidebarBottom() string {
	var sections []string

	// 1. Close prompt trumps everything: while a kill-window
	//    confirmation is pending, the only meaningful keys
	//    are y/any, and the normal bindings would dilute that
	//    signal. The close-prompt layout is its own single line
	//    so it reads as a focused question.
	if prompt := m.renderCloseConfirm(); prompt != "" {
		sections = append(sections, prompt)
	} else if m.help {
		// 2. Expanded help: show every binding for the current
		//    focus, plus the "? help" toggle so the user
		//    remembers how to close the block. The toggle is
		//    rendered last (after the bindings) so the visual
		//    hierarchy matches the action: read the list, press
		//    `?` again to dismiss.
		sections = append(sections, m.renderHelp())
	} else {
		// 3. Default: a single compact hint. Discovery is
		//    preserved ("press `?` to see everything"), but the
		//    sidebar stays uncluttered for the common case.
		sections = append(sections, m.renderHelpHint())
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

// renderHelpHint returns the single compact "? help" row that
// lives at the bottom of the sidebar in the default (help-off)
// state. Kept as its own helper so tests can target the
// "there is a discoverable help hint" contract without coupling
// to the full binding list that the expanded help block uses.
func (m Model) renderHelpHint() string {
	return m.chip().Render("?") + "  " + keyDescStyle.Render("help")
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
	return m.renderRightPaneHeader() + "\n\n" + m.renderRightPaneBody()
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
	// Window ID (@N) is intentionally omitted from the header —
	// it is an opaque tmux identifier users never refer to by
	// hand, and live usage confirmed nobody was reading it. The
	// session name is the only user-meaningful label in this
	// slot; `sm4c ls` still surfaces IDs for debugging. This
	// matches the same decision the sidebar row format made in
	// M3d polish (see renderSessionList).
	line := titleStyle.Render(name)
	// Focus indicator: a bracketed tag lets the user tell at a
	// glance which surface owns keystrokes. We avoid relying on
	// border color (sm4c's no-hex-colors rule) and keep the
	// signal purely in the text channel so every terminal
	// renders it identically.
	if m.focus == FocusPane {
		line += "  " + m.chip().Render("[focus]")
		// When the sidebar is zoomed away, spell out the
		// restoration path right next to the focus chip —
		// the only reserved shortcut in this mode is ctrl+b,
		// and "show sidebar" carries more semantic weight than
		// "back to sidebar" because there is no visible sidebar
		// to go back to.
		if m.sidebarHidden {
			line += "  " + hintStyle.Render("[ctrl+b: show sidebar]")
		}
	} else {
		line += "  " + hintStyle.Render("[ctrl+b to focus]")
	}
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
		return hintStyle.Render("pane preview unavailable")
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

// renderSessionList emits one "card" per session. The card is up
// to two lines:
//
//	<status-glyph> <name>
//	  <short-cwd>                              [faint, optional]
//
// The second line is omitted when Session.Cwd is empty (tmux has
// not yet observed a cwd for the pane, or the backend is the
// test stub). shortPath trims the home prefix to "~" and
// truncates the head to "…" when the path would overflow the
// sidebar column, so the line is always single-wrap-safe at
// sidebarMax.
//
// Highlight is a full-width "card" band: the row styled with
// lipgloss.Width(sidebarContentW) is padded with spaces out to
// the column edge before the Reverse attribute is applied, so
// the selection reads as a light block spanning the whole
// sidebar instead of a reverse-video run that ends at the glyph
// text. This matches the claude-squad / Cursor convention and
// makes the cursor obvious at a glance, even on themes where
// reverse-video alone is subtle. On the unit-test path (width
// == 0) we fall back to the single-line rowHighlightStyle so
// the "highlighted row contains <name> in reverse" contract
// stays stable for Tests that never emit a WindowSizeMsg.
//
// Cards are separated by a single newline (no extra blank line
// between items — live feedback found double spacing too loose).
//
// Earlier iterations trailed the first line with the opaque
// tmux window ID ("@3") rendered faintly. Live usage found the
// ID was pure noise and the ID column was stealing horizontal
// space on narrow sidebars. Window IDs are still available via
// `sm4c ls` for debugging.
//
// When the sessions slice is empty we emit a single faint
// placeholder row so the sidebar stays visible with its key bar,
// rather than collapsing to a header + footer. This is the
// "sidebar is always present" design constraint made literal.
func (m Model) renderSessionList() string {
	if len(m.sessions) == 0 {
		return hintStyle.Render("  no sessions yet — press n to start one")
	}
	contentW := m.sidebarContentWidth()
	cards := make([]string, 0, len(m.sessions))
	for i, s := range m.sessions {
		status := m.statusForWindow(s.WindowID)
		highlighted := i == m.highlight
		glyph := statusGlyph(status, m.statusFrame)
		if highlighted && contentW > 0 {
			glyph = statusGlyphPlain(status, m.statusFrame)
		}
		name := s.Name
		// Prefer the OSC terminal title captured by tmux (#{pane_title})
		// over the static tmux window name. Claude Code sets this via
		// OSC 0/2; tmux intercepts it and exposes it through the
		// list-windows poll, so the sidebar reflects the dynamic
		// session/project label rather than the hardcoded "claude" name.
		if s.Title != "" {
			name = stripTitleIcon(s.Title)
		}
		if name == "" {
			// tmux always has a window name; an empty one would
			// indicate a parser bug in tmuxctl. Render a sentinel
			// so the sidebar doesn't become an empty column.
			name = "(unnamed)"
		}
		card := m.renderSessionCard(glyph, name, s.Cwd, contentW, highlighted)
		cards = append(cards, card)
	}
	return strings.Join(cards, "\n")
}

// renderSessionCard formats a single session row. Extracted so
// the multi-branch logic (one-line vs two-line; filled-band vs
// inline-highlight) lives in one readable place instead of
// being inlined into the loop above.
//
// Width accounting. Both cardBaseStyle and sidebarHighlightStyle
// apply PaddingLeft(1) plus vertical padding — outer width = Width
// param; inner text width subtracts cardPaddingW (left inset only).
func (m Model) renderSessionCard(glyph, name, cwd string, contentW int, highlighted bool) string {
	header := m.sessionCardHeader(glyph, name, highlighted)

	// textW is the width available for actual glyphs inside
	// horizontal padding. Used to truncate the cwd line so it never
	// wraps into a third row.
	textW := 0
	if contentW > cardPaddingW {
		textW = contentW - cardPaddingW
	}

	// Second line: the short path. Indent by two columns so it
	// lines up visually under the name (the status glyph
	// occupies the leading two cells). We render the indent as
	// part of the unstyled string and let hintStyle color only
	// the path portion — this way the card's background fills
	// underneath the indentation too and the selection band
	// reads as a contiguous bar.
	var body string
	if short := shortPath(cwd); short != "" {
		// Clamp the path to the text width minus the indent so
		// the faint line never wraps into a third row. On the
		// unit-test path (textW == 0) the path is left
		// un-truncated; lipgloss will not force a wrap without
		// a Width constraint on the enclosing style.
		if textW > 2 {
			short = truncLeft(short, textW-2)
		}
		if highlighted {
			body = "  " + sidebarHighlightPathStyle(m.sidebarHighlightFG).Render(short)
		} else {
			body = "  " + hintStyle.Render(short)
		}
	}

	card := header
	if body != "" {
		card = header + "\n" + body
	}

	// Highlight path: the selected card renders as a solid
	// full-width band with an ANSI-8 lighter background (see
	// cardHighlightStyle in style.go). The base card renders
	// with matching padding and no background so both occupy
	// the same outer width — the list stays visually stable as
	// the user cursors through it. When contentW is zero (test
	// / stacked-fallback geometry) we degrade to the pre-M3e
	// single-glyph highlight so existing substring-based tests
	// keep matching.
	if contentW > 0 {
		if highlighted {
			return sidebarHighlightStyle(m.sidebarHighlightBG, m.sidebarHighlightFG).
				Width(contentW).
				Render(card)
		}
		return cardBaseStyle.Width(contentW).Render(card)
	}
	if highlighted {
		return rowHighlightStyle.Render(card)
	}
	return card
}

// sessionCardHeader renders the first line of a session card. The
// session name is always bold; on the highlighted row it also
// uses the configured highlight foreground. The status glyph stays
// unstyled so braille/●/✓ colors stay consistent.
func (m Model) sessionCardHeader(glyph, name string, highlighted bool) string {
	nameStyle := lipgloss.NewStyle().Bold(true)
	if highlighted {
		fg := m.sidebarHighlightFG
		if fg == "" {
			fg = defaultSidebarHighlightFG
		}
		nameStyle = nameStyle.Foreground(lipgloss.Color(fg))
	}
	return glyph + nameStyle.Render(name)
}

// chip returns a chipStyle using this model's configured highlight colors.
func (m Model) chip() lipgloss.Style {
	bg := m.sidebarHighlightBG
	if bg == "" {
		bg = defaultSidebarHighlightBG
	}
	fg := m.sidebarHighlightFG
	if fg == "" {
		fg = defaultSidebarHighlightFG
	}
	return chipStyle(bg, fg)
}

// cardPaddingW is the total horizontal padding both card variants
// consume — one column of PaddingLeft only.
const cardPaddingW = 1

// sidebarContentWidth returns the content-area width inside the
// sidebar column (sidebarColumnStyle has no horizontal padding).
// On the test / narrow-terminal paths where we never got a
// WindowSizeMsg, returns 0 to signal "unconstrained" — callers
// downgrade to pre-M3e rendering paths that don't depend on a
// known column width.
func (m Model) sidebarContentWidth() int {
	if m.width < minSplitWidth || m.height < 1 {
		return 0
	}
	w := m.sidebarWidth()
	if w < 1 {
		return 0
	}
	return w
}

// shortPath normalizes an absolute filesystem path for display
// in the narrow sidebar column. Two transforms, in order:
//
//  1. Replace the user's home directory prefix with "~" (the
//     same convention shells and most developer TUIs use). If
//     homeDir() can't be resolved, we skip this step rather
//     than risk mis-trimming.
//  2. Leave overflow handling to truncLeft, which is applied
//     later at render time once the column width is known.
//
// stripTitleIcon removes the status icon that Claude Code prepends to the
// terminal title before the project/session name. Claude Code formats its
// title as "<icon> <name>" where <icon> is a single non-alphanumeric rune
// (a braille spinner frame, ✓, ?, or similar) followed by a space. The
// icon is decorative metadata that sm4c already surfaces through its own
// status glyph column, so we strip it to avoid showing it twice.
//
// The rule: if the first rune is not a Unicode letter or decimal digit,
// and the second rune is a space, drop both. Otherwise the title is
// returned as-is. This is intentionally narrow — only the single-char
// prefix pattern is matched — so titles that legitimately start with
// punctuation (e.g. "(archived) my-project") are left untouched.
func stripTitleIcon(title string) string {
	runes := []rune(title)
	if len(runes) >= 2 && !unicode.IsLetter(runes[0]) && !unicode.IsDigit(runes[0]) && runes[1] == ' ' {
		return strings.TrimSpace(string(runes[2:]))
	}
	return title
}

// Returns "" for empty input so callers can skip the whole
// "render a second line" branch with a cheap string check.
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	if home := homeDir(); home != "" && strings.HasPrefix(p, home) {
		rest := strings.TrimPrefix(p, home)
		switch {
		case rest == "":
			return "~"
		case strings.HasPrefix(rest, "/"):
			return "~" + rest
		}
	}
	return p
}

// homeDir returns the current user's home directory, cached for
// the life of the process. We look it up lazily (not at init) so
// a test that changes HOME before calling shortPath sees the new
// value on first invocation; subsequent calls hit the cache so
// render-time path shortening stays allocation-free. A lookup
// failure collapses to "" which makes shortPath a no-op and we
// fall back to the absolute path — degraded but still correct.
var (
	homeDirOnce sync.Once
	homeDirVal  string
)

func homeDir() string {
	homeDirOnce.Do(func() {
		if h, err := os.UserHomeDir(); err == nil {
			homeDirVal = h
		}
	})
	return homeDirVal
}

// truncLeft shortens s to at most max visible runes by dropping
// the head and prepending an ellipsis when necessary. We trim
// from the LEFT (not the right) because the meaningful portion
// of a working-directory path is the tail — "~/Repos/proj" is
// more useful than "~/Repositor…" when the user is trying to
// tell two sessions apart. Returns "" for non-positive max so
// the caller can skip rendering entirely.
func truncLeft(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return "…" + string(rs[len(rs)-(max-1):])
}

// statusForWindow resolves a tmux window ID to the user-facing
// SessionStatus. A window with no hook events yet is StatusQuiet.
func (m Model) statusForWindow(windowID string) SessionStatus {
	ps, ok := m.paneStatuses[windowID]
	if !ok {
		return StatusQuiet
	}
	return ps.derivedStatus()
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

// Run starts a Bubble Tea program with a Model constructed from
// deps, using the given input/output writers. `in` is normally
// os.Stdin and `out` os.Stdout; tests pass pipes so they can drive
// keystrokes programmatically.
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
// deps.InitialHighlight is an optional tmux window ID: when non-
// empty, the first sessionsMsg that contains a matching row snaps
// the highlight to that row instead of the default first-row
// behavior. This is how `sm4c [claude-args]` opens the TUI already
// focused on the freshly-spawned session without any exec-into-
// tmux shortcut.
func Run(
	in interface {
		Read(p []byte) (n int, err error)
	},
	out interface {
		Write(p []byte) (n int, err error)
	},
	deps Deps,
) (Model, error) {
	// Bind lipgloss's global renderer to this program's output so
	// termenv can detect color capability (ANSI vs 256 vs true
	// color) and downgrade 256-color indices when the terminal only
	// supports 16 ANSI colors. Without this, styles use
	// termenv.DefaultOutput() which may not match `out`.
	lipgloss.SetDefaultRenderer(lipgloss.NewRenderer(out))
	p := tea.NewProgram(
		NewModel(deps),
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

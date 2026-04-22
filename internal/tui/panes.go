package tui

import (
	"context"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

// panes.go carries the pane-preview data path: a PaneEvent value type, the
// two dependency-injection seams (PaneEventStream, ActivePaneResolver)
// the Model calls into, a per-pane VT emulator, and the message
// types the Bubble Tea runtime delivers.
//
// Design mirrors sessions.go on purpose: everything here is side-
// effect free from the Model's point of view. The TUI package never
// imports tmuxctl; the CLI layer (cmd/sm4c/cli/tui.go) builds the
// stream and resolver from tmuxctl.Client / tmuxctl.OneShot and
// injects them through NewModel / Run. Tests drop in in-memory stubs.

// PaneEvent is a single chunk of bytes produced by a tmux pane. One
// chunk corresponds to one `%output %<pane-id> …` control-mode
// notification; the stream carries chunks for every pane on the
// sm4c socket, and the TUI filters by pane ID.
//
// Data is the already-unescaped raw bytes the pane produced, exactly
// as they appeared on the pane's local terminal. Starting in M3b.2
// the TUI feeds these bytes into a charmbracelet/x/vt emulator
// so ANSI styling, cursor positioning, and line wrapping render
// identically to a native tmux attach.
type PaneEvent struct {
	PaneID string
	Data   []byte
}

// PaneEventStream returns a receive-only channel of PaneEvent values.
// The TUI consumes it via a single tea.Cmd that loops "read one event
// → emit tea.Msg → re-arm". A closed channel is the terminal signal:
// the Model switches the pane preview into a "stream closed" state
// and stops re-arming.
//
// A nil PaneEventStream is explicitly supported. It means "no live
// pane data" — the Model renders a static placeholder in the right
// pane. Every unit test that does not care about pane data leaves
// this nil. Production callers pass a stream backed by
// tmuxctl.Client.Events() filtered to OutputEvents.
type PaneEventStream func() <-chan PaneEvent

// ActivePaneResolver maps a tmux window ID to the active pane ID of
// that window. The TUI calls it asynchronously via tea.Cmd whenever
// the highlighted session changes, so a slow tmux round-trip never
// blocks keystrokes.
//
// A nil ActivePaneResolver is supported for the same reason
// PaneEventStream is: unit tests that exercise navigation and
// rendering without a live tmux server leave it nil, and the Model
// treats resolution as disabled.
type ActivePaneResolver func(ctx context.Context, windowID string) (paneID string, err error)

// PaneCapturer returns the currently-visible screen of a tmux pane
// as raw bytes with ANSI escapes preserved. The TUI calls it once
// per pane the first time that pane resolves, so switching to a
// session shows its current state immediately instead of waiting
// for the next live chunk. Subsequent %output notifications are
// appended after the capture is flushed into the emulator.
//
// A nil PaneCapturer is supported and disables backfill; the
// emulator still boots when the first live chunk arrives, so the
// feature is strictly additive. Production callers pass a func
// that wraps tmuxctl.OneShot.CapturePane.
type PaneCapturer func(ctx context.Context, paneID string) (data []byte, err error)

// KeySender forwards raw input bytes to a tmux pane. The TUI uses
// it to realize "focus on the pane means keystrokes flow into
// claude": every tea.KeyMsg that is not a sm4c-reserved shortcut
// (ctrl+b toggles focus back to the sidebar) is translated by
// keyMsgToBytes into the byte sequence a tty would emit for that
// keypress, and handed here.
//
// A nil KeySender is supported and disables input routing; the TUI
// still renders and navigates, but pane focus becomes effectively
// read-only. Production callers wrap tmuxctl.OneShot.SendKeys.
//
// Errors: when the target pane is gone (the window was closed
// mid-keystroke), implementations should return a sentinel that
// errors.Is against tmuxctl.ErrNoSuchPane so the TUI can revert
// focus to the sidebar on the next tick; other errors are
// absorbed silently (the next %output will paint whatever state
// tmux reached).
type KeySender func(ctx context.Context, paneID string, data []byte) error

// WindowResizer tells tmux to resize a window's cell grid to
// (width, height). The TUI invokes it whenever its right-pane
// viewport changes (terminal resized) or the highlight lands on a
// different window, so claude always draws into a grid that
// matches what sm4c renders.
//
// A nil WindowResizer disables sync. In that mode wrapping will
// drift after a terminal resize until the user switches sessions
// (claude's redraw path reacts to tmux's next size notification,
// not ours), which is acceptable for tests and for environments
// where the user's tmux is pre-3.2. Production callers wrap
// tmuxctl.OneShot.ResizeWindow.
type WindowResizer func(ctx context.Context, windowID string, width, height int) error

// WindowCloser terminates a tmux window (the "close session" action)
// identified by its tmux window ID. The TUI invokes it when the user
// confirms an `x` close in the sidebar: killing the window sends
// SIGHUP to claude inside the pane, which is the same lifecycle
// effect as running `/exit` in claude — the session goes away and
// the next sessionsMsg poll drops its row.
//
// A nil WindowCloser disables the close-session binding: `x` becomes
// a no-op in sidebar focus. Production callers wrap
// tmuxctl.OneShot.KillWindow. Implementations should treat an
// already-gone window as a successful outcome (user's intent is
// "this session should no longer exist", which is true either way).
type WindowCloser func(ctx context.Context, windowID string) error

// defaultPaneWidth / defaultPaneHeight are the emulator dimensions
// used before we have seen a tea.WindowSizeMsg. They roughly match
// an 80x24 terminal so a fresh model with no resize still produces
// sensible output in unit tests. Once the runtime delivers a real
// size, resizePaneTerminals replaces these on every existing pane
// emulator.
const (
	defaultPaneWidth  = 80
	defaultPaneHeight = 24
)

// paneScrollStep is the number of terminal rows moved per mouse-wheel tick.
const paneScrollStep = 3

// paneTerminal pairs a VT emulator with rendering state.
//
// written tracks whether any bytes have been fed to the emulator; it lets
// renderRightPaneBody distinguish "emulator exists but is still the
// post-boot blank screen" from "emulator has drawn something", so we can
// keep showing a "waiting for output" hint until claude actually emits
// bytes instead of flashing an empty grid.
//
// scrollOffset is the number of rows above the live view that are
// currently visible. 0 means the live bottom of the screen; a positive
// value means the user has scrolled back into the scrollback buffer.
// Scrolling is always clamped to [0, scrollback.Len()].
type paneTerminal struct {
	emu          *vt.Emulator
	written      bool
	scrollOffset int
}

// newPaneTerminal constructs a fresh emulator at the given
// dimensions. Callers are expected to pass positive integers; width
// and height are clamped to a minimum of 1 on the emulator side, but
// we clamp defensively too to keep internal invariants obvious.
//
// We also start a background goroutine that drains the emulator's
// response pipe (emu.Read) into io.Discard. Claude emits device-
// attributes queries on startup (CSI c, CSI >0q, and friends); the
// charmbracelet/x/vt emulator answers those by queuing response
// bytes into an internal buffer exposed via Read. If nothing drains
// that buffer, the internal pipe fills up and any subsequent
// emu.Write call blocks forever — deadlocking the entire TUI
// goroutine, since Write is invoked from Bubble Tea's main loop
// during handlePaneData.
//
// We silently discard the responses because they are spurious in
// our topology: claude's actual PTY is on the tmux side, and tmux
// has already answered claude's queries on that PTY. Our emulator
// is a passive observer of the bytes claude has written, not the
// terminal claude believes it is attached to, so its generated
// responses have no correct destination.
//
// The goroutine exits when emu.Read returns an error, which
// happens when the emulator is closed via (*paneTerminal).close.
func newPaneTerminal(width, height int) *paneTerminal {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	emu := vt.NewEmulator(width, height)
	go func() {
		_, _ = io.Copy(io.Discard, emu)
	}()
	return &paneTerminal{emu: emu}
}

// write feeds a chunk of bytes to the emulator and records that we
// have drawn something. The VT parser accepts partial escape
// sequences across Write calls, so chunk boundaries from tmux
// `%output` notifications are safe.
func (p *paneTerminal) write(data []byte) {
	if p == nil || p.emu == nil || len(data) == 0 {
		return
	}
	_, _ = p.emu.Write(data)
	p.written = true
}

// resize updates the emulator's grid dimensions. No-op when either
// dimension is non-positive or the terminal is nil.
func (p *paneTerminal) resize(width, height int) {
	if p == nil || p.emu == nil {
		return
	}
	if width < 1 || height < 1 {
		return
	}
	p.emu.Resize(width, height)
}

// scroll adjusts scrollOffset by delta rows (positive = scroll up / older,
// negative = scroll down / newer). No-op when the emulator is in alt-screen
// mode (scrollback is nil). The offset is clamped to [0, scrollback.Len()].
func (p *paneTerminal) scroll(delta int) {
	if p == nil || p.emu == nil {
		return
	}
	sb := p.emu.Scrollback()
	if sb == nil {
		return // alt-screen mode has no scrollback
	}
	p.scrollOffset += delta
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
	if max := sb.Len(); p.scrollOffset > max {
		p.scrollOffset = max
	}
}

// render returns the emulator's current screen as an ANSI-encoded string:
// one line per row with SGR escapes embedded so colors and attributes
// survive the handoff to the outer terminal. When scrollOffset > 0, the
// view is a window into the scrollback buffer + the top of the live screen;
// otherwise it is the full live screen.
func (p *paneTerminal) render() string {
	if p == nil || p.emu == nil {
		return ""
	}
	if p.scrollOffset > 0 {
		return p.renderScrolled()
	}
	// cellbuf.Render() joins rows with \r\n. Strip the \r so Bubble Tea's
	// incremental diff sees plain \n-terminated lines: the \r would confuse
	// the visible-width measurement, causing the diff to skip "erase to end
	// of line" sequences and leave stale characters on screen.
	return strings.ReplaceAll(p.emu.Render(), "\r\n", "\n")
}

// renderScrolled builds the viewport when scrollOffset > 0. It combines
// the tail of the scrollback buffer (older rows) with the top of the live
// screen (newer rows) to fill exactly emu.Height() rows.
func (p *paneTerminal) renderScrolled() string {
	sb := p.emu.Scrollback()
	if sb == nil {
		return strings.ReplaceAll(p.emu.Render(), "\r\n", "\n")
	}
	sbLen := sb.Len()
	height := p.emu.Height()

	// Clamp in case scrollback shrunk since last scroll() call.
	offset := p.scrollOffset
	if offset > sbLen {
		offset = sbLen
		p.scrollOffset = sbLen
	}

	// Split the viewport into scrollback rows and live-screen rows.
	fromScrollback := offset
	fromScreen := height - fromScrollback

	lines := make([]string, 0, height)

	// Pull the last fromScrollback lines from the scrollback buffer.
	sbStart := sbLen - fromScrollback
	for i := sbStart; i < sbLen; i++ {
		line := sb.Line(i)
		if line == nil {
			lines = append(lines, "")
		} else {
			lines = append(lines, line.Render())
		}
	}

	// Fill the remainder from the top of the live screen.
	if fromScreen > 0 {
		screenStr := strings.ReplaceAll(p.emu.Render(), "\r\n", "\n")
		screenRows := strings.SplitN(screenStr, "\n", fromScreen+1)
		for i := 0; i < fromScreen && i < len(screenRows); i++ {
			lines = append(lines, screenRows[i])
		}
	}

	return strings.Join(lines, "\n")
}

// paneDataMsg is delivered when the pane event stream yielded a
// chunk. The Update handler forwards Data into the matching pane's
// VT emulator and re-arms the read.
type paneDataMsg struct {
	paneID string
	data   []byte
}

// paneStreamClosedMsg is delivered when the stream channel was
// closed. The Model records the state so renderRightPane can tell
// the user "preview disconnected" and stops arming further reads.
type paneStreamClosedMsg struct{}

// paneResolvedMsg is delivered when ActivePaneResolver returned. If
// err is non-nil, paneID is empty and the Model records the error
// for the affected window so the right pane can surface it.
type paneResolvedMsg struct {
	windowID string
	paneID   string
	err      error
}

// paneCaptureMsg is delivered when PaneCapturer returns. The Model
// feeds data into the pane's emulator (creating it if missing),
// flushes any bytes that arrived while the capture was in flight,
// and marks the pane as backfilled so a second capture is never
// issued. On err we mark captured anyway — one failed attempt is
// enough; subsequent live bytes paint the eventual state.
type paneCaptureMsg struct {
	paneID string
	data   []byte
	err    error
}

// keysSentMsg is delivered when KeySender returns. The Model uses
// err to decide whether the target pane is still live: a
// "no such pane" error tells us the session was closed between
// keystroke and forward, so we revert focus to the sidebar and
// let the next sessionsMsg poll prune the stale highlight.
// Other errors are absorbed silently; one dropped keystroke on a
// transient tmux hiccup is preferable to a sidebar full of noisy
// error lines.
type keysSentMsg struct {
	paneID string
	err    error
}

// windowClosedMsg is delivered when WindowCloser returns. The
// Model's handler reacts by dropping any cached per-pane state
// for the closed window and requesting an immediate sessionsMsg
// so the sidebar row disappears without waiting for the next
// poll tick. err is recorded on the Model's listErr surface only
// when non-nil AND not ErrPaneGone-equivalent (an already-gone
// window is the desired post-condition, not an error to shout
// about).
type windowClosedMsg struct {
	windowID string
	err      error
}

// windowResizedMsg is delivered when WindowResizer returns. The
// Model absorbs it silently: resize failures (window closed, tmux
// flaked) manifest as visual drift the user can recover from by
// switching sessions, which is preferable to a noisy toast on
// every viewport change. The field is kept for future telemetry
// (e.g. surfacing repeated failures as a sidebar hint).
type windowResizedMsg struct {
	windowID string
	err      error
}

// normalizeCaptureEOL converts bare LF (`\n`) row terminators into
// CRLF (`\r\n`) so a VT emulator interprets them as "move cursor to
// column 0 of the next row" rather than "cursor down, same column".
//
// Why this matters: `tmux capture-pane -p -e` emits one line per
// row of the visible grid, separated by bare `\n`. The emulator
// follows strict VT semantics where `\n` (0x0A) is LINE FEED —
// advance the row, keep the column. Without a leading `\r`
// (0x0D, CARRIAGE RETURN), every subsequent row starts at whatever
// column the previous row ended on, producing a visible staircase
// where each captured row is indented by the cumulative width of
// all rows above it. The effect in the TUI is exactly the
// "text wrapped far beyond the pane and bleeds into the next row"
// distortion the user reported, and it is fixed ONLY by a manual
// terminal resize because a resize triggers claude to emit a full
// redraw whose byte stream uses proper CSI cursor positioning (or
// CRLF) and overwrites the staircase.
//
// Live %output bytes from tmux do NOT need this normalization:
// tmux forwards the pane's PTY output verbatim, which already uses
// CRLF or cursor-positioning escapes. This helper is therefore
// scoped to the capture-pane payload only, invoked in
// handlePaneCapture.
//
// We deliberately scan for isolated `\n` rather than unconditionally
// inserting `\r` before every `\n`: preserving an existing `\r\n`
// pair (should tmux ever emit it) is correct, and the cost of a
// one-pass byte scan is trivial next to the cost of the emulator
// parsing the data itself.
func normalizeCaptureEOL(src []byte) []byte {
	needs := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' && (i == 0 || src[i-1] != '\r') {
			needs++
		}
	}
	if needs == 0 {
		return src
	}
	out := make([]byte, 0, len(src)+needs)
	for i := 0; i < len(src); i++ {
		b := src[i]
		if b == '\n' && (i == 0 || src[i-1] != '\r') {
			out = append(out, '\r', '\n')
			continue
		}
		out = append(out, b)
	}
	return out
}

// waitForPaneEvent returns a tea.Cmd that reads one PaneEvent from
// the stream and wraps it as a paneDataMsg. If the channel is closed
// we emit paneStreamClosedMsg, which stops the re-arm loop in
// Update. Returns nil when no stream is wired (empty-state tests).
func (m Model) waitForPaneEvent() tea.Cmd {
	ch := m.paneEvents
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return paneStreamClosedMsg{}
		}
		return paneDataMsg{paneID: ev.PaneID, data: ev.Data}
	}
}

// resolveActivePane returns a tea.Cmd that calls ActivePaneResolver
// with a bounded context and wraps the result in a paneResolvedMsg.
// Returns nil when no resolver is wired or when the window ID is
// empty (nothing to resolve).
func (m Model) resolveActivePane(windowID string) tea.Cmd {
	if m.paneResolver == nil || windowID == "" {
		return nil
	}
	resolver := m.paneResolver
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		paneID, err := resolver(ctx, windowID)
		return paneResolvedMsg{windowID: windowID, paneID: paneID, err: err}
	}
}

// captureActivePane returns a tea.Cmd that asks PaneCapturer for
// the current screen of paneID and wraps the result in a
// paneCaptureMsg. Returns nil when no capturer is wired or when
// the pane ID is empty — both cases are "no backfill possible";
// the emulator will boot on the first live chunk as before.
func (m Model) captureActivePane(paneID string) tea.Cmd {
	if m.paneCapturer == nil || paneID == "" {
		return nil
	}
	capturer := m.paneCapturer
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		data, err := capturer(ctx, paneID)
		return paneCaptureMsg{paneID: paneID, data: data, err: err}
	}
}

// sendKeysToPane returns a tea.Cmd that asks KeySender to forward
// data to paneID. Returns nil when no sender is wired, the pane
// ID is empty, or data is empty — all of which are "nothing to
// route" and should never produce a spurious tea.Msg in the
// runtime loop.
func (m Model) sendKeysToPane(paneID string, data []byte) tea.Cmd {
	if m.keySender == nil || paneID == "" || len(data) == 0 {
		return nil
	}
	sender := m.keySender
	// Copy so a later write to the Model's internal buffers cannot
	// race with the async subprocess; the tea.Cmd runs on a
	// separate goroutine and receives its own owned slice.
	buf := make([]byte, len(data))
	copy(buf, data)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		err := sender(ctx, paneID, buf)
		return keysSentMsg{paneID: paneID, err: err}
	}
}

// closeManagedWindow returns a tea.Cmd that asks WindowCloser to
// terminate windowID. Returns nil when no closer is wired or
// windowID is empty. The result is wrapped in a windowClosedMsg
// the Update loop consumes to refresh the sidebar snapshot.
func (m Model) closeManagedWindow(windowID string) tea.Cmd {
	if m.windowCloser == nil || windowID == "" {
		return nil
	}
	closer := m.windowCloser
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		err := closer(ctx, windowID)
		return windowClosedMsg{windowID: windowID, err: err}
	}
}

// resizeManagedWindow returns a tea.Cmd that asks WindowResizer to
// resize windowID to (width, height). Returns nil when no resizer
// is wired, the window ID is empty, or dimensions are non-positive.
// The caller is responsible for debouncing duplicate (wid, w, h)
// invocations; this helper is pure transport.
func (m Model) resizeManagedWindow(windowID string, width, height int) tea.Cmd {
	if m.windowResizer == nil || windowID == "" {
		return nil
	}
	if width < 1 || height < 1 {
		return nil
	}
	resizer := m.windowResizer
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		err := resizer(ctx, windowID, width, height)
		return windowResizedMsg{windowID: windowID, err: err}
	}
}

// forceResizeManagedWindow returns a tea.Cmd that asks WindowResizer
// to RESIZE twice in quick succession: first to (width, height+1),
// then back to (width, height). The temporary +1 row is the trigger
// — tmux no-ops a resize-window whose target dims match the pane's
// current dims, which means a same-size resize issued after a
// window close does NOT produce a SIGWINCH on the surviving
// pane's claude process. No SIGWINCH, no redraw; no redraw, no
// cursor-repositioning escapes on the wire; no cursor escapes,
// our emulator's cursor stays at the stale position left by the
// pre-close capture-pane backfill — and subsequent character
// echoes from claude (which rely on claude's own drawing code
// having already positioned the cursor to the input box) land
// wherever the stale cursor was, typically at the bottom of the
// grid.
//
// The wiggle is transient: the +1 row exists only between the two
// tmux round-trips (sub-millisecond on a local socket), and our
// emulator's grid is never resized during this dance — only
// tmux's view of the pane is. Claude observes a height change,
// redraws for (H+1), then observes another height change and
// redraws for H. The second redraw is the one our emulator
// consumes, with cursor-addressed writes that land where claude
// intends them.
//
// Errors from either call collapse into the returned
// windowResizedMsg. We do NOT surface the intermediate +1
// failure separately: if tmux rejects the (H+1) call, the
// second call (same height we would send anyway) still has a
// reasonable chance of succeeding, and a two-step error report
// would just double-count the same problem.
//
// Returns nil under the same degenerate conditions as
// resizeManagedWindow (no resizer, empty windowID, non-positive
// dims), which keeps the call sites that batch both helpers
// symmetric.
func (m Model) forceResizeManagedWindow(windowID string, width, height int) tea.Cmd {
	if m.windowResizer == nil || windowID == "" {
		return nil
	}
	if width < 1 || height < 1 {
		return nil
	}
	resizer := m.windowResizer
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		if err := resizer(ctx, windowID, width, height+1); err != nil {
			return windowResizedMsg{windowID: windowID, err: err}
		}
		err := resizer(ctx, windowID, width, height)
		return windowResizedMsg{windowID: windowID, err: err}
	}
}

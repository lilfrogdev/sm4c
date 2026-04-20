package tui

import (
	"context"

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

// paneTerminal pairs a VT emulator with a "has received any bytes
// yet" flag. The flag lets renderRightPaneBody distinguish "emulator
// exists but is still the post-boot blank screen" from "emulator has
// drawn something", so we can keep showing a "waiting for output"
// hint until claude actually emits bytes instead of flashing an
// empty grid.
type paneTerminal struct {
	emu     *vt.Emulator
	written bool
}

// newPaneTerminal constructs a fresh emulator at the given
// dimensions. Callers are expected to pass positive integers; width
// and height are clamped to a minimum of 1 on the emulator side, but
// we clamp defensively too to keep internal invariants obvious.
func newPaneTerminal(width, height int) *paneTerminal {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return &paneTerminal{emu: vt.NewEmulator(width, height)}
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

// render returns the emulator's current screen as an ANSI-encoded
// string: one line per row, each clipped to the emulator width,
// with SGR escapes embedded so colors and attributes survive the
// handoff to the outer terminal. The caller is responsible for
// trimming trailing empty lines if the surrounding layout prefers
// tight content.
func (p *paneTerminal) render() string {
	if p == nil || p.emu == nil {
		return ""
	}
	return p.emu.Render()
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

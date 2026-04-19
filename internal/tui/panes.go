package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// panes.go carries the M3b.1 data path: a PaneEvent value type, the
// two dependency-injection seams (PaneEventStream, ActivePaneResolver)
// the Model calls into, a per-pane ring buffer, and the message
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
// as they appeared on the pane's local terminal. The TUI treats them
// as opaque until VT emulation lands in M3b.2; for M3b.1 we strip
// them down to printable ASCII before rendering.
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

// paneBufferBytes caps the per-pane ring buffer. A few kilobytes is
// plenty for a raw-bytes preview (enough to cover one screen of
// output on any realistic terminal); M3b.2's VT emulator will have
// its own bounded grid and this raw buffer will go away.
const paneBufferBytes = 8 * 1024

// paneBuffer is a fixed-size ring that keeps the most recent N
// bytes produced by a pane. We deliberately discard older bytes
// silently — the right pane is a live preview, not a scrollback
// viewer.
type paneBuffer struct {
	data []byte
	size int
	head int
}

// newPaneBuffer allocates a fresh ring of the configured capacity.
// Kept internal so the Model can mint one per newly-seen pane ID on
// demand without exposing the ring type to callers.
func newPaneBuffer() *paneBuffer {
	return &paneBuffer{data: make([]byte, paneBufferBytes)}
}

// append appends p to the ring, discarding the oldest bytes when
// capacity is exceeded. Safe to call with an empty slice.
func (b *paneBuffer) append(p []byte) {
	if len(p) == 0 {
		return
	}
	cap := len(b.data)
	// If this chunk alone is larger than the ring, keep just the
	// tail: everything before it would be overwritten anyway.
	if len(p) >= cap {
		copy(b.data, p[len(p)-cap:])
		b.head = 0
		b.size = cap
		return
	}
	// Two-phase write: from head to end of the ring, then wrap.
	n := copy(b.data[b.head:], p)
	if n < len(p) {
		copy(b.data, p[n:])
	}
	b.head = (b.head + len(p)) % cap
	if b.size < cap {
		if b.size+len(p) > cap {
			b.size = cap
		} else {
			b.size += len(p)
		}
	}
}

// snapshot returns a fresh linear copy of the ring's current
// contents in oldest-to-newest order. We copy on every read rather
// than handing back an internal slice because the Model's render
// path retains the value across frames, and the ring keeps being
// written to by incoming paneDataMsg handlers.
func (b *paneBuffer) snapshot() []byte {
	if b.size == 0 {
		return nil
	}
	out := make([]byte, b.size)
	cap := len(b.data)
	// The oldest byte starts at (head - size + cap) % cap.
	start := (b.head - b.size + cap) % cap
	if start+b.size <= cap {
		copy(out, b.data[start:start+b.size])
		return out
	}
	first := cap - start
	copy(out, b.data[start:])
	copy(out[first:], b.data[:b.size-first])
	return out
}

// paneDataMsg is delivered when the pane event stream yielded a
// chunk. The Update handler forwards Data into the matching pane's
// ring buffer and re-arms the read.
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

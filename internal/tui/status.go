package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// status.go implements the per-session status indicator driven by Claude
// Code lifecycle hooks. Hook scripts write "<pane_id> <event_type>" lines
// to a named FIFO; the TUI reads them and maps events to display states.
//
// State machine per pane (hook-driven):
//
//	Any  ──(UserPromptSubmit)──> Working   (spinner animates)
//	Any  ──(Stop)──────────────> Done      (green ✓)
//	Any  ──(Notification)──────> Waiting   (yellow ?)
//	Any  ──(user keystroke)────> Idle      (clears Done/Waiting)
//
// Sessions that have never received a hook event show StatusQuiet (faint
// dot). Sessions whose hook state was cleared by a keystroke show StatusIdle
// (solid dot). Only StatusWorking animates; the ticker self-terminates when
// no pane is Working, so an idle TUI costs zero background CPU.
//
// Hook scripts live in ~/.claude/settings.json and must guard on
// $SM4C_HOOK_FIFO being set — this prevents cross-talk when claude runs
// outside an sm4c tmux session:
//
//	"UserPromptSubmit": [{
//	    "type": "command",
//	    "command": "[ -n \"$SM4C_HOOK_FIFO\" ] && [ -n \"$TMUX_PANE\" ] && printf '%s prompt_submit\\n' \"$TMUX_PANE\" >> \"$SM4C_HOOK_FIFO\" 2>/dev/null",
//	    "async": true
//	}],
//	"Stop": [{
//	    "type": "command",
//	    "command": "[ -n \"$SM4C_HOOK_FIFO\" ] && [ -n \"$TMUX_PANE\" ] && printf '%s stop\\n' \"$TMUX_PANE\" >> \"$SM4C_HOOK_FIFO\" 2>/dev/null",
//	    "async": true
//	}],
//	"Notification": [{
//	    "type": "command",
//	    "command": "[ -n \"$SM4C_HOOK_FIFO\" ] && [ -n \"$TMUX_PANE\" ] && printf '%s notification\\n' \"$TMUX_PANE\" >> \"$SM4C_HOOK_FIFO\" 2>/dev/null",
//	    "async": true
//	}]

// SessionStatus is the per-session sidebar indicator state.
type SessionStatus uint8

const (
	// StatusQuiet is the initial state: the session has never received
	// a hook event. Rendered as a faint middle dot.
	StatusQuiet SessionStatus = iota

	// StatusIdle means the session had hook activity but the user's
	// last keystroke cleared the previous hook state. Rendered as a
	// solid dot — "available, nothing pending".
	StatusIdle

	// StatusWorking means Claude received a UserPromptSubmit hook and
	// has not yet fired Stop or Notification. Rendered as an animated
	// braille spinner.
	StatusWorking

	// StatusDone means Claude fired a Stop hook — the response finished
	// and the prompt is ready. Rendered as a green ✓.
	StatusDone

	// StatusWaiting means Claude fired a Notification hook — it needs
	// the user's attention (permission prompt, clarifying question, etc.).
	// Rendered as a yellow ?.
	StatusWaiting
)

// notificationDebounce is the window after a Stop event during which a
// Notification (hookEventWaiting) is suppressed. Claude Code fires its
// desktop notification ~5 s after Stop for both task completions and
// genuine questions; 7 s catches the automatic completion notification
// while leaving the pathway open for future Claude Code versions that
// may fire sooner or with different semantics.
const notificationDebounce = 7 * time.Second

// paneStatus is the per-window hook state record.
type paneStatus struct {
	// hookState is the most recent hook event received for this window.
	// hookEventNone is the zero value and means "no hook has fired yet"
	// (or a keystroke cleared the previous state).
	hookState hookEvent

	// everHadHook distinguishes "no hooks ever" (StatusQuiet) from
	// "had hooks, now cleared by keystroke" (StatusIdle). Set the first
	// time any hook event arrives; never cleared.
	everHadHook bool

	// doneAt records when the most recent Stop hook was received. Used
	// by applyHookEvent to debounce the Notification hook: a Notification
	// that arrives within notificationDebounce of Done is the automatic
	// "I'm done" desktop ping, not a genuine "waiting for input" signal,
	// and is suppressed. Zero value means no Stop has been received yet.
	doneAt time.Time
}

// derivedStatus maps the hook state record to the user-facing SessionStatus.
func (ps paneStatus) derivedStatus() SessionStatus {
	switch ps.hookState {
	case hookEventWorking:
		return StatusWorking
	case hookEventDone:
		return StatusDone
	case hookEventWaiting:
		return StatusWaiting
	}
	if ps.everHadHook {
		return StatusIdle
	}
	return StatusQuiet
}

// spinnerFrames is the classic 10-frame braille spinner. Each codepoint is
// in the Braille Patterns block (U+2800–U+28FF) and occupies exactly one
// terminal cell, so the two-cell status column never jitters.
var spinnerFrames = []string{
	"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
}

// statusFrameInterval is how often statusFrameTickMsg fires while at least
// one pane is Working. 100ms at 10 frames gives a 1-second rotation.
const statusFrameInterval = 100 * time.Millisecond

// statusGlyph renders the two-column status cell for one session. The second
// column is always an ASCII space so every row's name starts at the same
// column. The animation frame is taken modulo len(spinnerFrames) so callers
// can pass a monotonically increasing counter without thinking about wrap.
//
// Styles for colored glyphs are created inline so they pick up the renderer
// set by lipgloss.SetDefaultRenderer in Run — package-level vars would
// capture the renderer at init time, before the TUI output is wired in.
func statusGlyph(status SessionStatus, frame int) string {
	switch status {
	case StatusWorking:
		idx := frame % len(spinnerFrames)
		if idx < 0 {
			idx += len(spinnerFrames)
		}
		return spinnerFrames[idx] + " "
	case StatusDone:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true).Render("✓") + " "
	case StatusWaiting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render("?") + " "
	case StatusIdle:
		return "· "
	case StatusQuiet:
		return hintStyle.Render("·") + " "
	}
	return "  "
}

// statusGlyphPlain is the unstyled two-column cell used when the parent
// render already applies a background + foreground (the highlighted row).
// Inner styled spans would fight the parent ANSI sequences; plain characters
// let the selection band's own colors dominate while still carrying the
// shape signal (✓ and ? are recognizable without color).
func statusGlyphPlain(status SessionStatus, frame int) string {
	switch status {
	case StatusWorking:
		idx := frame % len(spinnerFrames)
		if idx < 0 {
			idx += len(spinnerFrames)
		}
		return spinnerFrames[idx] + " "
	case StatusDone:
		return "✓ "
	case StatusWaiting:
		return "? "
	case StatusIdle:
		return "· "
	case StatusQuiet:
		return "· "
	}
	return "  "
}

// statusFrameTickMsg is delivered by the ticker while at least one pane is
// Working. It carries no payload; the handler advances Model.statusFrame.
type statusFrameTickMsg struct{}

// anyPaneWorking reports whether at least one tracked pane is currently in
// the Working state. This is the animation-tick gating predicate: we only
// keep the ticker running while a spinner needs animating.
func (m Model) anyPaneWorking() bool {
	for _, ps := range m.paneStatuses {
		if ps.derivedStatus() == StatusWorking {
			return true
		}
	}
	return false
}

// scheduleStatusTick returns a Bubble Tea command that delivers a
// statusFrameTickMsg after statusFrameInterval, gated on:
//
//   - at least one pane being Working (spinner needs animating), AND
//   - no tick already in flight (guarded by Model.statusTickArmed).
//
// Returning nil terminates the tick chain when nothing needs animating.
func (m Model) scheduleStatusTick() tea.Cmd {
	if m.statusTickArmed {
		return nil
	}
	if !m.anyPaneWorking() {
		return nil
	}
	return tea.Tick(statusFrameInterval, func(time.Time) tea.Msg {
		return statusFrameTickMsg{}
	})
}

package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// status.go implements M3d: per-session status detection and the
// animated sidebar glyph that exposes it.
//
// Status is derived entirely from the `%output` stream we already
// consume from tmux's control client — we deliberately do NOT
// enable tmux's monitor-bell / monitor-activity / monitor-silence
// options. Those flags are sticky on tmux's side (cleared only
// when a client "sees" the window) and our control-mode attach
// has no corresponding concept of "seen", so they are a poor fit
// for a reactive UI. Streaming the raw bytes gives us the full
// picture: activity is the presence of bytes, silence is their
// absence, and bells are the BEL byte (0x07) inside those bytes.
//
// State machine per pane, given a silence threshold T (configured
// via Config.MonitorSilence, default 3s):
//
//	Quiet      ──(first byte)──> Working
//	Working    ──(no bytes for T)──> Idle
//	Idle       ──(new bytes)──> Working
//	Any        ──(BEL seen)──> Attention
//	Attention  ──(keystroke to pane)──> derived-from-bytes
//
// Attention is sticky on purpose: a bell that disappears because
// the user happened to be looking at a different pane in the
// moment it rang would be worse than no bell at all. Only an
// explicit keystroke to the ringing pane clears it.
//
// Rendering lives in statusGlyph; the animation frame comes from
// Model.statusFrame, which is advanced by statusFrameTickMsg only
// while at least one pane is in the Working state (see
// scheduleStatusTick). When every pane is Quiet, Idle, or
// Attention, the ticker stops, so an idle TUI costs zero
// background work.

// SessionStatus is the per-session sidebar indicator state.
// Values are ordered by "user-facing urgency": Quiet (ignore) <
// Idle (available) < Working (active) < Attention (needs you).
// Tests rely on that order for a simple "most-urgent of these"
// aggregation.
type SessionStatus uint8

const (
	// StatusQuiet is the initial state: the pane exists but has
	// never emitted a byte. Rendered as a faint middle dot so
	// the row is visibly "there" without drawing the eye.
	StatusQuiet SessionStatus = iota

	// StatusIdle means the pane had output at some point, and
	// then stayed silent for at least the silence threshold.
	// In claude terms this is "the response finished, the
	// prompt is waiting for you".
	StatusIdle

	// StatusWorking means the pane emitted bytes more recently
	// than the silence threshold. Rendered as an animated
	// spinner so the user sees that it is actively moving.
	StatusWorking

	// StatusAttention means the pane emitted a BEL byte that
	// the user has not yet acknowledged. Claude rings the bell
	// on permission prompts and some error states; we keep the
	// flag sticky until the user sends a keystroke to the
	// owning pane (handled in the KeySender path).
	StatusAttention
)

// paneStatus is the per-pane record we maintain to derive a
// SessionStatus. It is intentionally small so the Model can carry
// one entry per pane without worrying about allocation churn;
// every field is populated by handlePaneData as bytes arrive.
type paneStatus struct {
	// lastOutputAt is the monotonic time of the most recent
	// byte we saw for this pane. Zero means "never seen any
	// output" (which is also what everHadOutput=false implies;
	// they travel together for clarity at call sites).
	lastOutputAt time.Time

	// everHadOutput gates the Quiet → Working transition. It
	// stays true once set, even after the pane goes silent
	// again, so a pane that went Working → Idle renders as
	// Idle rather than collapsing back to Quiet.
	everHadOutput bool

	// bell is set the first time we see 0x07 in this pane's
	// byte stream and cleared only when the user sends a
	// keystroke to this pane (the KeySender path calls
	// clearAttention). It is sticky on purpose — see the
	// package-level comment.
	bell bool
}

// derivedStatus folds a paneStatus record into the user-facing
// SessionStatus at the current moment. "Current" is passed as a
// parameter rather than read from time.Now() so tests can pin a
// specific instant without racing the test clock.
//
// Priority: Attention wins over anything else. Below that, we
// check output history; an empty history is Quiet. If we have
// history and the most recent byte is older than the silence
// threshold, the pane is Idle; otherwise it is Working. This
// ordering lets a pane with a stuck bell and no output still
// surface an Attention glyph — the fact that it has nothing to
// say does not mean the user has nothing to answer.
func (ps paneStatus) derivedStatus(now time.Time, silenceThreshold time.Duration) SessionStatus {
	if ps.bell {
		return StatusAttention
	}
	if !ps.everHadOutput {
		return StatusQuiet
	}
	if silenceThreshold <= 0 {
		// Operator disabled the silence FSM. Once a pane has
		// had any output it stays Working forever (until a
		// bell flips it to Attention). This is a deliberate
		// opt-out shape: `monitor_silence = "0s"` in TOML.
		return StatusWorking
	}
	if now.Sub(ps.lastOutputAt) >= silenceThreshold {
		return StatusIdle
	}
	return StatusWorking
}

// spinnerFrames is the classic 10-frame braille spinner used by
// ora, spin.js, oclif, and most modern CLI tooling. Each frame
// is a U+28xx Braille Patterns codepoint that lights a subset of
// the 2×4 dot matrix; stepped in order they read as a single
// dot rotating clockwise around the cell. Braille gives us a
// smoother-looking spinner than the 4-frame ASCII `|/-\` at the
// cost of requiring a UTF-8 terminal with a font that covers
// U+2800..U+28FF — which is every terminal sm4c targets on
// macOS and Linux in 2026 (Terminal.app, iTerm2, Alacritty,
// Kitty, WezTerm, Ghostty, GNOME Terminal, Konsole…). Fixed-
// width font coverage means every frame occupies exactly one
// terminal cell, so the two-cell status column never jitters.
//
// It is a []string rather than a string because the codepoints
// are three bytes each in UTF-8; byte-indexing would slice
// them apart and render garbage. String-slice indexing is
// O(1) per frame and the slice header is constant.
var spinnerFrames = []string{
	"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
}

// statusFrameInterval is how often statusFrameTickMsg fires
// while at least one pane is Working. 100ms at 10 frames is a
// 1s rotation, which matches the rotation cadence of the old
// 4-frame / 250ms ASCII spinner — identical "speed" feel, just
// a smoother glyph. Going faster (e.g. 80ms, the ora default)
// pushes the sidebar re-render rate to ~12/s, which starts to
// tax a sluggish SSH link; 100ms keeps that cost at 10/s while
// still reading as obviously animated.
const statusFrameInterval = 100 * time.Millisecond

// attentionStyle renders the Attention glyph in ANSI yellow.
// Yellow (color 3) resolves to amber/orange on almost every
// mainstream terminal theme (Iceberg, Solarized, Gruvbox,
// GitHub Dark, Apple defaults…) which is as close to "Cursor's
// orange attention dot" as we can get without painting a hex
// color the no-hex-colors rule forbids. Bright-yellow (color
// 11) was rejected because on light-background themes it tends
// to appear washed out and defeats the "this one wants you"
// signal.
var attentionStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("3")).
	Bold(true)

// statusGlyph renders the two-column status cell for one
// session, given the current status and the animation frame.
// The second column is always an ASCII space so every row's
// name starts at the same column — alignment matters more than
// squeezing a character back, and glyph + space is only 2
// cells wide anyway.
//
// The animation frame is taken modulo len(spinnerFrames) here,
// not at the call site, so callers that maintain a monotonically
// increasing counter do not have to think about overflow or
// wrap-around.
func statusGlyph(status SessionStatus, frame int) string {
	switch status {
	case StatusWorking:
		idx := frame % len(spinnerFrames)
		if idx < 0 {
			idx += len(spinnerFrames)
		}
		return spinnerFrames[idx] + " "
	case StatusAttention:
		return attentionStyle.Render("●") + " "
	case StatusIdle:
		return "● "
	case StatusQuiet:
		return hintStyle.Render("·") + " "
	}
	return "  "
}

// statusFrameTickMsg is delivered by the ticker command
// scheduleStatusTick fires while at least one pane is Working.
// It carries no payload: the handler advances Model.statusFrame
// by one and, if any pane is still Working after that advance,
// schedules the next tick. If none are Working, the ticker
// chain naturally terminates and the sidebar stops re-rendering.
type statusFrameTickMsg struct{}

// anyPaneWorking reports whether at least one tracked pane is
// currently in the Working state. This is the animation-tick
// gating predicate: we only keep the ticker running while it
// has a reason to advance the spinner.
//
// We take "now" as a parameter for the same reason derivedStatus
// does — tests can pin an instant to assert the gate flips at
// the exact boundary of silenceThreshold.
func (m Model) anyPaneWorking(now time.Time) bool {
	for _, ps := range m.paneStatuses {
		if ps.derivedStatus(now, m.silenceThreshold) == StatusWorking {
			return true
		}
	}
	return false
}

// scheduleStatusTick returns a Bubble Tea command that delivers
// a statusFrameTickMsg after statusFrameInterval, BUT only when:
//
//   - at least one pane is currently Working (so there is
//     actually a spinner that needs animating), AND
//   - we do not already have a tick in flight (guarded by
//     Model.statusTickArmed, toggled by the tick handler).
//
// Returning nil is the normal way to let the tick chain terminate
// when everything is Quiet/Idle/Attention: Bubble Tea has no "stop
// this timer" API, so the pattern is "fire, and re-arm if still
// needed on the handler". That pattern — and the need for
// statusTickArmed to prevent double-arming — is the same shape
// as the sessions poll chain (scheduleNextPoll), just with a
// different cadence and predicate.
func (m Model) scheduleStatusTick() tea.Cmd {
	if m.statusTickArmed {
		return nil
	}
	if !m.anyPaneWorking(time.Now()) {
		return nil
	}
	// We can't mutate m here because scheduleStatusTick is a
	// value-receiver method called on the post-handler snapshot
	// (e.g. next.scheduleStatusTick() in Update). The handler
	// that consumed the tick sets statusTickArmed=false; the
	// handler that produced fresh bytes calls scheduleStatusTick
	// from a Model copy where armed is already false. The
	// armed bookkeeping is therefore done by the caller
	// (Update) explicitly — see the paneDataMsg and
	// statusFrameTickMsg cases.
	return tea.Tick(statusFrameInterval, func(time.Time) tea.Msg {
		return statusFrameTickMsg{}
	})
}

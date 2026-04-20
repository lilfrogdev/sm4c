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
// via Config.MonitorSilence, default 3s) and an echo window E
// (keystrokeEchoWindow, fixed at 1500ms):
//
//	Quiet      ──(first byte, no recent keystroke)──> Working
//	Working    ──(no bytes for T)──> Idle
//	Idle       ──(new bytes, no recent keystroke)──> Working
//	Any        ──(BEL seen)──> Attention
//	Attention  ──(keystroke to pane)──> derived-from-bytes
//	Any        ──(user typed within E)──> Idle/Quiet (echo suppression)
//
// The "echo suppression" arc is why this package tracks
// lastKeystrokeAt as well as lastOutputAt. When the user types
// into a pane, claude's TUI redraws the prompt line on every
// keypress — each keystroke produces a burst of bytes in
// %output. If we counted those as "Working", the spinner would
// animate while the user types and freeze the moment they stop,
// which is exactly backwards of what the user wants the spinner
// to mean ("claude is doing work"). Suppressing Working for a
// short window after the most recent keystroke means the only
// bytes that can drive the spinner are ones that clearly aren't
// user-triggered echo: claude's own "thinking" spinner, its
// token stream, tool output, etc. The window is deliberately
// short (1.5s) so that claude's response starting within a
// second of Enter still lights the spinner almost immediately.
//
// Attention is sticky on purpose: a bell that disappears because
// the user happened to be looking at a different pane in the
// moment it rang would be worse than no bell at all. Only an
// explicit keystroke to the ringing pane clears it — which also
// happens to refresh lastKeystrokeAt, so the echo-suppression
// arc above subsumes "don't also spin while acknowledging a
// bell" for free.
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
// every field is populated by handlePaneData as bytes arrive, or
// by notePaneKeystroke as the user types.
type paneStatus struct {
	// lastOutputAt is the monotonic time of the most recent
	// byte we saw for this pane. Zero means "never seen any
	// output" (which is also what everHadOutput=false implies;
	// they travel together for clarity at call sites).
	lastOutputAt time.Time

	// lastKeystrokeAt is the monotonic time of the most recent
	// keystroke the user forwarded to this pane. Zero means
	// "user has never typed into this pane". It is read by
	// derivedStatus to suppress Working while the user is
	// actively typing — see keystrokeEchoWindow and the
	// package-level state-machine diagram.
	lastKeystrokeAt time.Time

	// everHadOutput gates the Quiet → Working transition. It
	// stays true once set, even after the pane goes silent
	// again, so a pane that went Working → Idle renders as
	// Idle rather than collapsing back to Quiet.
	everHadOutput bool

	// bell is set the first time we see 0x07 in this pane's
	// byte stream and cleared only when the user sends a
	// keystroke to this pane (the KeySender path calls
	// notePaneKeystroke). It is sticky on purpose — see the
	// package-level comment.
	bell bool
}

// keystrokeEchoWindow is how long after the most recent
// keystroke we assume incoming bytes are echo / redraw from the
// user's own typing rather than work the TUI should visualize
// as "claude is thinking".
//
// 400ms was chosen empirically: claude's per-keystroke redraw
// (the input line bouncing back) lands on the order of tens of
// milliseconds in local usage and rarely exceeds 200ms even
// over a slow SSH hop. 400ms buys enough headroom that a burst
// of three or four fast keystrokes all "chain" into one echo
// window, while still being short enough that claude's actual
// response streaming — which for simple prompts starts within
// ~500ms of Enter and for anything non-trivial runs for
// multiple seconds — spends the overwhelming majority of its
// lifetime outside the echo window and therefore animates the
// sidebar spinner.
//
// An earlier revision used 1500ms and left users reporting "the
// spinner never moves" because short claude responses to simple
// prompts finished entirely inside the suppression window. The
// right mental model is "echo is near-instant; claude is
// seconds-scale"; 400ms stays firmly in the gap.
//
// Fixed rather than configurable: this is a heuristic, not a
// policy, and exposing it as a knob would invite users to tune
// it in ways that hide real bugs.
const keystrokeEchoWindow = 400 * time.Millisecond

// derivedStatus folds a paneStatus record into the user-facing
// SessionStatus at the current moment. "Current" is passed as a
// parameter rather than read from time.Now() so tests can pin a
// specific instant without racing the test clock.
//
// Priority order (top wins):
//
//  1. bell  → Attention. Bell always wins; the user's active
//     attention is what clears it, so even if they are typing
//     into this very pane the glyph stays amber until the
//     clearing keystroke is processed (which happens on the
//     same Update cycle, so the visible flicker is zero).
//  2. never had output → Quiet.
//  3. user typed within keystrokeEchoWindow → Idle (echo
//     suppression, see package comment). We treat the pane as
//     Idle rather than Quiet because "user has typed here"
//     implies the pane is live and ready; Quiet is reserved
//     for panes that have genuinely never done anything.
//  4. silent longer than silenceThreshold → Idle.
//  5. otherwise → Working.
//
// Note that step 3 is why a pane that has received bytes from
// user-echo alone (never any work from claude) still leaves
// everHadOutput = true — the field is "has bytes ever arrived
// here", not "has claude ever worked here". We cannot
// distinguish the two without protocol knowledge we do not
// have, and mis-firing step 3 is preferable to missing step 5
// when the user genuinely wants to see the spinner.
func (ps paneStatus) derivedStatus(now time.Time, silenceThreshold time.Duration) SessionStatus {
	if ps.bell {
		return StatusAttention
	}
	if !ps.everHadOutput {
		return StatusQuiet
	}
	if !ps.lastKeystrokeAt.IsZero() && now.Sub(ps.lastKeystrokeAt) < keystrokeEchoWindow {
		return StatusIdle
	}
	if silenceThreshold <= 0 {
		// Operator disabled the silence FSM. Once a pane has
		// had any output it stays Working forever (until a
		// bell flips it to Attention or the echo-window gate
		// above fires). This is a deliberate opt-out shape:
		// `monitor_silence = "0s"` in TOML.
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

// attentionStyle renders the Attention glyph in ANSI red plus
// bold. Red (color 1) is the universal "needs you / error /
// warning" color across every mainstream terminal theme
// (Solarized, Gruvbox, Iceberg, GitHub Dark, Apple defaults,
// Nord, Dracula), which is the semantic we want.
//
// Hex colors are forbidden by the existing CI grep gate; color
// 1 is the best 0-15 match to the intent. Bright-red (9) looks
// slightly louder on dark themes but tends to wash out on light
// themes, whereas color 1 paints consistently across both.
//
// Important rendering note: the Attention glyph itself
// (attentionGlyph below) is intentionally a shape-distinct
// character, not a colored dot. Earlier iterations used `●`
// with a red foreground, which worked fine on unhighlighted
// rows but surfaced a "color around the dot changes, not the
// dot itself" complaint on the highlighted row. Reason: the
// highlighted row is painted with rowHighlightStyle
// (Reverse(true)), which — per ANSI SGR semantics — swaps
// foreground and background at render time. Applied on top of
// a `Foreground(red)` glyph, that swap pushes red into the
// background channel and leaves the glyph drawn in the default
// foreground. So the user saw "a red-backed cell with a
// default-colored dot inside", not "a red dot". Using a
// character whose shape alone screams "attention" means the
// signal survives the swap: even if red becomes background on
// the highlighted row, the glyph is still obviously different
// from the Idle/Quiet dots and the Working spinner. Color is
// now a reinforcement, not the sole carrier.
var attentionStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("1")).
	Bold(true)

// attentionGlyph is the character we render for a pane in the
// Attention state. `!` was chosen because:
//
//   - It is universally understood as "alert / warning"
//     regardless of writing system or terminal theme.
//   - It occupies exactly one cell in every terminal font so
//     the two-cell status column never jitters.
//   - Its vertical-stroke shape is unmistakably different from
//     the round dots (`·`, `●`) used for Quiet/Idle, the
//     braille spinner used for Working, and anything else the
//     sidebar is likely to render, which means the attention
//     signal survives even if the foreground color does not
//     (see attentionStyle's rendering note).
//   - ASCII means no font-coverage risk; every terminal sm4c
//     runs on renders it identically, full stop.
const attentionGlyph = "!"

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
		return attentionStyle.Render(attentionGlyph) + " "
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

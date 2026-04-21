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
// via Config.MonitorSilence, default 1.5s) and an echo window E
// (keystrokeEchoWindow, fixed at 400ms):
//
//	Quiet      ──(first byte, no recent keystroke)──> Working
//	Working    ──(no bytes for T AND bell unset)──> Idle
//	Working    ──(no bytes for T AND bell set)──> Attention
//	Idle       ──(new bytes, no recent keystroke)──> Working
//	Idle       ──(BEL seen)──> Attention
//	Attention  ──(new bytes, no recent keystroke)──> Working
//	Attention  ──(keystroke to pane)──> derived-from-bytes
//	Any        ──(user typed within E)──> Idle (echo suppression)
//
// Note the Working transitions: a bell fired DURING a token
// stream does not flip the glyph to Attention immediately. It
// sets paneStatus.bell=true, which is remembered, and the
// spinner keeps animating until the stream quiets down for T.
// Only THEN does the remembered bell surface as Attention. This
// is the fix for "the bang appears too soon" — claude rings the
// bell at several points inside a single response (tool-call
// boundaries, partial confirmations, completion), and surfacing
// each one immediately would rip the spinner away mid-stream
// and tell the user "done" while claude was still generating.
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
// The central insight this function encodes is that "claude is
// working" and "claude wants your attention" are NOT competing
// signals on the same axis — they are layered. Whether claude
// is working is answered by bytes flowing (vs. silent); whether
// claude wants you is answered by a bell, which is only
// meaningful AFTER it has stopped working. A bell fired in the
// middle of a token stream is a hint for "when this calms
// down, look here", not a hint for "drop the token stream and
// look now". Surfacing Attention while bytes are still flowing
// yanks the spinner away mid-response and makes the user think
// claude is done when it isn't — which is exactly the "the
// bang appears too soon" regression this ordering fixes.
//
// Priority order (top wins):
//
//  1. user typed within keystrokeEchoWindow → Idle. The user
//     is actively engaged with this pane; they are not waiting
//     on a signal from us. We also treat any pending bell as
//     implicitly acknowledged here (the user's hands are on
//     the pane), so the bang does not flash while they type
//     past a permission prompt.
//  2. never had output → Quiet (or Attention if somehow a
//     bell landed with no prior output, which would be a
//     pathological case but we handle it anyway).
//  3. actively emitting (now.Sub(lastOutputAt) < T) → Working.
//     This is where bell's priority flips: bell is remembered
//     in paneStatus but NOT surfaced while bytes are still
//     flowing. The spinner keeps going through bell events
//     until the stream actually quiets down.
//  4. silent AND bell → Attention. The pane has settled AND
//     claude asked for your attention at some point during
//     the run. This is the "your turn" signal the user cares
//     about.
//  5. silent, no bell → Idle. The pane has output history but
//     is currently quiet.
//
// The silence threshold is the same knob in both directions:
// it determines when Working yields to Idle/Attention. Shorter
// values make the "done" signal more responsive but risk
// flipping during a mid-response pause; longer values are more
// stable but add latency between "claude actually finished"
// and "user sees the bang / solid dot". Default 3s from
// Config.MonitorSilence.
//
// Edge case: silenceThreshold <= 0 disables silence-based
// transitions entirely — panes stay Working forever once they
// have emitted a byte. Bells in this mode are remembered but
// never surfaced as Attention, because "silent" never becomes
// true. This is the deliberate opt-out shape for
// `monitor_silence = "0s"` in TOML and is intentionally a
// degenerate config: operators who pick it have opted out of
// status transitions, including Attention.
func (ps paneStatus) derivedStatus(now time.Time, silenceThreshold time.Duration) SessionStatus {
	if !ps.lastKeystrokeAt.IsZero() && now.Sub(ps.lastKeystrokeAt) < keystrokeEchoWindow {
		return StatusIdle
	}
	if !ps.everHadOutput {
		if ps.bell {
			return StatusAttention
		}
		return StatusQuiet
	}
	if silenceThreshold <= 0 {
		return StatusWorking
	}
	silent := now.Sub(ps.lastOutputAt) >= silenceThreshold
	if !silent {
		return StatusWorking
	}
	if ps.bell {
		return StatusAttention
	}
	return StatusIdle
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

// attentionStyle is the identity style — attentionGlyph
// renders in the same weight and color as the surrounding
// row, with no bold / foreground override.
//
// The progression of choices here encodes a deliberate
// de-escalation of visual weight as we learned what the
// signal actually needs to convey:
//
//   - red `●`: loud, semantically "alarm". Problem: users
//     mapped it to "something broke" when the common case
//     is the opposite — claude finished successfully and
//     wants to hand control back.
//   - red `!`: slightly less loud but still read as
//     error/warning. Same semantic mismatch.
//   - bold `✓`: checkmark shape fixes the semantic (reads
//     as "done / your turn"), but bold made it stand out
//     more than the Working spinner or Idle dot, which
//     overstated the signal's urgency. Attention IS
//     more interesting than Idle — but it is not MORE
//     urgent; it is just a richer flavor of "done".
//   - regular `✓`: this version. Shape alone carries the
//     signal (checkmark is unmistakably different from the
//     round Idle/Quiet dots and the braille spinner), and
//     regular weight keeps every sidebar row visually
//     equal — no row shouts louder than any other.
//
// The shape-distinct approach means the signal survives
// rows painted with rowHighlightStyle's Reverse(true),
// which swaps foreground/background per ANSI SGR: a
// color-only signal would invert on highlighted rows, but
// a checkmark is still a checkmark regardless.
var attentionStyle = lipgloss.NewStyle()

// attentionGlyph is the character we render for a pane in the
// Attention state. `✓` (U+2713 CHECK MARK) was chosen because:
//
//   - It reads as "task completed / your turn" rather than
//     "error", which matches the actual semantic of the state
//     (claude finished a response and, at some point during
//     the run, rang the bell to say "hey, look at me"). The
//     previous `!` was technically accurate but read as
//     alarm/error in live usage — users associated it with
//     "something broke" when in the common case the session
//     is simply done and waiting.
//   - It occupies exactly one cell in every terminal font so
//     the two-cell status column never jitters. Unlike the
//     heavier `✔` (U+2714), which is width-ambiguous in some
//     fonts, the narrow `✓` renders as a single cell
//     consistently across the terminals sm4c targets
//     (Terminal.app, iTerm2, Alacritty, Kitty, WezTerm,
//     Ghostty, GNOME Terminal, Konsole).
//   - Its shape — two diagonal strokes meeting at a point —
//     is unmistakably different from the round dots (`·`,
//     `●`) used for Quiet/Idle and the braille spinner used
//     for Working, so the signal carries even when color
//     does not (see attentionStyle's rendering note about
//     Reverse).
const attentionGlyph = "✓"

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

// statusGlyphPlain is the unstyled two-column cell matching
// statusGlyph's character choice, but without lipgloss wrapping
// Quiet/Attention in hintStyle / attentionStyle. Used when the
// parent render (the session card highlight) already applies
// Background + Foreground — inner styled spans would fight the
// parent's ANSI sequences and leave the glyph at the terminal's
// default color on top of the selection bar, reintroducing the
// low-contrast failure mode statusGlyph exists to avoid in the
// non-highlighted path.
func statusGlyphPlain(status SessionStatus, frame int) string {
	switch status {
	case StatusWorking:
		idx := frame % len(spinnerFrames)
		if idx < 0 {
			idx += len(spinnerFrames)
		}
		return spinnerFrames[idx] + " "
	case StatusAttention:
		return attentionGlyph + " "
	case StatusIdle:
		return "● "
	case StatusQuiet:
		return "· "
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

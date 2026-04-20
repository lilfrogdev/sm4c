package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// status_test.go pins the M3d status FSM and the sidebar glyph
// renderer. The FSM is pure (paneStatus + now + threshold →
// SessionStatus), so the tests run without a Bubble Tea runtime,
// without a tmux server, and without a real pane.

// TestDerivedStatusTransitions walks every arrow in the
// state-diagram defined in status.go:
//
//	Quiet → Working   (first byte)
//	Working → Idle    (silence elapsed)
//	Idle → Working    (new byte)
//	Any → Attention   (bell seen)
//
// The threshold is kept small so the tests don't have to sleep,
// and "now" is threaded explicitly so there is no dependence on
// wall-clock timing.
func TestDerivedStatusTransitions(t *testing.T) {
	t.Parallel()
	const threshold = 500 * time.Millisecond
	start := time.Unix(1700000000, 0)

	cases := []struct {
		name  string
		ps    paneStatus
		now   time.Time
		want  SessionStatus
		why   string
		tThrs time.Duration
	}{
		{
			name:  "fresh pane is Quiet",
			ps:    paneStatus{},
			now:   start,
			tThrs: threshold,
			want:  StatusQuiet,
			why:   "no bytes ever seen",
		},
		{
			name:  "recent output is Working",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true},
			now:   start.Add(100 * time.Millisecond),
			tThrs: threshold,
			want:  StatusWorking,
			why:   "elapsed < threshold",
		},
		{
			name:  "output older than threshold is Idle",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true},
			now:   start.Add(threshold + time.Millisecond),
			tThrs: threshold,
			want:  StatusIdle,
			why:   "elapsed >= threshold",
		},
		{
			name:  "exactly-at-threshold is Idle",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true},
			now:   start.Add(threshold),
			tThrs: threshold,
			want:  StatusIdle,
			why:   "boundary: >= is the FSM rule",
		},
		{
			name:  "bell wins over Working",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true, bell: true},
			now:   start.Add(50 * time.Millisecond),
			tThrs: threshold,
			want:  StatusAttention,
			why:   "bell is the top-priority state",
		},
		{
			name:  "bell wins over Idle",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true, bell: true},
			now:   start.Add(threshold * 5),
			tThrs: threshold,
			want:  StatusAttention,
			why:   "bell is the top-priority state",
		},
		{
			name:  "bell wins over Quiet (nothing emitted yet, already ringing)",
			ps:    paneStatus{bell: true},
			now:   start,
			tThrs: threshold,
			want:  StatusAttention,
			why:   "bell priority is independent of history",
		},
		{
			name:  "zero threshold means never-Idle",
			ps:    paneStatus{lastOutputAt: start, everHadOutput: true},
			now:   start.Add(1 * time.Hour),
			tThrs: 0,
			want:  StatusWorking,
			why:   "0 threshold = opt-out of the Idle flip",
		},
		{
			name: "recent keystroke suppresses Working",
			ps: paneStatus{
				lastOutputAt:    start,
				lastKeystrokeAt: start,
				everHadOutput:   true,
			},
			now:   start.Add(100 * time.Millisecond),
			tThrs: threshold,
			want:  StatusIdle,
			why:   "bytes arriving within keystrokeEchoWindow of a keypress are treated as echo",
		},
		{
			name: "keystroke older than echo window releases Working",
			ps: paneStatus{
				lastOutputAt:    start.Add(keystrokeEchoWindow + 200*time.Millisecond),
				lastKeystrokeAt: start,
				everHadOutput:   true,
			},
			now:   start.Add(keystrokeEchoWindow + 250*time.Millisecond),
			tThrs: threshold,
			want:  StatusWorking,
			why:   "once keystrokeEchoWindow elapses, fresh bytes mean claude, not echo",
		},
		{
			name: "bell still wins over recent keystroke",
			ps: paneStatus{
				lastOutputAt:    start,
				lastKeystrokeAt: start,
				everHadOutput:   true,
				bell:            true,
			},
			now:   start.Add(100 * time.Millisecond),
			tThrs: threshold,
			want:  StatusAttention,
			why:   "bell has priority over every other FSM arrow, including echo suppression",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.ps.derivedStatus(tc.now, tc.tThrs)
			if got != tc.want {
				t.Fatalf("derivedStatus = %v; want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestHandlePaneDataRecordsActivity pins the "every byte chunk
// updates status" contract: the first chunk flips everHadOutput,
// the timestamp tracks the most recent chunk, and the owning
// window's status record is keyed by window ID (not pane ID),
// so statusForWindow picks it up via the correct map.
func TestHandlePaneDataRecordsActivity(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	// Seed a resolved pane so updatePaneStatus can map paneID →
	// windowID. Without this, bytes arriving before resolution
	// are silently dropped on the floor (by design).
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("hello")})
	ps, ok := m.paneStatuses["@1"]
	if !ok {
		t.Fatalf("paneStatuses did not record window @1")
	}
	if !ps.everHadOutput {
		t.Fatalf("everHadOutput = false after first byte chunk; want true")
	}
	if ps.bell {
		t.Fatalf("bell = true without BEL byte; want false")
	}
	if ps.lastOutputAt.IsZero() {
		t.Fatalf("lastOutputAt is zero after a byte chunk")
	}

	// Second chunk: the timestamp should advance.
	first := ps.lastOutputAt
	time.Sleep(2 * time.Millisecond)
	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("world")})
	if got := m.paneStatuses["@1"].lastOutputAt; !got.After(first) {
		t.Fatalf("lastOutputAt did not advance (first=%v second=%v)", first, got)
	}
}

// TestHandlePaneDataDetectsBEL pins the sticky-bell behavior: a
// single 0x07 anywhere in any chunk flips the flag, and it stays
// flipped across subsequent chunks until explicitly cleared.
// This is what makes the "attention" glyph survive while the
// user is looking at a different session.
func TestHandlePaneDataDetectsBEL(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("prefix\x07suffix")})
	if !m.paneStatuses["@1"].bell {
		t.Fatalf("BEL byte did not flip bell flag")
	}

	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("no bell here")})
	if !m.paneStatuses["@1"].bell {
		t.Fatalf("bell flag cleared by non-bell chunk; want sticky")
	}
}

// TestNotePaneKeystrokeClearsBell pins the "user typed, so
// acknowledge the bell" half of notePaneKeystroke's contract.
// We invoke it directly — the handleKeyInPaneFocus path also
// produces a tea.Cmd that would try to run a real KeySender;
// driving that end-to-end requires a context and a stub sender,
// and the interesting assertion (bell gets cleared) is the
// same either way.
func TestNotePaneKeystrokeClearsBell(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:  time.Now(),
		everHadOutput: true,
		bell:          true,
	}

	m.notePaneKeystroke("%1")
	got := m.paneStatuses["@1"]
	if got.bell {
		t.Fatalf("bell flag survived notePaneKeystroke")
	}
	if !got.everHadOutput {
		t.Fatalf("notePaneKeystroke erased everHadOutput; it should only touch bell + lastKeystrokeAt")
	}
}

// TestNotePaneKeystrokeStampsTimestamp pins the "opens the echo-
// suppression window" half of notePaneKeystroke's contract: the
// call must advance lastKeystrokeAt so derivedStatus can see it
// on the next render. This is the fix for the "spinner animates
// while I type and freezes when I stop" regression.
func TestNotePaneKeystrokeStampsTimestamp(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	before := time.Now()
	m.notePaneKeystroke("%1")
	after := time.Now()

	got := m.paneStatuses["@1"].lastKeystrokeAt
	if got.IsZero() {
		t.Fatalf("lastKeystrokeAt is zero after notePaneKeystroke")
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("lastKeystrokeAt = %v; want within [%v, %v]", got, before, after)
	}
}

// TestAnyPaneWorkingRespectsEchoSuppression pins the ticker-gate
// side of the echo-suppression fix: a pane that is only emitting
// echo bytes from user keystrokes must NOT keep the animation
// ticker alive. Before the fix, the ticker would run as long as
// bytes were flowing; now it only runs when bytes are flowing
// AND the user hasn't typed recently.
func TestAnyPaneWorkingRespectsEchoSuppression(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	now := time.Unix(1700000000, 0)

	// User typed and claude immediately echoed (typical for
	// every single keypress claude processes). Both timestamps
	// are at "now"; derivedStatus should suppress Working.
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:    now,
		lastKeystrokeAt: now,
		everHadOutput:   true,
	}
	if m.anyPaneWorking(now.Add(10 * time.Millisecond)) {
		t.Fatalf("anyPaneWorking is true during echo suppression window")
	}

	// Past the echo window, bytes are assumed to be claude
	// working rather than user echo.
	if !m.anyPaneWorking(now.Add(keystrokeEchoWindow + 100*time.Millisecond)) {
		// Note: by this point lastOutputAt is also older than
		// now, so we need to simulate fresh bytes too.
		m.paneStatuses["@1"] = paneStatus{
			lastOutputAt:    now.Add(keystrokeEchoWindow + 50*time.Millisecond),
			lastKeystrokeAt: now,
			everHadOutput:   true,
		}
		if !m.anyPaneWorking(now.Add(keystrokeEchoWindow + 100*time.Millisecond)) {
			t.Fatalf("anyPaneWorking stays false after keystrokeEchoWindow elapsed with fresh bytes")
		}
	}
}

// TestStatusGlyphShape pins the visual surface of each state.
// We match on substring because the Working glyph is an animated
// braille spinner frame (one of spinnerFrames), and because the
// Attention glyph is wrapped in an ANSI color escape — asserting
// the escape would couple the test to lipgloss internals.
//
// The Working-frame cases are derived from spinnerFrames rather
// than hardcoded so a future change to the spinner glyph set does
// not require a parallel test edit. The wrap assertion still
// pins the modulo contract: frame len == frame 0.
func TestStatusGlyphShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status SessionStatus
		frame  int
		want   string
		name   string
	}{
		{StatusQuiet, 0, "·", "quiet is a middle dot"},
		{StatusIdle, 0, "●", "idle is a solid dot"},
		{StatusAttention, 0, attentionGlyph, "attention is a shape-distinct glyph (survives reverse-video)"},
		{StatusWorking, 0, spinnerFrames[0], "working frame 0 is first braille glyph"},
		{StatusWorking, 1, spinnerFrames[1], "working frame 1 is second braille glyph"},
		{StatusWorking, 2, spinnerFrames[2], "working frame 2 is third braille glyph"},
		{StatusWorking, len(spinnerFrames), spinnerFrames[0], "working frame wraps at len(spinnerFrames)"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := statusGlyph(tc.status, tc.frame)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("statusGlyph(%v, %d) = %q; want substring %q", tc.status, tc.frame, got, tc.want)
			}
		})
	}
}

// TestSpinnerFramesAreBraille pins the "we chose braille, and
// every frame is one cell wide" contract. It guards against
// accidental edits that would reintroduce ASCII glyphs (which
// would regress visual smoothness) or mix in wider codepoints
// (which would jitter the sidebar column width).
func TestSpinnerFramesAreBraille(t *testing.T) {
	t.Parallel()
	if len(spinnerFrames) == 0 {
		t.Fatalf("spinnerFrames is empty")
	}
	for i, f := range spinnerFrames {
		runes := []rune(f)
		if len(runes) != 1 {
			t.Fatalf("spinnerFrames[%d] = %q has %d runes; want 1", i, f, len(runes))
		}
		r := runes[0]
		if r < 0x2800 || r > 0x28FF {
			t.Fatalf("spinnerFrames[%d] = %q (U+%04X) is not in the Braille Patterns block (U+2800..U+28FF)", i, f, r)
		}
	}
}

// TestStatusGlyphHandlesNegativeFrame pins the defensive modulo
// branch in statusGlyph. A negative counter should still produce
// a valid frame rather than panic or index out of bounds.
func TestStatusGlyphHandlesNegativeFrame(t *testing.T) {
	t.Parallel()
	got := statusGlyph(StatusWorking, -1)
	if got == "" {
		t.Fatalf("statusGlyph returned empty for negative frame")
	}
}

// TestAnyPaneWorkingGatesTicker pins that the animation ticker
// self-gates correctly: a Model with only Quiet/Idle panes does
// NOT schedule a tick, but a Model with a Working pane does.
// This is what makes an idle TUI cost zero background CPU.
func TestAnyPaneWorkingGatesTicker(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	const threshold = 500 * time.Millisecond

	t.Run("no Working → no tick", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{SilenceThreshold: threshold})
		m.paneStatuses["@1"] = paneStatus{} // Quiet
		m.paneStatuses["@2"] = paneStatus{
			lastOutputAt:  now,
			everHadOutput: true,
		}
		// @2 is Idle at now+2T (stale output), @1 is Quiet.
		// Ticker gate should be false — nothing to animate.
		if m.anyPaneWorking(now.Add(threshold * 2)) {
			t.Fatalf("anyPaneWorking reports true when all panes are Quiet/Idle")
		}
	})

	t.Run("one Working → tick", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{SilenceThreshold: threshold})
		m.paneStatuses["@1"] = paneStatus{
			lastOutputAt:  now,
			everHadOutput: true,
		}
		if !m.anyPaneWorking(now.Add(50 * time.Millisecond)) {
			t.Fatalf("anyPaneWorking reports false for a recently-active pane")
		}
	})

	t.Run("Attention alone does not animate", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{SilenceThreshold: threshold})
		m.paneStatuses["@1"] = paneStatus{bell: true}
		// Attention is a steady glyph (dot), not animated, so
		// we must not waste a ticker on it.
		if m.anyPaneWorking(now) {
			t.Fatalf("anyPaneWorking is true for Attention-only state")
		}
	})
}

// TestStatusFrameTickAdvancesFrame pins the tick handler's
// side effect: it increments statusFrame by one and re-arms
// itself only while at least one pane is Working.
func TestStatusFrameTickAdvancesFrame(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:  time.Now(),
		everHadOutput: true,
	}
	m.statusTickArmed = true
	prev := m.statusFrame

	updated, cmd := m.Update(statusFrameTickMsg{})
	next := updated.(Model)

	if next.statusFrame != prev+1 {
		t.Fatalf("statusFrame = %d after tick; want %d", next.statusFrame, prev+1)
	}
	if !next.statusTickArmed {
		t.Fatalf("next tick was not re-armed while a pane is Working")
	}
	if cmd == nil {
		t.Fatalf("tick handler returned no follow-up Cmd while Working")
	}
}

// TestStatusFrameTickStopsWhenIdle pins the "no more Working
// panes → stop ticking" contract: once activity stops long
// enough for the silence threshold to elapse, the ticker chain
// terminates naturally and statusTickArmed flips back to false.
func TestStatusFrameTickStopsWhenIdle(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 1 * time.Nanosecond})
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:  time.Now().Add(-1 * time.Second),
		everHadOutput: true,
	}
	m.statusTickArmed = true

	updated, cmd := m.Update(statusFrameTickMsg{})
	next := updated.(Model)

	if next.statusTickArmed {
		t.Fatalf("statusTickArmed still true after idle tick")
	}
	if cmd != nil {
		t.Fatalf("tick handler returned a follow-up Cmd for an all-Idle state")
	}
}

// TestPaneDataArmsStatusTicker pins that the first byte chunk
// arriving on a quiet pane kicks off the animation chain. This
// is the "Cold start → Working" transition as observed through
// the Update pipeline.
func TestPaneDataArmsStatusTicker(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	if m.statusTickArmed {
		t.Fatalf("statusTickArmed is true on a fresh Model")
	}

	updated, cmd := m.Update(paneDataMsg{paneID: "%1", data: []byte("hi")})
	next := updated.(Model)

	if !next.statusTickArmed {
		t.Fatalf("paneDataMsg on a fresh pane did not arm the ticker")
	}
	if cmd == nil {
		t.Fatalf("paneDataMsg returned no Cmd; expected at least a waitForPaneEvent")
	}
}

// TestSidebarRendersStatusGlyph pins the row shape: the leftmost
// two columns are the status glyph, the session name follows,
// and the window ID trails faintly. We drive the Model with a
// session and a status record and assert the glyph character
// appears in the rendered row.
func TestSidebarRendersStatusGlyph(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.sessions = []Session{{WindowID: "@1", Name: "dev", Active: true}}
	m.highlight = 0
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:  time.Now().Add(-1 * time.Hour),
		everHadOutput: true,
	}
	// With lastOutput an hour ago and threshold 500ms, this
	// pane is Idle → glyph is "●".

	row := m.renderSessionList()
	if !strings.Contains(row, "●") {
		t.Fatalf("rendered row missing Idle glyph: %q", row)
	}
	if !strings.Contains(row, "dev") {
		t.Fatalf("rendered row missing session name: %q", row)
	}
	// The window ID column was removed — sessions are identified
	// by name only. `sm4c ls` still surfaces IDs for debugging.
	if strings.Contains(row, "@1") {
		t.Fatalf("rendered row still contains window ID: %q", row)
	}
}

// TestSidebarRendersWorkingSpinner pins that a Working pane
// shows a spinner frame (rather than a dot). We don't assert
// which frame — statusFrame=0 maps to '|' by construction, but
// we read through statusGlyph so the test is robust to changes
// in the spinner character set.
func TestSidebarRendersWorkingSpinner(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{SilenceThreshold: 500 * time.Millisecond})
	m.sessions = []Session{{WindowID: "@1", Name: "dev"}}
	m.highlight = 0
	m.paneStatuses["@1"] = paneStatus{
		lastOutputAt:  time.Now(),
		everHadOutput: true,
	}
	row := m.renderSessionList()
	// Read frame 0 through spinnerFrames so this test
	// survives edits to the spinner character set — only the
	// "the sidebar renders frame 0 of the spinner for a
	// Working pane" contract is under test here.
	wantChar := spinnerFrames[0]
	if !strings.Contains(row, wantChar) {
		t.Fatalf("rendered row missing Working glyph %q: %q", wantChar, row)
	}
}

// TestSidebarStatusRoutesThroughStatusForWindow pins the full
// Model → sidebar glyph pipeline at the logical level
// (Model.statusForWindow → statusGlyph), one step below the
// actual string rendering. We use this path instead of asserting
// on color escapes in the rendered row because lipgloss's
// Foreground calls are a no-op when the test environment has no
// TERM profile — the dot still lands, but the ANSI bytes around
// it don't. The logical assertions below are the contract that
// matters: "a pane with bell=true reports Attention", not "the
// rendered byte sequence contains \x1b[33m".
func TestSidebarStatusRoutesThroughStatusForWindow(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	const threshold = 500 * time.Millisecond

	cases := []struct {
		name string
		ps   paneStatus
		at   time.Time
		want SessionStatus
	}{
		{"no record → Quiet", paneStatus{}, now, StatusQuiet},
		{"recent bytes → Working", paneStatus{lastOutputAt: now, everHadOutput: true}, now.Add(10 * time.Millisecond), StatusWorking},
		{"stale bytes → Idle", paneStatus{lastOutputAt: now, everHadOutput: true}, now.Add(threshold * 2), StatusIdle},
		{"bell → Attention", paneStatus{bell: true}, now, StatusAttention},
		{"bell over stale bytes → Attention", paneStatus{lastOutputAt: now, everHadOutput: true, bell: true}, now.Add(threshold * 2), StatusAttention},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewModel(Deps{SilenceThreshold: threshold})
			if tc.ps != (paneStatus{}) {
				m.paneStatuses["@1"] = tc.ps
			}
			got := m.statusForWindow("@1", tc.at)
			if got != tc.want {
				t.Fatalf("statusForWindow = %v; want %v", got, tc.want)
			}
		})
	}
}

// Touch tea.Msg so the import is kept if future refactors
// remove the last direct consumer; statusFrameTickMsg
// satisfies the interface and this guards the signal that
// "status_test.go builds against the bubbletea message
// contract" rather than accidentally drifting.
var _ tea.Msg = statusFrameTickMsg{}

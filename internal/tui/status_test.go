package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// status_test.go pins the hook-driven status FSM and sidebar glyph renderer.

// TestDerivedStatus walks the hook-based state machine.
func TestDerivedStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ps   paneStatus
		want SessionStatus
	}{
		{
			name: "fresh pane is Quiet",
			ps:   paneStatus{},
			want: StatusQuiet,
		},
		{
			name: "no hook state with prior activity is Idle",
			ps:   paneStatus{everHadHook: true},
			want: StatusIdle,
		},
		{
			name: "hookEventWorking is Working",
			ps:   paneStatus{hookState: hookEventWorking, everHadHook: true},
			want: StatusWorking,
		},
		{
			name: "hookEventDone is Done",
			ps:   paneStatus{hookState: hookEventDone, everHadHook: true},
			want: StatusDone,
		},
		{
			name: "hookEventWaiting is Waiting",
			ps:   paneStatus{hookState: hookEventWaiting, everHadHook: true},
			want: StatusWaiting,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.ps.derivedStatus()
			if got != tc.want {
				t.Fatalf("derivedStatus = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestNotePaneKeystrokeTransitions pins the keystroke state machine:
//   - Done → None (clears glyph to Idle; spinner comes from UserPromptSubmit hook)
//   - Waiting → None (same)
//   - None → None (idle pane stays idle)
//   - Working → Working (guard: don't clobber an already-armed spinner)
//
// everHadHook must be preserved in all cases.
func TestNotePaneKeystrokeTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		before    hookEvent
		wantAfter hookEvent
	}{
		{"done → none", hookEventDone, hookEventNone},
		{"waiting → none", hookEventWaiting, hookEventNone},
		{"none → none", hookEventNone, hookEventNone},
		{"working → working (no-op)", hookEventWorking, hookEventWorking},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewModel(Deps{})
			m.paneByWindow["@1"] = "%1"
			m.paneToWindow["%1"] = "@1"
			m.paneStatuses["@1"] = paneStatus{hookState: tc.before, everHadHook: true}

			m.notePaneKeystroke("%1")
			got := m.paneStatuses["@1"]
			if got.hookState != tc.wantAfter {
				t.Fatalf("hookState = %v after keystroke on %v; want %v", got.hookState, tc.before, tc.wantAfter)
			}
			if !got.everHadHook {
				t.Fatalf("notePaneKeystroke cleared everHadHook; should preserve it")
			}
		})
	}
}

// TestWaitingGatedAfterDone pins the waitingGated state machine:
//   - Notification after Done is blocked (gate is set by Stop).
//   - Notification after Done → Working is allowed (gate cleared by Working).
//   - Notification with no prior Done is always allowed.
func TestWaitingGatedAfterDone(t *testing.T) {
	t.Parallel()

	t.Run("after Done — blocked", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneByWindow["@1"] = "%1"
		m.paneToWindow["%1"] = "@1"
		m.paneStatuses["@1"] = paneStatus{
			hookState:    hookEventDone,
			everHadHook:  true,
			waitingGated: true, // set by the Stop event
		}

		updated, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWaiting})
		next := updated.(Model)
		got := next.paneStatuses["@1"].hookState
		if got != hookEventDone {
			t.Fatalf("hookState = %v after Notification while gated; want hookEventDone (blocked)", got)
		}
	})

	t.Run("after Done then Working — allowed", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneByWindow["@1"] = "%1"
		m.paneToWindow["%1"] = "@1"
		// gate was set by Done, then cleared by Working
		m.paneStatuses["@1"] = paneStatus{
			hookState:    hookEventWorking,
			everHadHook:  true,
			waitingGated: false,
		}

		updated, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWaiting})
		next := updated.(Model)
		got := next.paneStatuses["@1"].hookState
		if got != hookEventWaiting {
			t.Fatalf("hookState = %v after Notification with gate cleared; want hookEventWaiting (allowed)", got)
		}
	})

	t.Run("no prior Done — allowed", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneByWindow["@1"] = "%1"
		m.paneToWindow["%1"] = "@1"
		m.paneStatuses["@1"] = paneStatus{hookState: hookEventNone}

		updated, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWaiting})
		next := updated.(Model)
		got := next.paneStatuses["@1"].hookState
		if got != hookEventWaiting {
			t.Fatalf("hookState = %v after Notification with no prior Done; want hookEventWaiting", got)
		}
	})

	t.Run("Done sets gate, Working clears it", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneByWindow["@1"] = "%1"
		m.paneToWindow["%1"] = "@1"

		// Working → Done: gate should be set
		m2, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWorking})
		m3, _ := m2.(Model).Update(hookMsg{paneID: "%1", event: hookEventDone})
		if !m3.(Model).paneStatuses["@1"].waitingGated {
			t.Fatalf("waitingGated not set after Done")
		}

		// Working again: gate should be cleared
		m4, _ := m3.(Model).Update(hookMsg{paneID: "%1", event: hookEventWorking})
		if m4.(Model).paneStatuses["@1"].waitingGated {
			t.Fatalf("waitingGated not cleared after Working")
		}
	})
}

// TestDoneHookClearsStatusTickArmed pins that a Stop event resets
// statusTickArmed so the next UserPromptSubmit can arm the spinner tick
// without racing against an in-flight statusFrameTickMsg.
func TestDoneHookClearsStatusTickArmed(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"
	m.paneStatuses["@1"] = paneStatus{hookState: hookEventWorking, everHadHook: true}
	m.statusTickArmed = true // simulate in-flight tick from Working state

	updated, _ := m.Update(hookMsg{paneID: "%1", event: hookEventDone})
	next := updated.(Model)

	if next.statusTickArmed {
		t.Fatalf("statusTickArmed still true after Done; next UserPromptSubmit would fail to arm the spinner")
	}
}

// TestAnyPaneWorking pins the ticker-gate predicate for all hook states.
func TestAnyPaneWorking(t *testing.T) {
	t.Parallel()

	t.Run("Done does not animate", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneStatuses["@1"] = paneStatus{hookState: hookEventDone, everHadHook: true}
		if m.anyPaneWorking() {
			t.Fatalf("anyPaneWorking returned true for Done state")
		}
	})

	t.Run("Waiting does not animate", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneStatuses["@1"] = paneStatus{hookState: hookEventWaiting, everHadHook: true}
		if m.anyPaneWorking() {
			t.Fatalf("anyPaneWorking returned true for Waiting state")
		}
	})

	t.Run("Working animates", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneStatuses["@1"] = paneStatus{hookState: hookEventWorking, everHadHook: true}
		if !m.anyPaneWorking() {
			t.Fatalf("anyPaneWorking returned false for Working state")
		}
	})

	t.Run("Quiet does not animate", func(t *testing.T) {
		t.Parallel()
		m := NewModel(Deps{})
		m.paneStatuses["@1"] = paneStatus{}
		if m.anyPaneWorking() {
			t.Fatalf("anyPaneWorking returned true for Quiet state")
		}
	})
}

// TestStatusGlyphShape pins the visual surface of each state. We match on
// substring because some glyphs are wrapped in ANSI color escapes.
func TestStatusGlyphShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status SessionStatus
		frame  int
		want   string
		name   string
	}{
		{StatusQuiet, 0, "·", "quiet is a middle dot"},
		{StatusIdle, 0, "·", "idle is a middle dot"},
		{StatusWorking, 0, spinnerFrames[0], "working frame 0 is first braille glyph"},
		{StatusWorking, 1, spinnerFrames[1], "working frame 1 is second braille glyph"},
		{StatusWorking, len(spinnerFrames), spinnerFrames[0], "working frame wraps at len(spinnerFrames)"},
		{StatusDone, 0, "✓", "done is a check mark"},
		{StatusWaiting, 0, "?", "waiting is a question mark"},
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

// TestSpinnerFramesAreBraille pins the "braille only, one cell each" contract.
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

// TestStatusGlyphHandlesNegativeFrame pins the defensive modulo branch.
func TestStatusGlyphHandlesNegativeFrame(t *testing.T) {
	t.Parallel()
	got := statusGlyph(StatusWorking, -1)
	if got == "" {
		t.Fatalf("statusGlyph returned empty for negative frame")
	}
}

// TestStatusFrameTickAdvancesFrame pins the tick handler: increments
// statusFrame and re-arms while at least one pane is Working.
func TestStatusFrameTickAdvancesFrame(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneStatuses["@1"] = paneStatus{hookState: hookEventWorking, everHadHook: true}
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

// TestStatusFrameTickStopsWhenIdle pins that the ticker terminates when no
// pane is Working.
func TestStatusFrameTickStopsWhenIdle(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneStatuses["@1"] = paneStatus{hookState: hookEventDone, everHadHook: true}
	m.statusTickArmed = true

	updated, cmd := m.Update(statusFrameTickMsg{})
	next := updated.(Model)

	if next.statusTickArmed {
		t.Fatalf("statusTickArmed still true after non-Working tick")
	}
	if cmd != nil {
		t.Fatalf("tick handler returned a follow-up Cmd for non-Working state")
	}
}

// TestHookMsgUpdatesStatus pins that a hookMsg flowing through Update
// transitions the pane to the expected SessionStatus.
func TestHookMsgUpdatesStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		event hookEvent
		want  SessionStatus
	}{
		{"prompt_submit → Working", hookEventWorking, StatusWorking},
		{"stop → Done", hookEventDone, StatusDone},
		{"notification → Waiting", hookEventWaiting, StatusWaiting},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewModel(Deps{})
			m.paneByWindow["@1"] = "%1"
			m.paneToWindow["%1"] = "@1"

			updated, _ := m.Update(hookMsg{paneID: "%1", event: tc.event})
			next := updated.(Model)
			got := next.statusForWindow("@1")
			if got != tc.want {
				t.Fatalf("statusForWindow = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestHookMsgArmsStatusTicker pins that a Working hook starts the animation.
func TestHookMsgArmsStatusTicker(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	updated, cmd := m.Update(hookMsg{paneID: "%1", event: hookEventWorking})
	next := updated.(Model)

	if !next.statusTickArmed {
		t.Fatalf("Working hook did not arm the status ticker")
	}
	if cmd == nil {
		t.Fatalf("Working hook returned no Cmd")
	}
}

// TestHookMsgNonWorkingDoesNotArmTicker pins that Done/Waiting hooks do not
// start the animation ticker (they are steady glyphs, not animated).
func TestHookMsgNonWorkingDoesNotArmTicker(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	updated, _ := m.Update(hookMsg{paneID: "%1", event: hookEventDone})
	next := updated.(Model)

	if next.statusTickArmed {
		t.Fatalf("Done hook armed the status ticker; only Working should animate")
	}
}

// TestSidebarRendersStatusGlyph pins that the sidebar shows · for Idle.
func TestSidebarRendersStatusGlyph(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.sessions = []Session{{WindowID: "@1", Name: "dev", Active: true}}
	m.highlight = 0
	m.paneStatuses["@1"] = paneStatus{everHadHook: true} // Idle

	row := m.renderSessionList()
	if !strings.Contains(row, "·") {
		t.Fatalf("rendered row missing Idle glyph: %q", row)
	}
	if !strings.Contains(row, "dev") {
		t.Fatalf("rendered row missing session name: %q", row)
	}
	if strings.Contains(row, "@1") {
		t.Fatalf("rendered row still contains window ID: %q", row)
	}
}

// TestSidebarRendersWorkingSpinner pins that a Working pane shows a spinner.
func TestSidebarRendersWorkingSpinner(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.sessions = []Session{{WindowID: "@1", Name: "dev"}}
	m.highlight = 0
	m.paneStatuses["@1"] = paneStatus{hookState: hookEventWorking, everHadHook: true}
	row := m.renderSessionList()
	wantChar := spinnerFrames[0]
	if !strings.Contains(row, wantChar) {
		t.Fatalf("rendered row missing Working glyph %q: %q", wantChar, row)
	}
}

// TestSpinnerRestartsAfterDone pins the multi-cycle spinner case:
// Working → Done → Working must re-arm the spinner, not leave it dead.
// This covers the "spinner stops after first answer" regression.
func TestSpinnerRestartsAfterDone(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	// Cycle 1: Working → Done
	m2, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWorking})
	if !m2.(Model).statusTickArmed {
		t.Fatalf("cycle 1: Working did not arm ticker")
	}
	m3, _ := m2.(Model).Update(hookMsg{paneID: "%1", event: hookEventDone})
	if m3.(Model).statusTickArmed {
		t.Fatalf("cycle 1: Done did not clear ticker")
	}
	if m3.(Model).statusForWindow("@1") != StatusDone {
		t.Fatalf("cycle 1: expected StatusDone after Stop")
	}

	// Cycle 2: Working again — spinner must re-arm
	m4, cmd := m3.(Model).Update(hookMsg{paneID: "%1", event: hookEventWorking})
	if !m4.(Model).statusTickArmed {
		t.Fatalf("cycle 2: Working did not re-arm ticker after prior Done")
	}
	if cmd == nil {
		t.Fatalf("cycle 2: Working returned no Cmd")
	}
	if m4.(Model).statusForWindow("@1") != StatusWorking {
		t.Fatalf("cycle 2: expected StatusWorking, got %v", m4.(Model).statusForWindow("@1"))
	}
}

// TestSpinnerRestartsAfterDoneAndKeystroke pins the full observed failure
// sequence: Working → Done → user keystroke (clears glyph) → Working.
// The spinner must still re-arm after the keystroke clears Done to Idle.
func TestSpinnerRestartsAfterDoneAndKeystroke(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%1"
	m.paneToWindow["%1"] = "@1"

	// Working → Done
	m2, _ := m.Update(hookMsg{paneID: "%1", event: hookEventWorking})
	m3, _ := m2.(Model).Update(hookMsg{paneID: "%1", event: hookEventDone})

	// Keystroke clears Done → Idle (simulated via notePaneKeystroke)
	m3.(Model).notePaneKeystroke("%1")
	if m3.(Model).paneStatuses["@1"].hookState != hookEventNone {
		t.Fatalf("keystroke did not clear Done to None")
	}
	if m3.(Model).statusTickArmed {
		t.Fatalf("statusTickArmed should be false after Done")
	}

	// Working again — spinner must re-arm
	m4, cmd := m3.(Model).Update(hookMsg{paneID: "%1", event: hookEventWorking})
	if !m4.(Model).statusTickArmed {
		t.Fatalf("Working did not re-arm ticker after Done+keystroke")
	}
	if cmd == nil {
		t.Fatalf("Working returned no Cmd after Done+keystroke")
	}
}

// Guard the bubbletea import.
var _ tea.Msg = statusFrameTickMsg{}

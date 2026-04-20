package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// close_test.go covers the M3c close-session flow. The entry point
// is the `x` binding in sidebar focus: it arms pendingCloseWindow
// but does NOT call through to tmux until the user confirms with
// `y`. Every test drives the state machine with synthetic key
// messages and a stub WindowCloser; no tmux, no runtime.

// stubCloser records every windowID it is asked to close and
// returns the same error for every call. A nil `err` models the
// common happy path; tests that care about error propagation
// construct their own closer inline.
type stubCloser struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (s *stubCloser) closer() WindowCloser {
	return func(_ context.Context, windowID string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, windowID)
		return s.err
	}
}

func (s *stubCloser) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// modelWithClose returns a Model that has absorbed a single
// sessionsMsg so the sidebar is ready to render and the close
// flow has a real highlighted row to operate on.
func modelWithClose(t *testing.T, closer WindowCloser, sessions []Session) Model {
	t.Helper()
	m := NewModel(Deps{
		Lister:       stubLister(sessions, nil),
		WindowCloser: closer,
	})
	m = m.handleSessions(sessionsMsg{sessions: sessions})
	return m
}

func TestCloseKeyArmsButDoesNotCloseUntilConfirmed(t *testing.T) {
	t.Parallel()
	// Pressing `x` on a highlighted row arms pendingCloseWindow
	// and MUST NOT issue a WindowCloser call — the confirmation
	// keystroke is the commit point. This is the whole point of
	// the two-step UX: one fat-finger `x` never loses data.
	sc := &stubCloser{}
	m := modelWithClose(t, sc.closer(), []Session{
		{WindowID: "@1", Name: "one"},
		{WindowID: "@2", Name: "two"},
	})
	m, cmd := applyKey(t, m, "x")
	if m.pendingCloseWindow != "@1" {
		t.Fatalf("pendingCloseWindow = %q, want %q", m.pendingCloseWindow, "@1")
	}
	if cmd != nil {
		t.Fatalf("expected no cmd on arm; got %T", cmd())
	}
	if got := sc.recorded(); len(got) != 0 {
		t.Fatalf("closer called before confirmation: %v", got)
	}
}

func TestCloseConfirmCallsWindowCloser(t *testing.T) {
	t.Parallel()
	// After `x` arms a close, `y` commits it: the Model emits a
	// tea.Cmd that, when executed, invokes WindowCloser with the
	// armed window ID. pendingCloseWindow is cleared before the
	// cmd fires so a duplicate keystroke cannot re-commit.
	sc := &stubCloser{}
	m := modelWithClose(t, sc.closer(), []Session{
		{WindowID: "@7", Name: "target"},
	})
	m, _ = applyKey(t, m, "x")
	m, cmd := applyKey(t, m, "y")
	if m.pendingCloseWindow != "" {
		t.Fatalf("pendingCloseWindow = %q; want cleared after confirm", m.pendingCloseWindow)
	}
	if cmd == nil {
		t.Fatal("expected cmd after confirm; got nil")
	}
	// Fire the cmd to exercise the closer.
	msg := cmd()
	cmsg, ok := msg.(windowClosedMsg)
	if !ok {
		t.Fatalf("cmd returned %T; want windowClosedMsg", msg)
	}
	if cmsg.windowID != "@7" || cmsg.err != nil {
		t.Fatalf("unexpected close msg: %+v", cmsg)
	}
	if got := sc.recorded(); len(got) != 1 || got[0] != "@7" {
		t.Fatalf("closer calls = %v; want [@7]", got)
	}
}

func TestCloseCancelsOnNonConfirmKey(t *testing.T) {
	t.Parallel()
	// Any non-`y` key while pendingCloseWindow is armed cancels
	// the close and MUST NOT produce unrelated side effects
	// (e.g. `n` after a cancel should not spawn a new session,
	// `q` after a cancel should not quit). The cancel is a
	// clean one-step rollback.
	for _, k := range []string{"n", "q", "j", "esc"} {
		k := k
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			sc := &stubCloser{}
			m := modelWithClose(t, sc.closer(), []Session{
				{WindowID: "@1", Name: "one"},
			})
			m, _ = applyKey(t, m, "x")
			if m.pendingCloseWindow == "" {
				t.Fatal("arm did not take; test fixture broken")
			}
			m, cmd := applyKey(t, m, k)
			if m.pendingCloseWindow != "" {
				t.Fatalf("pendingCloseWindow not cleared after %q; still %q", k, m.pendingCloseWindow)
			}
			if m.Action() != ActionNone {
				t.Fatalf("%q after cancel produced Action=%v; want ActionNone", k, m.Action())
			}
			if cmd != nil {
				if msg := cmd(); msg != nil {
					t.Fatalf("%q after cancel produced cmd msg %T; want no side effect", k, msg)
				}
			}
			if got := sc.recorded(); len(got) != 0 {
				t.Fatalf("%q produced close calls: %v", k, got)
			}
		})
	}
}

func TestCloseIsNoOpWithoutCloser(t *testing.T) {
	t.Parallel()
	// A Model with no WindowCloser wired must treat `x` as a
	// no-op. This is the safe-degrade shape: environments where
	// we cannot mutate the tmux socket (tests, read-only) keep
	// the rest of the TUI working; only the close binding goes
	// quiet.
	m := NewModel(Deps{
		Lister: stubLister([]Session{{WindowID: "@1", Name: "a"}}, nil),
	})
	m = m.handleSessions(sessionsMsg{sessions: []Session{{WindowID: "@1", Name: "a"}}})
	m, _ = applyKey(t, m, "x")
	if m.pendingCloseWindow != "" {
		t.Fatalf("pendingCloseWindow = %q; want empty (no closer wired)", m.pendingCloseWindow)
	}
}

func TestCloseIsNoOpOnEmptySidebar(t *testing.T) {
	t.Parallel()
	// `x` on an empty sidebar has no target to arm. The key must
	// be a no-op (no pendingCloseWindow, no close call) rather
	// than crashing on the highlight bounds check.
	sc := &stubCloser{}
	m := NewModel(Deps{WindowCloser: sc.closer()})
	m, _ = applyKey(t, m, "x")
	if m.pendingCloseWindow != "" {
		t.Fatalf("pendingCloseWindow = %q on empty sidebar; want empty", m.pendingCloseWindow)
	}
}

func TestCloseConfirmPromptIncludesSessionName(t *testing.T) {
	t.Parallel()
	// The confirmation prompt rendered in place of the key bar
	// MUST include the highlighted session's display name so the
	// user can verify the target before pressing `y`. We render
	// the sidebar column directly rather than the full split
	// layout so the test does not need a WindowSizeMsg.
	sc := &stubCloser{}
	m := modelWithClose(t, sc.closer(), []Session{
		{WindowID: "@9", Name: "critical-sesh"},
	})
	m, _ = applyKey(t, m, "x")
	view := m.renderSidebarColumn()
	if !strings.Contains(view, "close critical-sesh?") {
		t.Fatalf("confirm prompt missing or wrong; view=\n%s", view)
	}
	if !strings.Contains(view, " y ") {
		t.Fatalf("confirm prompt did not advertise `y` key; view=\n%s", view)
	}
}

func TestWindowClosedMsgRefreshesSessions(t *testing.T) {
	t.Parallel()
	// The happy-path response to a windowClosedMsg is to fire an
	// immediate session refresh (fetchSessions cmd). Without the
	// refresh, the closed row would linger on the sidebar until
	// the next poll tick — which defeats the "closing feels
	// instant" UX goal.
	fetched := false
	lister := SessionLister(func(context.Context) ([]Session, error) {
		fetched = true
		return nil, nil
	})
	m := NewModel(Deps{Lister: lister})
	m.paneByWindow["@1"] = "%10"
	m.paneTerminals["%10"] = newPaneTerminal(40, 10)
	_, cmd := m.Update(windowClosedMsg{windowID: "@1"})
	if cmd == nil {
		t.Fatal("expected fetchSessions cmd after close; got nil")
	}
	// Run the cmd; it should dispatch the lister.
	_ = cmd()
	if !fetched {
		t.Fatal("windowClosedMsg did not trigger fetchSessions")
	}
}

func TestWindowClosedMsgDropsCachedPaneState(t *testing.T) {
	t.Parallel()
	// After a successful close the per-pane caches for that
	// window must be pruned so the right pane does not keep
	// rendering a ghost emulator until the next sessionsMsg
	// prunes it. We poke the caches directly (rather than
	// building them organically via messages) to keep the test
	// focused on handleWindowClosed's pruning contract.
	m := NewModel(Deps{})
	m.paneByWindow["@3"] = "%30"
	m.paneErrByWindow["@3"] = errors.New("stale")
	m.paneTerminals["%30"] = newPaneTerminal(40, 10)
	m.paneCaptured["%30"] = true
	m.paneCapturing["%30"] = true
	m.panePending["%30"] = []byte("buffered")
	m.sizedFor["@3"] = [2]int{40, 10}
	m.skipCaptureWindow = "@3"
	next, _ := m.Update(windowClosedMsg{windowID: "@3"})
	m = next.(Model)
	if _, ok := m.paneByWindow["@3"]; ok {
		t.Fatalf("paneByWindow[@3] not pruned")
	}
	if _, ok := m.paneErrByWindow["@3"]; ok {
		t.Fatalf("paneErrByWindow[@3] not pruned")
	}
	if _, ok := m.paneTerminals["%30"]; ok {
		t.Fatalf("paneTerminals[%%30] not pruned")
	}
	if _, ok := m.sizedFor["@3"]; ok {
		t.Fatalf("sizedFor[@3] not pruned")
	}
	if m.skipCaptureWindow != "" {
		t.Fatalf("skipCaptureWindow = %q; want cleared", m.skipCaptureWindow)
	}
}

func TestWindowClosedMsgInvalidatesSurvivorPanes(t *testing.T) {
	t.Parallel()
	// When tmux kills the active window, the control client is
	// switched to a surviving window; that switch can dribble a
	// partial redraw into the %output stream that mixes with the
	// existing emulator contents and leaves the preview looking
	// "mostly right but with stale rows". handleWindowClosed
	// forces a re-capture of every surviving pane (by pruning
	// paneByWindow, paneTerminals, and the capture flags) so the
	// next resolveHighlightedPaneIfNeeded tick redraws them from
	// tmux's authoritative grid. This test pins that contract at
	// the Model level — the resolveHighlightedPaneIfNeeded call
	// chain is covered elsewhere.
	m := NewModel(Deps{})
	// Closed window + pane.
	m.paneByWindow["@1"] = "%10"
	m.paneTerminals["%10"] = newPaneTerminal(40, 10)
	m.paneCaptured["%10"] = true
	// Surviving window + pane with pre-close emulator content.
	m.paneByWindow["@2"] = "%20"
	m.paneTerminals["%20"] = newPaneTerminal(40, 10)
	m.paneCaptured["%20"] = true
	m.panePending["%20"] = []byte("mid-flight")
	m.resolvedWindowID = "@1"

	next, _ := m.Update(windowClosedMsg{windowID: "@1"})
	m = next.(Model)

	if _, ok := m.paneByWindow["@2"]; ok {
		t.Fatalf("survivor paneByWindow[@2] not invalidated; map=%v", m.paneByWindow)
	}
	if _, ok := m.paneTerminals["%20"]; ok {
		t.Fatalf("survivor paneTerminals[%%20] not cleared")
	}
	if m.paneCaptured["%20"] {
		t.Fatalf("survivor paneCaptured[%%20] still true; want reset for re-capture")
	}
	if _, ok := m.panePending["%20"]; ok {
		t.Fatalf("survivor panePending[%%20] not cleared")
	}
	if m.resolvedWindowID != "" {
		t.Fatalf("resolvedWindowID = %q; want cleared so the next sessionsMsg re-resolves", m.resolvedWindowID)
	}
}

// TestWindowClosedMsgClearsSurvivorSizedFor pins the first half
// of the "typing lands at the bottom of the survivor" fix:
// sizedFor is fully cleared on close so the next
// resizeHighlightedWindow pass cannot debounce itself into a
// no-op. The second half — forcing the resize to be a wiggle
// shape rather than a single same-size call — is pinned by
// TestWindowClosedMsgArmsForceResize below.
//
// Symptom the full fix guards against: after closing a
// session, the surviving session's preview looked correct,
// but typed characters landed at the very bottom of the pane
// instead of in claude's input box. Root cause: the emulator
// inherited a stale cursor (end-of-capture row) from the
// pre-close capture-pane backfill, AND the post-close resize
// was a tmux no-op because the surviving pane's current dims
// already matched our paneViewW/paneViewH. No SIGWINCH
// reached claude, so claude never redrew, so no cursor-
// addressed writes re-positioned the emulator's cursor, so
// echoes landed wherever the stale cursor was parked.
func TestWindowClosedMsgClearsSurvivorSizedFor(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%10"
	m.paneByWindow["@2"] = "%20"
	m.sizedFor["@1"] = [2]int{120, 30}
	m.sizedFor["@2"] = [2]int{120, 30}

	next, _ := m.Update(windowClosedMsg{windowID: "@1"})
	m = next.(Model)

	if len(m.sizedFor) != 0 {
		t.Fatalf("sizedFor not fully cleared after close; got %v", m.sizedFor)
	}
}

// TestWindowClosedMsgArmsForceResize pins the second half of
// the "typing lands at the bottom" fix: handleWindowClosed
// arms forceResizePending so the next resizeHighlightedWindow
// call fires a wiggle (W, H+1)→(W, H) through
// forceResizeManagedWindow rather than a single same-size
// resize-window that tmux would no-op. See the
// forceResizeManagedWindow docstring for why the wiggle is
// the only shape that guarantees SIGWINCH on a pane whose
// current dims already match the target.
func TestWindowClosedMsgArmsForceResize(t *testing.T) {
	t.Parallel()
	m := NewModel(Deps{})
	m.paneByWindow["@1"] = "%10"
	m.paneByWindow["@2"] = "%20"
	if m.forceResizePending {
		t.Fatal("forceResizePending set before any close; baseline broken")
	}
	next, _ := m.Update(windowClosedMsg{windowID: "@1"})
	m = next.(Model)
	if !m.forceResizePending {
		t.Fatal("handleWindowClosed did not arm forceResizePending")
	}
}

// TestResizeHighlightedWindowConsumesForceResizeFlag pins the
// "consume the flag on first use" invariant. Without this,
// a subsequent j/k navigation would also pay the wiggle cost
// on top of its own resize, which is wasteful (extra tmux
// traffic) and would produce a visible flicker on every nav
// after a close. We cannot easily differentiate wiggle vs
// single-resize shape from tea.Cmd opacity, so we pin the
// observable proxy instead: the flag must flip to false the
// moment a resize is emitted.
func TestResizeHighlightedWindowConsumesForceResizeFlag(t *testing.T) {
	t.Parallel()
	rz := &resizerStub{}
	m := NewModel(Deps{WindowResizer: rz.resizer()})
	m = m.handleSessions(sessionsMsg{sessions: []Session{
		{WindowID: "@1", Name: "a"},
	}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)
	// Arm the flag directly — the handleWindowClosed path is
	// already covered by TestWindowClosedMsgArmsForceResize.
	m.forceResizePending = true
	if cmd := m.resizeHighlightedWindow(); cmd == nil {
		t.Fatal("resizeHighlightedWindow returned nil while flag was armed")
	}
	if m.forceResizePending {
		t.Fatal("forceResizePending not cleared after one-shot consumption")
	}
}

func TestSessionsMsgClearsStalePendingClose(t *testing.T) {
	t.Parallel()
	// If the target session disappears between `x` (arm) and
	// the next sessionsMsg, the pending close is cleared so the
	// prompt cannot land on a no-longer-existent row. A stale
	// confirmation that kept hanging would produce a confusing
	// "kill-window @N" on a window that has already been gone
	// for a poll tick.
	sc := &stubCloser{}
	m := modelWithClose(t, sc.closer(), []Session{
		{WindowID: "@1", Name: "one"},
	})
	m, _ = applyKey(t, m, "x")
	if m.pendingCloseWindow != "@1" {
		t.Fatalf("arm did not take: %q", m.pendingCloseWindow)
	}
	// Simulate an external kill: next poll shows an empty list.
	m = m.handleSessions(sessionsMsg{sessions: nil})
	if m.pendingCloseWindow != "" {
		t.Fatalf("pendingCloseWindow = %q; want cleared by stale poll", m.pendingCloseWindow)
	}
	// And pressing `y` now must not fire the closer (nothing to
	// confirm against).
	m, cmd := applyKey(t, m, "y")
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("post-stale `y` produced msg %T; want no-op", msg)
		}
	}
	if len(sc.recorded()) != 0 {
		t.Fatalf("closer called after stale cancel: %v", sc.recorded())
	}
	_ = m
}

// Silence "imported and not used" if the file is pared down in
// the future; tea is currently used via applyKey.
var _ tea.Cmd = nil

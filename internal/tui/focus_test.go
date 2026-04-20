package tui

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// focus_test.go pins the M3c focus / input-routing semantics. The
// fundamental invariant: in FocusSidebar, keystrokes run sm4c's
// navigation bindings; in FocusPane, they flow through KeySender
// to the pane (except ctrl+b which is always sm4c-reserved).
//
// Every test here constructs a Model with a stub KeySender and a
// pre-seeded paneByWindow map so pane focus has somewhere to send
// keys — matching what the CLI wires up in production from
// tmuxctl.

// stubKeySender records every (paneID, data) pair it was asked
// to forward, and lets a test pre-seed an error to return. It
// also serializes access so tests that exercise async behavior
// in a future milestone do not race. For the current milestone
// every KeySender call is synthesized in-process via applyKey;
// the mutex is future-proofing, not latency-critical.
type stubKeySender struct {
	mu    sync.Mutex
	calls []stubSendCall
	err   error
}

type stubSendCall struct {
	paneID string
	data   []byte
}

func (s *stubKeySender) send(_ context.Context, paneID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	s.calls = append(s.calls, stubSendCall{paneID: paneID, data: buf})
	return s.err
}

func (s *stubKeySender) recorded() []stubSendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubSendCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// focusedPaneModel builds a Model with one live session, a resolved
// pane, and a stub KeySender — which is the steady-state shape the
// pane-focus tests exercise.
func focusedPaneModel(t *testing.T, sender *stubKeySender) Model {
	t.Helper()
	m := NewModel(Deps{
		Lister:    stubLister(nil, nil),
		KeySender: sender.send,
	})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "primary"}},
	})
	// Pane resolver would normally populate paneByWindow via
	// paneResolvedMsg; for a unit test we can seed it directly.
	m.paneByWindow["@1"] = "%42"
	m.focus = FocusPane
	return m
}

// runKeyThroughUpdate exercises the public Update entrypoint (NOT
// handleKey directly) so the keysSentMsg round-trip behavior is
// also covered. It runs the returned cmd synchronously and feeds
// the resulting message back into Update, mirroring what the
// Bubble Tea runtime does in production.
func runKeyThroughUpdate(t *testing.T, m Model, msg tea.KeyMsg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m2, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, expected Model", next)
	}
	if cmd != nil {
		result := cmd()
		if result != nil {
			back, _ := m2.Update(result)
			m3, ok := back.(Model)
			if !ok {
				t.Fatalf("second Update returned %T, expected Model", back)
			}
			m2 = m3
		}
	}
	return m2
}

func TestPaneFocusForwardsRuneKeystroke(t *testing.T) {
	t.Parallel()

	sender := &stubKeySender{}
	m := focusedPaneModel(t, sender)
	m = runKeyThroughUpdate(t, m, keyMsg("a"))

	calls := sender.recorded()
	if len(calls) != 1 {
		t.Fatalf("KeySender calls = %d, want 1", len(calls))
	}
	if calls[0].paneID != "%42" {
		t.Fatalf("paneID = %q, want %%42", calls[0].paneID)
	}
	if !bytes.Equal(calls[0].data, []byte{0x61}) {
		t.Fatalf("data = % x, want 61", calls[0].data)
	}
	if m.focus != FocusPane {
		t.Fatalf("focus changed on rune forward: %v", m.focus)
	}
}

func TestPaneFocusForwardsEnterAsCR(t *testing.T) {
	t.Parallel()

	sender := &stubKeySender{}
	m := focusedPaneModel(t, sender)
	_ = runKeyThroughUpdate(t, m, keyMsg("enter"))

	calls := sender.recorded()
	if len(calls) != 1 || !bytes.Equal(calls[0].data, []byte{0x0d}) {
		t.Fatalf("enter forwarded as %+v; want single 0x0d", calls)
	}
}

func TestPaneFocusForwardsCtrlCToClaude(t *testing.T) {
	t.Parallel()

	// This is the critical M3c invariant: ctrl+c in pane focus
	// MUST go to claude (interrupt) and MUST NOT quit sm4c. The
	// "escape hatch" for quitting is to press ctrl+b first.
	sender := &stubKeySender{}
	m := focusedPaneModel(t, sender)
	next := runKeyThroughUpdate(t, m, keyMsg("ctrl+c"))

	if next.quitting {
		t.Fatal("ctrl+c in pane focus set quitting = true; MUST forward to claude instead")
	}
	if next.Action() != ActionNone {
		t.Fatalf("ctrl+c in pane focus: Action = %v; want ActionNone", next.Action())
	}
	calls := sender.recorded()
	if len(calls) != 1 || !bytes.Equal(calls[0].data, []byte{0x03}) {
		t.Fatalf("ctrl+c forwarded as %+v; want single 0x03", calls)
	}
}

func TestPaneFocusCtrlBTogglesBackToSidebar(t *testing.T) {
	t.Parallel()

	sender := &stubKeySender{}
	m := focusedPaneModel(t, sender)
	m = runKeyThroughUpdate(t, m, keyMsg("ctrl+b"))
	if m.focus != FocusSidebar {
		t.Fatalf("ctrl+b did not return focus to sidebar: %v", m.focus)
	}
	if len(sender.recorded()) != 0 {
		t.Fatalf("ctrl+b was forwarded to pane: %+v", sender.recorded())
	}
}

func TestSidebarFocusCtrlCQuits(t *testing.T) {
	t.Parallel()

	// The counter-invariant: ctrl+c in sidebar focus still quits
	// sm4c, so a user who never drops into a pane retains the
	// usual shell-level expectation.
	sender := &stubKeySender{}
	m := NewModel(Deps{
		Lister:    stubLister(nil, nil),
		KeySender: sender.send,
	})
	next := runKeyThroughUpdate(t, m, keyMsg("ctrl+c"))
	if !next.quitting {
		t.Fatal("ctrl+c in sidebar focus did not quit")
	}
	if len(sender.recorded()) != 0 {
		t.Fatalf("ctrl+c in sidebar focus leaked to pane: %+v", sender.recorded())
	}
}

func TestSidebarFocusQKeyQuits(t *testing.T) {
	t.Parallel()

	// q quits in sidebar focus; in pane focus q is a rune and is
	// forwarded. Pin both halves.
	sender := &stubKeySender{}
	m := NewModel(Deps{
		Lister:    stubLister(nil, nil),
		KeySender: sender.send,
	})
	sidebar := runKeyThroughUpdate(t, m, keyMsg("q"))
	if !sidebar.quitting {
		t.Fatal("q in sidebar focus did not quit")
	}

	pane := focusedPaneModel(t, sender)
	pane = runKeyThroughUpdate(t, pane, keyMsg("q"))
	if pane.quitting {
		t.Fatal("q in pane focus quit sm4c; want forwarded as rune")
	}
	calls := sender.recorded()
	if len(calls) != 1 || !bytes.Equal(calls[0].data, []byte{'q'}) {
		t.Fatalf("q in pane focus forwarded as %+v; want single 'q'", calls)
	}
}

func TestPaneFocusWithoutResolvedPaneDropsKeystroke(t *testing.T) {
	t.Parallel()

	// If the pane hasn't been resolved yet (the resolver round
	// trip is still in flight at the moment the user presses a
	// key), we MUST drop the keystroke rather than send it to a
	// phantom pane. A forwarded keystroke to no pane would error
	// at the tmuxctl layer; dropping here is cleaner.
	sender := &stubKeySender{}
	m := NewModel(Deps{
		Lister:    stubLister(nil, nil),
		KeySender: sender.send,
	})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "primary"}},
	})
	m.focus = FocusPane

	_ = runKeyThroughUpdate(t, m, keyMsg("a"))
	if len(sender.recorded()) != 0 {
		t.Fatalf("keystroke forwarded before pane resolved: %+v", sender.recorded())
	}
}

func TestPaneGoneErrorRevertsFocus(t *testing.T) {
	t.Parallel()

	// If KeySender reports the pane is gone (session closed
	// between the key press and the forward), the TUI must
	// revert focus to the sidebar so the next keystroke lands
	// on a meaningful surface. Any other KeySender error is
	// absorbed without flipping focus — a transient tmux hiccup
	// should not eject the user.
	sender := &stubKeySender{err: ErrPaneGone()}
	m := focusedPaneModel(t, sender)
	m = runKeyThroughUpdate(t, m, keyMsg("a"))
	if m.focus != FocusSidebar {
		t.Fatalf("pane-gone did not revert focus: %v", m.focus)
	}
}

func TestTransientSendErrorPreservesFocus(t *testing.T) {
	t.Parallel()

	sender := &stubKeySender{err: errors.New("temporary tmux glitch")}
	m := focusedPaneModel(t, sender)
	m = runKeyThroughUpdate(t, m, keyMsg("a"))
	if m.focus != FocusPane {
		t.Fatalf("transient error reverted focus to %v; want FocusPane", m.focus)
	}
}

func TestSessionsDroppingHighlightRevertsFocus(t *testing.T) {
	t.Parallel()

	// When the user closed the last session externally, the next
	// sessionsMsg drops every row. The Model must revert focus to
	// the sidebar so the empty state is usable; otherwise the
	// user would be "typing into nothing" with no visible cue.
	m := focusedPaneModel(t, &stubKeySender{})
	m = m.handleSessions(sessionsMsg{sessions: nil})
	if m.focus != FocusSidebar {
		t.Fatalf("empty sessionsMsg did not revert focus: %v", m.focus)
	}
}

func TestInitialFocusPaneIsHonoredOnFirstSessionsMsg(t *testing.T) {
	t.Parallel()

	// `sm4c [claude-args]` constructs the Model with
	// InitialFocus=FocusPane. Before the first sessionsMsg
	// arrives there is no session to focus — the request stays
	// pending. As soon as the first row lands (the freshly-
	// spawned window ID resolves into a Session), focus flips
	// to the pane so the user can type immediately.
	m := NewModel(Deps{
		Lister:           stubLister(nil, nil),
		InitialHighlight: "@7",
		InitialFocus:     FocusPane,
	})
	if m.focus != FocusSidebar {
		t.Fatalf("focus before sessionsMsg = %v; want FocusSidebar (pending)", m.focus)
	}
	if !m.pendingFocus {
		t.Fatal("pendingFocus not set for InitialFocus=FocusPane")
	}
	// First fetch: empty; pending focus stays.
	m = m.handleSessions(sessionsMsg{sessions: nil})
	if !m.pendingFocus || m.focus != FocusSidebar {
		t.Fatalf("empty first fetch consumed pendingFocus; focus=%v pending=%v", m.focus, m.pendingFocus)
	}
	// Second fetch: the target lands. pendingFocus must be
	// consumed and focus must flip.
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@7", Name: "target"}},
	})
	if m.focus != FocusPane {
		t.Fatalf("focus after matching sessionsMsg = %v; want FocusPane", m.focus)
	}
	if m.pendingFocus {
		t.Fatal("pendingFocus not cleared after consumption")
	}
}

func TestInitialFocusSidebarLeavesPendingFalse(t *testing.T) {
	t.Parallel()

	m := NewModel(Deps{
		Lister:       stubLister(nil, nil),
		InitialFocus: FocusSidebar,
	})
	if m.pendingFocus {
		t.Fatal("pendingFocus unexpectedly true for bare sidebar startup")
	}
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "x"}},
	})
	if m.focus != FocusSidebar {
		t.Fatalf("bare startup flipped to %v; want stay on FocusSidebar", m.focus)
	}
}

func TestNilKeySenderMakesPaneFocusReadOnly(t *testing.T) {
	t.Parallel()

	// A Model with focus on pane but no KeySender wired (tests,
	// degraded environments) MUST NOT crash and MUST NOT
	// silently exit — it simply drops the keystroke. The user
	// can press ctrl+b to go back to the sidebar.
	m := NewModel(Deps{Lister: stubLister(nil, nil)})
	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "a"}},
	})
	m.paneByWindow["@1"] = "%42"
	m.focus = FocusPane

	before := m
	after, cmd := m.Update(keyMsg("a"))
	if cmd != nil {
		t.Fatalf("nil sender produced cmd %T; want nil", cmd)
	}
	m2, ok := after.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", after)
	}
	if m2.focus != FocusPane {
		t.Fatalf("nil sender flipped focus to %v", m2.focus)
	}
	// Model must be unchanged (no hidden state shifted).
	if !modelsEqual(before, m2) {
		t.Fatal("nil sender mutated Model on dropped keystroke")
	}
}

func TestPaneFocusIndicatorAppearsInRightPaneHeader(t *testing.T) {
	t.Parallel()

	// The right-pane header carries a text marker so the user
	// can tell at a glance which surface owns keystrokes without
	// relying on border color (sm4c's no-hex-colors rule). Pin
	// both sides of the marker so a refactor can't silently
	// drop it.
	m := withSessions([]Session{{WindowID: "@1", Name: "primary"}})

	// Sidebar focus: the header tells the user how to focus.
	sidebar := m.renderRightPaneHeader()
	if !bytes.Contains([]byte(sidebar), []byte("ctrl+b")) {
		t.Fatalf("sidebar-focus header missing ctrl+b hint: %q", sidebar)
	}

	// Pane focus: a distinct marker appears.
	m.focus = FocusPane
	pane := m.renderRightPaneHeader()
	if !bytes.Contains([]byte(pane), []byte("[focus]")) {
		t.Fatalf("pane-focus header missing [focus] marker: %q", pane)
	}
}

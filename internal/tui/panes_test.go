package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// panes_test.go covers the M3b.2 pane-preview state machine as a
// set of pure (Model, Msg) -> (Model, Cmd) transitions. As with
// app_test.go, we drive Update with synthetic messages and never
// spin up a real Bubble Tea runtime, a real tmux server, or a
// real pane.
//
// The preview now runs through a charmbracelet/x/vt emulator
// instead of a raw-bytes ring, so the assertions here look through
// the VT's String() (plain) output rather than comparing byte
// slices: ANSI escapes are interpreted, wrapping is row-based, and
// the screen has a fixed grid.

// TestPaneTerminalWriteAndRenderRoundTrip pins the shape of the
// new pane backing: raw bytes go in, the interpreted screen comes
// back out. We read the plain String() snapshot for assertions so
// this test does not couple to the exact SGR encoding Render emits.
func TestPaneTerminalWriteAndRenderRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("empty terminal renders blank", func(t *testing.T) {
		t.Parallel()
		p := newPaneTerminal(20, 4)
		if p.written {
			t.Fatalf("fresh paneTerminal reports written=true")
		}
		// The VT emulator renders a grid; its plain-text snapshot
		// for an untouched screen is a sequence of blank rows.
		// Whitespace-only is the contract we rely on for the
		// "waiting for output" hint to stay visible.
		if got := strings.TrimSpace(p.emu.String()); got != "" {
			t.Fatalf("fresh terminal plain = %q; want blank", got)
		}
	})

	t.Run("plain bytes land on the grid verbatim", func(t *testing.T) {
		t.Parallel()
		p := newPaneTerminal(20, 4)
		p.write([]byte("hello world"))
		if !p.written {
			t.Fatalf("write did not flip written flag")
		}
		if got := p.emu.String(); !strings.Contains(got, "hello world") {
			t.Fatalf("terminal plain = %q; want to contain 'hello world'", got)
		}
	})

	t.Run("ansi sequences are consumed not echoed", func(t *testing.T) {
		t.Parallel()
		// A raw CSI SGR (\x1b[1;31m) and a reset (\x1b[0m) must be
		// absorbed by the parser: the plain-text snapshot should
		// contain only the visible payload, never the raw ESC byte.
		// This is the defensive posture we relied on stripToPrintable
		// for in M3b.1 — now the emulator provides it.
		p := newPaneTerminal(40, 4)
		p.write([]byte("\x1b[1;31mred\x1b[0m then plain"))
		plain := p.emu.String()
		if strings.ContainsRune(plain, 0x1b) {
			t.Fatalf("raw ESC byte leaked into plain snapshot: %q", plain)
		}
		if !strings.Contains(plain, "red") || !strings.Contains(plain, "plain") {
			t.Fatalf("visible payload missing from snapshot: %q", plain)
		}
	})

	t.Run("split writes re-assemble across chunk boundaries", func(t *testing.T) {
		t.Parallel()
		// tmux %output chunks are not guaranteed to align with
		// escape sequences. The VT parser's internal state must
		// carry over between Write calls; if it didn't, a chunk
		// boundary mid-escape would corrupt the screen.
		p := newPaneTerminal(40, 4)
		p.write([]byte("\x1b[1"))
		p.write([]byte(";31mred\x1b[0m done"))
		plain := p.emu.String()
		if strings.ContainsRune(plain, 0x1b) {
			t.Fatalf("ESC leaked from split-escape write: %q", plain)
		}
		if !strings.Contains(plain, "red") || !strings.Contains(plain, "done") {
			t.Fatalf("split-escape content missing: %q", plain)
		}
	})

	t.Run("resize changes emulator grid", func(t *testing.T) {
		t.Parallel()
		p := newPaneTerminal(20, 4)
		p.resize(40, 10)
		if got := p.emu.Width(); got != 40 {
			t.Fatalf("after resize, width = %d; want 40", got)
		}
		if got := p.emu.Height(); got != 10 {
			t.Fatalf("after resize, height = %d; want 10", got)
		}
	})

	t.Run("render preserves ansi styling", func(t *testing.T) {
		t.Parallel()
		// The styled snapshot MUST re-emit SGR escapes so colors
		// survive the handoff to the outer terminal. We don't pin
		// the exact byte sequence (lipgloss / vt may normalize),
		// only that some ESC-prefixed SGR payload is present.
		p := newPaneTerminal(40, 4)
		p.write([]byte("\x1b[31mred\x1b[0m"))
		styled := p.render()
		if !strings.ContainsRune(styled, 0x1b) {
			t.Fatalf("styled render stripped all ANSI; got %q", styled)
		}
	})
}

// stubStream returns a PaneEventStream that hands back the same
// channel on every call. Tests that want to feed synthetic events
// use this to keep the "one channel shared" semantics the TUI
// relies on.
func stubStream(ch <-chan PaneEvent) PaneEventStream {
	return func() <-chan PaneEvent { return ch }
}

// resolverStub records every windowID it has been asked about and
// returns a deterministic pane ID per window. If a window matches
// wantErrFor, the stub returns the canned error instead.
type resolverStub struct {
	panes      map[string]string
	wantErrFor string
	called     []string
}

func newResolverStub(panes map[string]string, errWin string) *resolverStub {
	return &resolverStub{panes: panes, wantErrFor: errWin}
}

func (r *resolverStub) resolver() ActivePaneResolver {
	return func(_ context.Context, windowID string) (string, error) {
		r.called = append(r.called, windowID)
		if windowID == r.wantErrFor {
			return "", errors.New("boom")
		}
		return r.panes[windowID], nil
	}
}

func TestPaneDataFeedsPerPaneEmulator(t *testing.T) {
	t.Parallel()
	// Deliver two chunks for two different panes; each pane's
	// emulator must see only its own bytes, so pane-A content
	// never leaks into pane-B.
	m := NewModel(nil, 0, nil, nil, "")
	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("alpha")})
	m = m.handlePaneData(paneDataMsg{paneID: "%2", data: []byte("beta")})
	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte(" more")})

	t1 := m.paneTerminals["%1"]
	t2 := m.paneTerminals["%2"]
	if t1 == nil || t2 == nil {
		t.Fatalf("expected terminals for both panes: %%1=%v %%2=%v", t1, t2)
	}
	p1 := t1.emu.String()
	p2 := t2.emu.String()
	if !strings.Contains(p1, "alpha more") {
		t.Fatalf("pane %%1 plain = %q; want to contain 'alpha more'", p1)
	}
	if strings.Contains(p1, "beta") {
		t.Fatalf("pane %%1 plain leaked pane %%2 bytes: %q", p1)
	}
	if !strings.Contains(p2, "beta") {
		t.Fatalf("pane %%2 plain = %q; want to contain 'beta'", p2)
	}
}

func TestPaneDataWithEmptyIDIsIgnored(t *testing.T) {
	t.Parallel()
	// A paneDataMsg with an empty pane ID is defensive nonsense
	// (the CLI bridge filters on OutputEvent, which always has a
	// pane ID). The Model must not allocate an emulator for it.
	m := NewModel(nil, 0, nil, nil, "")
	m = m.handlePaneData(paneDataMsg{paneID: "", data: []byte("x")})
	if _, ok := m.paneTerminals[""]; ok {
		t.Fatalf("empty-paneID data was buffered under key %q", "")
	}
}

func TestPaneStreamClosedStopsRearming(t *testing.T) {
	t.Parallel()
	// Model sees the paneStreamClosedMsg: it must flip the flag,
	// drop the channel reference, and return a nil cmd so the
	// waitForPaneEvent chain does not re-arm on a closed channel
	// (which would spin a hot loop).
	ch := make(chan PaneEvent)
	close(ch)
	m := NewModel(nil, 0, stubStream(ch), nil, "")
	// Init arms a reader; running it against a closed channel
	// yields paneStreamClosedMsg.
	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("Init returned nil despite stream being wired")
	}
	msg := initCmd()
	if _, ok := msg.(paneStreamClosedMsg); !ok {
		t.Fatalf("initial read returned %T; want paneStreamClosedMsg", msg)
	}
	next, cmd := m.Update(msg)
	if cmd != nil {
		t.Fatalf("paneStreamClosedMsg re-armed (cmd=%T); want nil", cmd)
	}
	got := next.(Model)
	if !got.paneStreamClosed {
		t.Fatal("paneStreamClosed flag not set")
	}
	if got.paneEvents != nil {
		t.Fatal("paneEvents channel was not dropped on close")
	}
}

func TestPaneDataMsgRearmsWaiter(t *testing.T) {
	t.Parallel()
	// A paneDataMsg must produce a non-nil cmd so the runtime
	// keeps pumping the stream. Otherwise the preview would freeze
	// after the first chunk.
	ch := make(chan PaneEvent, 1)
	m := NewModel(nil, 0, stubStream(ch), nil, "")
	_, cmd := m.Update(paneDataMsg{paneID: "%1", data: []byte("x")})
	if cmd == nil {
		t.Fatal("paneDataMsg returned nil cmd; stream reader would stall")
	}
}

func TestHighlightChangeTriggersResolver(t *testing.T) {
	t.Parallel()
	// Navigating j/k should issue a resolver call exactly once
	// per distinct highlighted window. Re-navigating back to a
	// row whose pane is already cached must NOT re-issue the call.
	stub := newResolverStub(map[string]string{
		"@1": "%10",
		"@2": "%20",
	}, "")
	m := withSessions([]Session{
		{WindowID: "@1", Name: "a"},
		{WindowID: "@2", Name: "b"},
	})
	m.paneResolver = stub.resolver()

	// First sessionsMsg already happened inside withSessions; call
	// resolveHighlightedPaneIfNeeded to simulate the post-fetch cmd.
	if cmd := m.resolveHighlightedPaneIfNeeded(); cmd == nil {
		t.Fatal("expected resolver cmd on initial highlight")
	} else {
		msg := cmd()
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	if len(stub.called) != 1 || stub.called[0] != "@1" {
		t.Fatalf("after initial resolve, calls = %v; want [@1]", stub.called)
	}
	if got := m.paneByWindow["@1"]; got != "%10" {
		t.Fatalf("paneByWindow[@1] = %q; want %%10", got)
	}

	// Navigate down to @2; that triggers a fresh resolve.
	after, cmd := applyKey(t, m, "j")
	if cmd == nil {
		t.Fatal("j on unresolved highlight did not emit a resolve cmd")
	}
	m = after
	msg := cmd()
	next, _ := m.Update(msg)
	m = next.(Model)
	if len(stub.called) != 2 || stub.called[1] != "@2" {
		t.Fatalf("after j, calls = %v; want [@1 @2]", stub.called)
	}

	// Navigate back to @1 — already cached, must NOT re-resolve.
	before := len(stub.called)
	after2, cmd2 := applyKey(t, m, "k")
	m = after2
	if cmd2 != nil {
		t.Fatalf("k to cached highlight produced cmd %T; want nil", cmd2)
	}
	if len(stub.called) != before {
		t.Fatalf("k re-issued resolver; calls = %v", stub.called)
	}
}

func TestResolverErrorSurfacedPerWindow(t *testing.T) {
	t.Parallel()
	// A resolver failure must not clear an already-working pane
	// mapping for other windows, and must record the error under
	// the offending window ID so the right pane can render it.
	stub := newResolverStub(map[string]string{"@1": "%10"}, "@2")
	m := withSessions([]Session{
		{WindowID: "@1", Name: "ok"},
		{WindowID: "@2", Name: "bad"},
	})
	m.paneResolver = stub.resolver()

	// Resolve @1 (succeeds).
	cmd := m.resolveHighlightedPaneIfNeeded()
	next, _ := m.Update(cmd())
	m = next.(Model)

	// Navigate to @2 (fails).
	m, cmd2 := applyKey(t, m, "j")
	if cmd2 == nil {
		t.Fatal("j to failing window did not emit resolver cmd")
	}
	next2, _ := m.Update(cmd2())
	m = next2.(Model)

	if err, ok := m.paneErrByWindow["@2"]; !ok || err == nil {
		t.Fatalf("paneErrByWindow[@2] not set: %v", m.paneErrByWindow)
	}
	if _, ok := m.paneErrByWindow["@1"]; ok {
		t.Fatal("resolver failure on @2 leaked into @1")
	}
	if got := m.paneByWindow["@1"]; got != "%10" {
		t.Fatalf("successful @1 mapping cleared: %q", got)
	}
}

func TestSessionsMsgPrunesStaleWindowEntries(t *testing.T) {
	t.Parallel()
	// After a session closes, its paneByWindow / paneErrByWindow
	// entries must be pruned so the caches do not grow without
	// bound across long-running sessions.
	m := withSessions([]Session{
		{WindowID: "@1", Name: "a"},
		{WindowID: "@2", Name: "b"},
	})
	m.paneByWindow["@1"] = "%10"
	m.paneByWindow["@2"] = "%20"
	m.paneErrByWindow["@2"] = errors.New("stale")

	m = m.handleSessions(sessionsMsg{
		sessions: []Session{{WindowID: "@1", Name: "a"}},
	})
	if _, ok := m.paneByWindow["@2"]; ok {
		t.Fatal("paneByWindow[@2] not pruned after session closed")
	}
	if _, ok := m.paneErrByWindow["@2"]; ok {
		t.Fatal("paneErrByWindow[@2] not pruned after session closed")
	}
	if got := m.paneByWindow["@1"]; got != "%10" {
		t.Fatalf("live @1 pruned by mistake: %q", got)
	}
}

func TestRightPaneShowsEmulatorForHighlightedSession(t *testing.T) {
	t.Parallel()
	// End-to-end: a session is highlighted, its pane is resolved,
	// and the stream has delivered bytes. The rendered right pane
	// must include the most recent visible payload.
	m := withSessions([]Session{{WindowID: "@1", Name: "refactor"}})
	m.paneResolver = newResolverStub(map[string]string{"@1": "%10"}, "").resolver()

	// Resolve.
	next, _ := m.Update(paneResolvedMsg{windowID: "@1", paneID: "%10"})
	m = next.(Model)

	// Give the model a realistic terminal size so the split layout
	// kicks in; width/height are stashed by WindowSizeMsg and the
	// pane emulator is resized to the body dims.
	next3, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next3.(Model)

	// Feed bytes.
	next2, _ := m.Update(paneDataMsg{paneID: "%10", data: []byte("hello world\r\n")})
	m = next2.(Model)

	out := m.View()
	if !strings.Contains(out, "hello world") {
		t.Fatalf("right pane missing emulator payload:\n%s", out)
	}
	if !strings.Contains(out, "refactor") {
		t.Fatalf("right pane missing session name:\n%s", out)
	}
}

func TestRightPaneEmulatorAbsorbsRawControlSequences(t *testing.T) {
	t.Parallel()
	// Raw tmux output contains ANSI escapes and control bytes.
	// The VT emulator parses them — nothing a pane emits should
	// appear in the rendered view as a literal ESC byte. This is
	// the defensive posture stripToPrintable provided in M3b.1
	// and the emulator provides natively in M3b.2.
	m := withSessions([]Session{{WindowID: "@1", Name: "tty"}})
	m.paneResolver = newResolverStub(map[string]string{"@1": "%10"}, "").resolver()
	next, _ := m.Update(paneResolvedMsg{windowID: "@1", paneID: "%10"})
	m = next.(Model)
	next2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next2.(Model)

	// \x1b[2J clears the screen; \x07 is BEL (interpreted, not
	// rendered); \x1b[31m sets red; \x1b[0m resets.
	next3, _ := m.Update(paneDataMsg{
		paneID: "%10",
		data:   []byte("before\x1b[2Jafter\x07\x1b[31mred\x1b[0m tail"),
	})
	m = next3.(Model)

	// The VT emulator consumes escape sequences; the plain-text
	// snapshot should never contain a raw ESC byte. The styled
	// render (which is what the View emits) may re-emit SGR
	// escapes for color, so we check the plain snapshot of the
	// emulator directly.
	term := m.paneTerminals["%10"]
	if term == nil {
		t.Fatalf("expected emulator for %%10")
	}
	plain := term.emu.String()
	for _, bad := range []byte{0x1b, 0x07} {
		if strings.IndexByte(plain, bad) >= 0 {
			t.Fatalf("emulator plain snapshot leaked byte 0x%02x: %q", bad, plain)
		}
	}
	// "before" was cleared by \x1b[2J so it must not survive.
	if strings.Contains(plain, "before") {
		t.Fatalf("\\x1b[2J did not clear: %q", plain)
	}
	// "after" and "tail" must be visible.
	if !strings.Contains(plain, "after") || !strings.Contains(plain, "tail") {
		t.Fatalf("visible payload missing from snapshot: %q", plain)
	}
}

func TestWindowResizePropagatesToExistingEmulators(t *testing.T) {
	t.Parallel()
	// A terminal resize while a pane emulator exists must resize
	// that emulator too; otherwise claude keeps drawing into a
	// stale grid and wrapping no longer matches what the outer
	// terminal shows. This pins the geometry sync.
	m := NewModel(nil, 0, nil, nil, "")
	// Boot an emulator at defaults by feeding any bytes.
	m = m.handlePaneData(paneDataMsg{paneID: "%10", data: []byte("x")})
	if w, h := m.paneTerminals["%10"].emu.Width(), m.paneTerminals["%10"].emu.Height(); w != defaultPaneWidth || h != defaultPaneHeight {
		t.Fatalf("fresh emulator dims = %dx%d; want %dx%d", w, h, defaultPaneWidth, defaultPaneHeight)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = next.(Model)
	if w := m.paneTerminals["%10"].emu.Width(); w != m.paneViewW {
		t.Fatalf("emulator width = %d; want paneViewW = %d", w, m.paneViewW)
	}
	if h := m.paneTerminals["%10"].emu.Height(); h != m.paneViewH {
		t.Fatalf("emulator height = %d; want paneViewH = %d", h, m.paneViewH)
	}
}

func TestRightPaneFallbackWhenNoStream(t *testing.T) {
	t.Parallel()
	// Without a stream or resolver the right pane must show an
	// explicit "unavailable" hint rather than a blank box. This
	// is the configuration tests and CI runs under, and it is
	// also what users see on fresh installs before the first
	// session exists.
	m := withSessions([]Session{{WindowID: "@1", Name: "x"}})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)
	out := m.View()
	if !strings.Contains(out, "pane preview unavailable") {
		t.Fatalf("right pane missing fallback hint:\n%s", out)
	}
}

func TestRightPaneReportsStreamClosed(t *testing.T) {
	t.Parallel()
	// When the stream was live and then closed, the right pane
	// must explain the state so the user does not think the
	// session is hung.
	m := withSessions([]Session{{WindowID: "@1", Name: "x"}})
	m.paneResolver = newResolverStub(map[string]string{"@1": "%10"}, "").resolver()
	// Pretend we had a stream.
	ch := make(chan PaneEvent)
	close(ch)
	m.paneEvents = ch
	// Drive the closed-msg path.
	next, _ := m.Update(paneStreamClosedMsg{})
	m = next.(Model)
	next2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next2.(Model)
	out := m.View()
	if !strings.Contains(out, "preview disconnected") {
		t.Fatalf("right pane missing stream-closed hint:\n%s", out)
	}
}

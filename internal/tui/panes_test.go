package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// panes_test.go covers the M3b.1 pane-preview state machine as a
// set of pure (Model, Msg) -> (Model, Cmd) transitions. As with
// app_test.go, we drive Update with synthetic messages and never
// spin up a real Bubble Tea runtime, a real tmux server, or a
// real pane.

// TestPaneBufferAppendAndSnapshot pins the contract for the ring:
// it returns only what was written, in order, with a hard cap at
// paneBufferBytes. The implementation is compact but uses pointer
// arithmetic (head, size), so the cases below target the boundary
// conditions that would fail a naive linear buffer.
func TestPaneBufferAppendAndSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("empty snapshot is nil", func(t *testing.T) {
		t.Parallel()
		b := newPaneBuffer()
		if got := b.snapshot(); got != nil {
			t.Fatalf("empty snapshot = %q; want nil", got)
		}
	})

	t.Run("small write round-trips", func(t *testing.T) {
		t.Parallel()
		b := newPaneBuffer()
		b.append([]byte("hello"))
		if got := b.snapshot(); !bytes.Equal(got, []byte("hello")) {
			t.Fatalf("snapshot = %q; want hello", got)
		}
	})

	t.Run("multiple appends preserve order", func(t *testing.T) {
		t.Parallel()
		b := newPaneBuffer()
		b.append([]byte("one "))
		b.append([]byte("two "))
		b.append([]byte("three"))
		if got := b.snapshot(); !bytes.Equal(got, []byte("one two three")) {
			t.Fatalf("snapshot = %q; want 'one two three'", got)
		}
	})

	t.Run("overflow keeps most recent bytes", func(t *testing.T) {
		t.Parallel()
		b := newPaneBuffer()
		// Fill to cap + 100, then verify only the last cap bytes
		// survive. This exercises the wrap-around branch.
		first := bytes.Repeat([]byte{'a'}, paneBufferBytes-100)
		b.append(first)
		extra := bytes.Repeat([]byte{'b'}, 200)
		b.append(extra)
		got := b.snapshot()
		if len(got) != paneBufferBytes {
			t.Fatalf("snapshot len = %d; want %d", len(got), paneBufferBytes)
		}
		// The tail is the most recently appended bytes.
		if !bytes.HasSuffix(got, bytes.Repeat([]byte{'b'}, 200)) {
			t.Fatalf("snapshot tail missing latest 200 'b' bytes")
		}
		// The head must contain 'a' (some of it; exactly paneBufferBytes-200 of them).
		if got[0] != 'a' {
			t.Fatalf("snapshot[0] = %q; want 'a'", got[0])
		}
	})

	t.Run("single write larger than capacity keeps tail", func(t *testing.T) {
		t.Parallel()
		b := newPaneBuffer()
		big := make([]byte, paneBufferBytes+500)
		for i := range big {
			big[i] = byte(i % 256)
		}
		b.append(big)
		got := b.snapshot()
		if len(got) != paneBufferBytes {
			t.Fatalf("snapshot len = %d; want %d", len(got), paneBufferBytes)
		}
		// The kept region should be the last paneBufferBytes bytes
		// of big, verbatim.
		want := big[len(big)-paneBufferBytes:]
		if !bytes.Equal(got, want) {
			t.Fatalf("snapshot mismatch on oversize write")
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

// stubResolver returns an ActivePaneResolver that records every
// windowID it's asked about and returns a deterministic pane ID
// per window. If a window matches wantErrFor, the stub returns the
// canned error instead.
type resolverStub struct {
	panes     map[string]string
	wantErrFor string
	called    []string
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

func TestPaneDataBuffersByPaneID(t *testing.T) {
	t.Parallel()
	// Deliver two chunks for two different panes; snapshots must
	// be independent, so bytes from pane A never leak into pane B.
	m := NewModel(nil, 0, nil, nil, "")
	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte("alpha")})
	m = m.handlePaneData(paneDataMsg{paneID: "%2", data: []byte("beta")})
	m = m.handlePaneData(paneDataMsg{paneID: "%1", data: []byte(" more")})

	if got := string(m.paneBuffers["%1"].snapshot()); got != "alpha more" {
		t.Fatalf("pane %%1 snapshot = %q; want 'alpha more'", got)
	}
	if got := string(m.paneBuffers["%2"].snapshot()); got != "beta" {
		t.Fatalf("pane %%2 snapshot = %q; want 'beta'", got)
	}
}

func TestPaneDataWithEmptyIDIsIgnored(t *testing.T) {
	t.Parallel()
	// A paneDataMsg with an empty pane ID is defensive nonsense
	// (the CLI bridge filters on OutputEvent, which always has a
	// pane ID). The Model must not allocate a buffer for it.
	m := NewModel(nil, 0, nil, nil, "")
	m = m.handlePaneData(paneDataMsg{paneID: "", data: []byte("x")})
	if _, ok := m.paneBuffers[""]; ok {
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

func TestRightPaneShowsBufferForHighlightedSession(t *testing.T) {
	t.Parallel()
	// End-to-end: a session is highlighted, its pane is resolved,
	// and the stream has delivered bytes. The rendered right pane
	// must include the latest portion of those bytes.
	m := withSessions([]Session{{WindowID: "@1", Name: "refactor"}})
	m.paneResolver = newResolverStub(map[string]string{"@1": "%10"}, "").resolver()

	// Resolve.
	next, _ := m.Update(paneResolvedMsg{windowID: "@1", paneID: "%10"})
	m = next.(Model)

	// Feed bytes.
	next2, _ := m.Update(paneDataMsg{paneID: "%10", data: []byte("hello world\n")})
	m = next2.(Model)

	// Give the model a realistic terminal size so the split layout
	// kicks in; width/height are stashed by WindowSizeMsg.
	next3, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next3.(Model)

	out := m.View()
	if !strings.Contains(out, "hello world") {
		t.Fatalf("right pane missing buffered bytes:\n%s", out)
	}
	if !strings.Contains(out, "refactor") {
		t.Fatalf("right pane missing session name:\n%s", out)
	}
}

func TestRightPaneStripsNonPrintableBytes(t *testing.T) {
	t.Parallel()
	// Raw tmux output contains ANSI escapes, UTF-8, and control
	// bytes. M3b.1 is read-only raw-preview, so stripToPrintable
	// must drop everything non-ASCII-printable so nothing a
	// malicious claude could emit corrupts the outer terminal.
	// This pins the policy at the rendering boundary.
	raw := []byte("safe\x1b[2Jhidden\x07bell\xffnon-ascii")
	got := stripToPrintable(raw, 200, 10)
	for _, bad := range []byte{0x1b, 0x07, 0xff} {
		if bytes.IndexByte([]byte(got), bad) >= 0 {
			t.Fatalf("stripToPrintable left byte 0x%02x in output: %q", bad, got)
		}
	}
	// Printable content must survive verbatim.
	if !strings.Contains(got, "safe") || !strings.Contains(got, "bell") || !strings.Contains(got, "non-ascii") {
		t.Fatalf("stripToPrintable lost printable text: %q", got)
	}
}

func TestRightPaneClipsToRecentLines(t *testing.T) {
	t.Parallel()
	// When the buffer contains many lines, only the most recent
	// maxLines should be rendered. This is the "live tail" shape
	// expected of a pane preview.
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line-"+intToString(i))
	}
	raw := []byte(strings.Join(lines, "\n"))
	got := stripToPrintable(raw, 200, 5)
	// line-95..line-99 must be present; line-50 must not.
	if !strings.Contains(got, "line-99") || !strings.Contains(got, "line-95") {
		t.Fatalf("recent lines missing from clipped output:\n%s", got)
	}
	if strings.Contains(got, "line-50") {
		t.Fatalf("older line leaked into clipped output:\n%s", got)
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

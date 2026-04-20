package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPreviewRenderedView is a throwaway dev aid that prints what
// the sidebar actually looks like at a realistic terminal size, so
// a maintainer can eyeball the layout by running:
//
//	go test -run TestPreviewRenderedView -v ./internal/tui
//
// It intentionally asserts nothing beyond non-emptiness; the other
// tests in this package lock the behavior.
func TestPreviewRenderedView(t *testing.T) {
	t.Parallel()

	// Build a model with live pane data so the preview case shows
	// the M3b.2 VT-rendered right-pane. We pin the resolver output
	// via paneResolvedMsg rather than driving a fake ctx round-trip.
	// The renderer suppresses pane content when both paneEvents and
	// paneResolver are nil, so wire a stub resolver to flip that
	// branch — we never call it here, paneResolvedMsg bypasses it.
	// The window size MUST be delivered before the pane data so the
	// emulator is created at the right-pane body dimensions rather
	// than the default 80x24 (which on a 140-wide preview would
	// crop the right-hand edge of the emulator).
	previewSessions := []Session{
		{WindowID: "@1", Name: "refactor-auth", Active: true},
		{WindowID: "@4", Name: "spike-queue"},
	}
	stubResolver := ActivePaneResolver(func(context.Context, string) (string, error) {
		return "%11", nil
	})
	withPreview := NewModel(stubLister(previewSessions, nil), 0, nil, stubResolver, "")
	withPreview = withPreview.handleSessions(sessionsMsg{sessions: previewSessions})
	sz, _ := withPreview.Update(tea.WindowSizeMsg{Width: 140, Height: 32})
	withPreview = sz.(Model)
	n1, _ := withPreview.Update(paneResolvedMsg{windowID: "@1", paneID: "%11"})
	withPreview = n1.(Model)
	// Terminals send \r\n between lines (LF alone advances the row
	// but leaves the column where it was, producing a staircase).
	// That's what tmux forwards verbatim on its %output channel.
	n2, _ := withPreview.Update(paneDataMsg{paneID: "%11", data: []byte(
		"$ claude --help\r\n" +
			"Usage: claude [options] [prompt]\r\n" +
			"\r\n" +
			"Options:\r\n" +
			"  -n, --name <string>   session name\r\n" +
			"  -h, --help            show help\r\n" +
			"$ _\r\n")})
	withPreview = n2.(Model)

	cases := []struct {
		name string
		m    Model
		w, h int
	}{
		{"empty_120x30", emptyModel(), 120, 30},
		{"with_sessions_120x30", withSessions([]Session{
			{WindowID: "@1", Name: "refactor-auth"},
			{WindowID: "@4", Name: "spike-queue", Active: true},
			{WindowID: "@9", Name: "docs-pass"},
		}), 120, 30},
		{"with_pane_preview_140x32", withPreview, 140, 32},
		{"narrow_40x20_fallback", emptyModel(), 40, 20},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			next, _ := c.m.Update(tea.WindowSizeMsg{Width: c.w, Height: c.h})
			m := next.(Model)
			out := m.View()
			if strings.TrimSpace(out) == "" {
				t.Fatal("rendered view is empty")
			}
			t.Logf("\n=== %s (%dx%d) ===\n%s=== end ===", c.name, c.w, c.h, out)
		})
	}
}

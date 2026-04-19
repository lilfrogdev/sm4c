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
	// the M3b.1 right-pane render path. We pin the resolver output
	// via paneResolvedMsg rather than driving a fake ctx round-trip.
	// The renderer suppresses pane content when both paneEvents and
	// paneResolver are nil, so wire a stub resolver to flip that
	// branch — we never call it here, paneResolvedMsg bypasses it.
	previewSessions := []Session{
		{WindowID: "@1", Name: "refactor-auth", Active: true},
		{WindowID: "@4", Name: "spike-queue"},
	}
	stubResolver := ActivePaneResolver(func(context.Context, string) (string, error) {
		return "%11", nil
	})
	withPreview := NewModel(stubLister(previewSessions, nil), 0, nil, stubResolver, "")
	withPreview = withPreview.handleSessions(sessionsMsg{sessions: previewSessions})
	n1, _ := withPreview.Update(paneResolvedMsg{windowID: "@1", paneID: "%11"})
	withPreview = n1.(Model)
	n2, _ := withPreview.Update(paneDataMsg{paneID: "%11", data: []byte(
		"$ claude --help\n" +
			"Usage: claude [options] [prompt]\n" +
			"\n" +
			"Options:\n" +
			"  -n, --name <string>   session name\n" +
			"  -h, --help            show help\n" +
			"$ _\n")})
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

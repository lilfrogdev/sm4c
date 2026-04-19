package tui

import (
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

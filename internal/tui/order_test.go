package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplySessionOrder(t *testing.T) {
	t.Parallel()
	mkSessions := func(names ...string) []Session {
		s := make([]Session, len(names))
		for i, n := range names {
			s[i] = Session{Name: n, WindowID: "@" + n}
		}
		return s
	}

	// order slices use WindowIDs (the "@<name>" format mkSessions generates).
	cases := []struct {
		name     string
		sessions []Session
		order    []string
		want     []string // expected Name sequence
	}{
		{
			name:     "empty order is identity",
			sessions: mkSessions("a", "b", "c"),
			order:    nil,
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "order applied",
			sessions: mkSessions("a", "b", "c"),
			order:    []string{"@c", "@a", "@b"},
			want:     []string{"c", "a", "b"},
		},
		{
			name:     "unknown sessions append at end in original order",
			sessions: mkSessions("a", "b", "c", "d"),
			order:    []string{"@c", "@a"},
			want:     []string{"c", "a", "b", "d"},
		},
		{
			name:     "removed entries in order are skipped",
			sessions: mkSessions("a", "c"),
			order:    []string{"@c", "@b", "@a"}, // @b no longer exists
			want:     []string{"c", "a"},
		},
		{
			name:     "empty sessions",
			sessions: nil,
			order:    []string{"@a", "@b"},
			want:     []string{},
		},
		{
			name:     "all unknown",
			sessions: mkSessions("x", "y"),
			order:    []string{"@a", "@b"},
			want:     []string{"x", "y"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := applySessionOrder(c.sessions, c.order)
			if len(got) != len(c.want) {
				t.Fatalf("got %d sessions, want %d: %v", len(got), len(c.want), got)
			}
			for i, s := range got {
				if s.Name != c.want[i] {
					t.Errorf("pos %d: got %q, want %q", i, s.Name, c.want[i])
				}
			}
		})
	}
}

func TestMoveSession(t *testing.T) {
	t.Parallel()
	mkSessions := func(names ...string) []Session {
		s := make([]Session, len(names))
		for i, n := range names {
			s[i] = Session{Name: n}
		}
		return s
	}
	names := func(ss []Session) []string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = s.Name
		}
		return out
	}

	cases := []struct {
		from, to int
		want     []string
	}{
		{0, 2, []string{"b", "c", "a", "d"}},
		{2, 0, []string{"c", "a", "b", "d"}},
		{1, 3, []string{"a", "c", "d", "b"}},
		{3, 1, []string{"a", "d", "b", "c"}},
		{1, 1, []string{"a", "b", "c", "d"}}, // no-op
	}

	for _, c := range cases {
		c := c
		t.Run("", func(t *testing.T) {
			t.Parallel()
			in := mkSessions("a", "b", "c", "d")
			got := names(moveSession(in, c.from, c.to))
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("pos %d: got %q, want %q (full: %v)", i, got[i], c.want[i], got)
				}
			}
		})
	}
}

func TestSaveLoadSessionOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sm4c", "session_order")

	names := []string{"alpha", "beta", "gamma"}
	if err := saveSessionOrder(path, names); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := loadSessionOrder(path)
	if len(got) != len(names) {
		t.Fatalf("got %v, want %v", got, names)
	}
	for i, n := range got {
		if n != names[i] {
			t.Errorf("pos %d: got %q, want %q", i, n, names[i])
		}
	}
}

func TestLoadSessionOrderMissing(t *testing.T) {
	t.Parallel()
	got := loadSessionOrder(filepath.Join(t.TempDir(), "no_such_file"))
	if got != nil {
		t.Fatalf("expected nil for missing file, got %v", got)
	}
}

func TestSaveSessionOrderCreatesDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "session_order")
	if err := saveSessionOrder(path, []string{"foo"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

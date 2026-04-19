package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnownClaudeLocations_ContainsLocalBin(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	want := filepath.Join(home, ".local", "bin", "claude")

	got := KnownClaudeLocations()
	var found bool
	for _, p := range got {
		if p == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("KnownClaudeLocations() missing %q; got %v", want, got)
	}
}

func TestKnownTmuxLocations_ContainsSystemBins(t *testing.T) {
	t.Parallel()

	got := KnownTmuxLocations()
	wantAll := []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
	}
	for _, w := range wantAll {
		var found bool
		for _, p := range got {
			if p == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("KnownTmuxLocations() missing %q; got %v", w, got)
		}
	}
}

func TestKnownLocations_OnlyAbsolute(t *testing.T) {
	t.Parallel()

	for _, p := range append(KnownClaudeLocations(), KnownTmuxLocations()...) {
		if !filepath.IsAbs(p) {
			t.Errorf("path is not absolute: %q", p)
		}
		if strings.Contains(p, "..") {
			t.Errorf("path contains traversal: %q", p)
		}
	}
}

func TestFindKnownBinary_PicksFirstExecutable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")
	nonExec := filepath.Join(dir, "nonexec")
	exec := filepath.Join(dir, "exec")

	if err := os.WriteFile(nonExec, []byte("#!/bin/sh\ntrue\n"), 0o600); err != nil {
		t.Fatalf("write nonexec: %v", err)
	}
	if err := os.WriteFile(exec, []byte("#!/bin/sh\ntrue\n"), 0o600); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	if err := os.Chmod(exec, 0o700); err != nil { // #nosec G302 -- test fixture must be executable
		t.Fatalf("chmod exec: %v", err)
	}

	got, ok := FindKnownBinary([]string{missing, nonExec, exec})
	if !ok {
		t.Fatalf("FindKnownBinary returned ok=false; wanted %q", exec)
	}
	if got != exec {
		t.Fatalf("got %q; want %q", got, exec)
	}
}

func TestFindKnownBinary_SkipsRelative(t *testing.T) {
	t.Parallel()

	_, ok := FindKnownBinary([]string{"claude", "./claude"})
	if ok {
		t.Fatal("FindKnownBinary accepted relative paths; it must not")
	}
}

func TestFindKnownBinary_SkipsDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, ok := FindKnownBinary([]string{dir}); ok {
		t.Fatal("FindKnownBinary returned a directory")
	}
}

func TestFindKnownBinary_ReturnsFalseWhenNothingMatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, ok := FindKnownBinary([]string{
		filepath.Join(dir, "a"),
		filepath.Join(dir, "b"),
	}); ok {
		t.Fatal("FindKnownBinary returned ok=true for nonexistent candidates")
	}
}

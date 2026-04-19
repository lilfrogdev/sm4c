package tmuxctl

import (
	"strings"
	"testing"
)

func TestParseWindows_Basic(t *testing.T) {
	t.Parallel()

	// Three rows: an active managed window, a background managed
	// window, and an unmanaged window created outside sm4c.
	in := []byte("" +
		"@1\t1\tsm4c\tclaude\t*\trefactor-api\n" +
		"@2\t0\tsm4c\tclaude\t#\ttests\n" +
		"@3\t0\tsm4c\t\t\trogue-shell\n")

	got, err := parseWindows(in)
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 rows; got %d: %+v", len(got), got)
	}
	if got[0].ID != "@1" || !got[0].Active || got[0].Name != "refactor-api" || got[0].Flags != "*" || got[0].Kind != "claude" {
		t.Errorf("row 0 mismatch: %+v", got[0])
	}
	if !got[0].Managed() {
		t.Errorf("row 0 Managed() want true")
	}
	if got[2].ID != "@3" || got[2].Kind != "" || got[2].Managed() {
		t.Errorf("row 2 (unmanaged) mismatch: %+v", got[2])
	}
}

func TestParseWindows_SanitizesName(t *testing.T) {
	t.Parallel()

	// Window name contains an ANSI escape sequence — this is the scary
	// case from the plan. parseWindows must strip it before handing
	// back, so nothing a hostile claude title can do will corrupt the
	// outer terminal when the sidebar renders it.
	in := []byte("@1\t1\tsm4c\tclaude\t*\tevil\x1b[2Jname\n")

	got, err := parseWindows(in)
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row; got %d", len(got))
	}
	name := got[0].Name
	if strings.ContainsRune(name, 0x1b) {
		t.Errorf("Name still contains ESC: %q", name)
	}
	if name != "evilname" {
		t.Errorf("Name = %q; want %q", name, "evilname")
	}
}

func TestParseWindows_NameWithTabs(t *testing.T) {
	t.Parallel()

	// If a window name contains a literal tab (unlikely but possible —
	// tmux does not strip them), the name field must absorb it because
	// SplitN's N=6 bundles everything past the 5th tab into parts[5].
	// safe.Label then strips control bytes (including \t) so the
	// stored Name is tab-free without losing the surrounding chars.
	in := []byte("@1\t1\tsm4c\tclaude\t*\ta\tb\tc\n")
	got, err := parseWindows(in)
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row; got %d", len(got))
	}
	if got[0].Name != "abc" {
		t.Errorf("Name = %q; want %q (tabs stripped by safe.Label)", got[0].Name, "abc")
	}
}

func TestParseWindows_EmptyInput(t *testing.T) {
	t.Parallel()

	for _, in := range [][]byte{nil, {}, []byte("\n"), []byte("\n\n")} {
		got, err := parseWindows(in)
		if err != nil {
			t.Errorf("parseWindows(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Errorf("parseWindows(%q): want 0 rows; got %d", in, len(got))
		}
	}
}

func TestParseWindows_Malformed(t *testing.T) {
	t.Parallel()

	// Fewer than 6 fields: must error rather than panic.
	in := []byte("@1\t1\tsm4c\tclaude\n")
	if _, err := parseWindows(in); err == nil {
		t.Fatal("parseWindows: want error on malformed row; got nil")
	}
}

func TestIsServerNotRunning(t *testing.T) {
	t.Parallel()

	want := []string{
		"error connecting to /tmp/tmux-501/sm4c (No such file or directory)",
		"no server running on /tmp/tmux-501/sm4c",
	}
	for _, s := range want {
		if !isServerNotRunning(s) {
			t.Errorf("isServerNotRunning(%q) = false; want true", s)
		}
	}
	dontWant := []string{
		"",
		"can't find session sm4c",
		"invalid option",
	}
	for _, s := range dontWant {
		if isServerNotRunning(s) {
			t.Errorf("isServerNotRunning(%q) = true; want false", s)
		}
	}
}

func TestNewOneShot_PanicsOnRelativePath(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewOneShot: want panic on relative path; got none")
		}
	}()
	_ = NewOneShot("tmux")
}

func TestNewOneShot_PanicsOnEmptyPath(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewOneShot: want panic on empty path; got none")
		}
	}()
	_ = NewOneShot("")
}

func TestNewOneShot_Defaults(t *testing.T) {
	t.Parallel()

	o := NewOneShot("/opt/homebrew/bin/tmux")
	if o.SocketName != DefaultSocketName || o.SessionName != DefaultSessionName {
		t.Errorf("defaults not set: %+v", o)
	}
}

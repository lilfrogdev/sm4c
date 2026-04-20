package tmuxctl

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Every shEscape test case asserts the critical round-trip property:
// `sh -c "echo " + shEscape(s)` must print s followed by newline,
// bit-for-bit. If that invariant ever breaks, a claude arg containing
// `; rm -rf ~` could reach sh with the shell metacharacters live.
func TestShEscape_RoundTripsViaSh(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"simple",
		"hello world",
		"with 'single' quotes",
		`with "double" quotes`,
		"with $dollar",
		"with `backticks`",
		"with\\backslash",
		"with\ttab",
		"with\nnewline",
		"with;semicolon",
		"with|pipe",
		"with&ampersand",
		"with>redirect",
		"with*glob",
		"with~tilde",
		"with(parens)",
		"all:the,punctuation@valid+characters=work/here_ok-fine.",
		"emoji: 🚀",
		"mixed 'it\"s' $complex `nested` | chain",
	}
	for _, s := range cases {
		s := s
		t.Run(short(s), func(t *testing.T) {
			t.Parallel()
			got := execThroughSh(t, `printf %s `+shEscape(s))
			if got != s {
				t.Fatalf("shEscape round-trip mismatch:\n  input : %q\n  output: %q\n  script: %q", s, got, shEscape(s))
			}
		})
	}
}

// FuzzShEscape asserts the round-trip property for arbitrary byte
// sequences. Any failing seed should be committed to the fuzz corpus.
// NUL and C0 controls other than tab are excluded because they are
// rejected by safe.Arg long before reaching shEscape; fuzzing against
// them would catch bugs in shEscape that cannot manifest in practice.
func FuzzShEscape(f *testing.F) {
	seeds := []string{
		"",
		"a",
		"'",
		`\`,
		"$PATH",
		"'\"'",
		"end\\",
		"with\tspecial",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if !utf8IsClean(in) {
			t.Skip()
		}
		got := execThroughSh(t, `printf %s `+shEscape(in))
		if got != in {
			t.Fatalf("shEscape round-trip mismatch:\n  input : %q\n  output: %q", in, got)
		}
	})
}

// execThroughSh runs `/bin/sh -c <script>` and returns stdout. Used to
// verify that shEscape's output survives the shell-interpretation
// layer tmux performs internally on new-window commands.
func execThroughSh(t *testing.T, script string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script) // #nosec G204 -- test-only harness; script is the thing under test.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c %q failed: %v; stderr=%q", script, err, stderr.String())
	}
	return stdout.String()
}

// utf8IsClean rejects byte sequences that safe.Arg would reject. We
// use the same allowlist the production code does so the fuzzer does
// not chase unreachable states.
func utf8IsClean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 {
			return false
		}
		if c < 0x20 && c != '\t' && c != '\n' {
			return false
		}
		if c == 0x7f {
			return false
		}
	}
	return true
}

func TestIsShSafeUnquoted(t *testing.T) {
	t.Parallel()
	safe := []string{
		"abc",
		"ABC",
		"abc123",
		"/usr/local/bin/claude",
		"with-dash",
		"with_underscore",
		"path/with/slashes",
		"at@sign",
		"percent%",
		"a.b.c",
		"foo=bar",
		"plus+minus-",
		"list,of,things",
	}
	unsafe := []string{
		"has space",
		"has'quote",
		"has$dollar",
		"has*glob",
		"has~tilde",
		"has(parens",
		"has;semi",
		"has|pipe",
		"has&amp",
		"has>redirect",
		"has`backtick",
		"has\\backslash",
		"has\"double",
	}
	for _, s := range safe {
		if !isShSafeUnquoted(s) {
			t.Errorf("isShSafeUnquoted(%q) = false, want true", s)
		}
	}
	for _, s := range unsafe {
		if isShSafeUnquoted(s) {
			t.Errorf("isShSafeUnquoted(%q) = true, want false", s)
		}
	}
}

func TestBuildShellCommand_PrependsExecAndEscapes(t *testing.T) {
	t.Parallel()
	got := buildShellCommand("/usr/local/bin/claude", []string{"-p", "hello world"})
	want := `exec /usr/local/bin/claude -p 'hello world'`
	if got != want {
		t.Errorf("buildShellCommand\n  got  %q\n  want %q", got, want)
	}
}

func TestBuildShellCommand_NoArgs(t *testing.T) {
	t.Parallel()
	got := buildShellCommand("/bin/claude", nil)
	want := "exec /bin/claude"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildShellCommand_QuotesTrickyBinPath(t *testing.T) {
	t.Parallel()
	got := buildShellCommand("/opt/Application Support/claude", []string{"run"})
	want := `exec '/opt/Application Support/claude' run`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseWindowID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"@3\n", "@3", false},
		{"@12345\n", "@12345", false},
		{"@0", "@0", false},
		{"  @7  \n", "@7", false},
		{"", "", true},
		{"3\n", "", true},
		{"@abc\n", "", true},
		{"@3 extra\n", "", true},
		{"@3;rm -rf\n", "", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseWindowID([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSpawnArgs_RejectsNUL(t *testing.T) {
	t.Parallel()
	err := validateSpawnArgs("/bin/claude", []string{"ok", "bad\x00thing"})
	if err == nil {
		t.Fatal("expected error for NUL in arg, got nil")
	}
	if !strings.Contains(err.Error(), "arg 1") {
		t.Errorf("expected error to mention arg index, got %v", err)
	}
}

func TestValidateSpawnArgs_RejectsControlInBin(t *testing.T) {
	t.Parallel()
	err := validateSpawnArgs("/bin/claude\x01", nil)
	if err == nil {
		t.Fatal("expected error for control byte in claudeBin, got nil")
	}
	if !strings.Contains(err.Error(), "claudeBin") {
		t.Errorf("expected error to mention claudeBin, got %v", err)
	}
}

func TestValidateSpawnArgs_AcceptsNormalArgs(t *testing.T) {
	t.Parallel()
	if err := validateSpawnArgs("/bin/claude", []string{"-p", "hello world", "--model", "opus"}); err != nil {
		t.Errorf("unexpected error on clean args: %v", err)
	}
}

func TestAttachArgv_Shape(t *testing.T) {
	t.Parallel()
	o := OneShot{TmuxBin: "/opt/homebrew/bin/tmux", SocketName: "sm4c", SessionName: "sm4c"}
	got := o.AttachArgv("@7")
	want := []string{"/opt/homebrew/bin/tmux", "-L", "sm4c", "attach-session", "-t", "sm4c:@7"}
	if len(got) != len(want) {
		t.Fatalf("argv length: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestNameFromClaudeArgs pins the sidebar-naming behavior: when the
// user passes `-n` / `--name` to claude, sm4c pre-seeds the tmux
// window name so the sidebar reflects the requested name on the
// first frame rather than waiting for claude to emit a rename
// escape (which claude does not do reliably for the `-n` flag).
func TestNameFromClaudeArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "nil args", args: nil, want: ""},
		{name: "empty args", args: []string{}, want: ""},
		{name: "no name flag", args: []string{"/help"}, want: ""},
		{name: "-n space-separated", args: []string{"-n", "my-session"}, want: "my-session"},
		{name: "--name space-separated", args: []string{"--name", "my-session"}, want: "my-session"},
		{name: "-n equals form", args: []string{"-n=my-session"}, want: "my-session"},
		{name: "--name equals form", args: []string{"--name=my-session"}, want: "my-session"},
		{name: "-n trailing dangling", args: []string{"-n"}, want: ""},
		{name: "--name trailing dangling", args: []string{"--name"}, want: ""},
		{name: "-n after other flags", args: []string{"-c", "-n", "picked"}, want: "picked"},
		{name: "first -n wins", args: []string{"-n", "first", "-n", "second"}, want: "first"},
		{name: "-- stops scanning", args: []string{"--", "-n", "not-a-name"}, want: ""},
		{name: "name trimmed of whitespace", args: []string{"-n", "  spaced  "}, want: "spaced"},
		{name: "name stripped of escapes", args: []string{"-n", "pre\x1b[31mpost"}, want: "prepost"},
		{name: "empty name value", args: []string{"-n", ""}, want: ""},
		{name: "name equals empty", args: []string{"-n="}, want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nameFromClaudeArgs(tc.args)
			if got != tc.want {
				t.Fatalf("nameFromClaudeArgs(%v) = %q; want %q", tc.args, got, tc.want)
			}
		})
	}
}

// short trims a string for use as a test subtest name.
func short(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	if s == "" {
		return "<empty>"
	}
	return s
}

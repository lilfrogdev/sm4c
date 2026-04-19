package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRootHelp(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("root --help: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sm4c", "session manager", "--config", "--debug"} {
		if !strings.Contains(got, want) {
			t.Errorf("help output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sm4c version: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sm4c ", "commit:", "go:", "os:"} {
		if !strings.Contains(got, want) {
			t.Errorf("version output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestDoctorDefaults(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"doctor"})
	// doctor returns a non-nil error when any preflight check fails
	// (e.g. claude not installed in CI), but the config header must still
	// be printed in both cases. We therefore assert on the output, not
	// the returned error.
	_ = cmd.Execute()
	got := out.String()
	for _, want := range []string{"socket_name", "prefix_key", "monitor_silence", "checks:"} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output missing %q\nfull output:\n%s", want, got)
		}
	}
}

// TestArbitraryFirstArgDoesNotFailAsUnknownSubcommand pins the M2c
// routing decision: a positional that does not match a subcommand is
// forwarded to the launch path (and thus to claude), not rejected
// with "unknown command for sm4c". The launch path will still return
// an error in a test environment without claude installed, but the
// error must come from preflight / launch wiring — NOT from Cobra's
// "unknown command %q" template.
//
// Guarding against a regression here is important because a previous
// iteration of root.go had the default legacyArgs validator, which
// silently shadowed the documented `sm4c /help` and
// `sm4c -- -n my-session` affordances.
func TestArbitraryFirstArgDoesNotFailAsUnknownSubcommand(t *testing.T) {
	t.Parallel()

	// We force setupOneShot to fail long before it reaches tmux or
	// claude by pointing --config at a definitely-non-existent path.
	// That way the test is completely side-effect free: no tmux
	// server is contacted, no claude process is started, and the
	// syscall.Exec handoff is never considered.
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{
		"--config", "/sm4c/tests/definitely/does/not/exist.toml",
		"nope-this-does-not-exist",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected launch with missing config to fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown command") {
		t.Fatalf("launch path regressed to Cobra unknown-command error: %v", err)
	}
	// Positive check: the error must come from the config load path,
	// not from Cobra's arg validator. This also proves Cobra accepted
	// "nope-this-does-not-exist" as a forwarded positional.
	if !strings.Contains(msg, "config") {
		t.Fatalf("expected config-load error from launch path, got: %v", err)
	}
}

// TestLaunchSurfacesClaudeMissing verifies that when preflight cannot
// find claude, the launch path returns a human-readable error that
// names claude explicitly, rather than a generic wrap or (worse) a
// syscall.Exec failure from trying to attach to a nonexistent window.
//
// We simulate "claude missing" by pointing cfg.ClaudeBin at a path
// we know does not exist; preflight's validateBinary will mark it
// SevFatal and leave ClaudePath empty, which runLaunch must detect
// BEFORE it calls any tmux one-shot.
func TestLaunchSurfacesClaudeMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfgPath := dir + "/sm4c.toml"
	// 0600 satisfies config.Load's permission check (owned by current
	// uid, not group/world readable). The tmux path points at /bin/sh
	// which is guaranteed to exist and be executable on any POSIX
	// system; the claude path is deliberately bogus.
	//nolint:gosec // 0600 is the required-by-config.Load mode; tmpdir-scoped.
	if err := os.WriteFile(cfgPath, []byte(`
tmux_bin = "/bin/sh"
claude_bin = "/sm4c/tests/no/such/claude"
`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}

	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"--config", cfgPath, "/help"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected launch to fail when claude_bin is bogus")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claude") {
		t.Fatalf("error does not mention claude: %v", err)
	}
}

package cli

import (
	"bytes"
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

func TestUnknownSubcommandFails(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs([]string{"nope-this-does-not-exist"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unknown subcommand to fail")
	}
}

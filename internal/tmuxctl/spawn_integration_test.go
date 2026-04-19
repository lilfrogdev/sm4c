//go:build integration

package tmuxctl

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegration_NewClaudeWindow_CreateTagListKill exercises the full
// spawn lifecycle against a real tmux binary on an ephemeral socket.
// We use `/bin/sleep` as a stand-in for the claude binary — NewClaudeWindow
// does not care what it runs, only that the pane stays alive long
// enough for us to observe the window via ListWindows.
//
// The argv-round-trip-through-sh property is exhaustively covered at
// the unit level by FuzzShEscape / TestShEscape_RoundTripsViaSh; this
// integration test focuses on tmux-side behaviour (session bootstrap,
// tagging, rename-option scope).
//
// Run with: go test -tags=integration -count=1 -run TestIntegration_NewClaudeWindow ./internal/tmuxctl/
func TestIntegration_NewClaudeWindow_CreateTagListKill(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	socket := "sm4c-spawn-" + randSuffix(t)
	session := "sm4cspawn-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	o := OneShot{
		TmuxBin:     tmux,
		SocketName:  socket,
		SessionName: session,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First spawn should create the session + first window atomically.
	id1, err := o.NewClaudeWindow(ctx, sleep, []string{"3600"})
	if err != nil {
		t.Fatalf("NewClaudeWindow (first): %v", err)
	}
	if !strings.HasPrefix(id1, "@") {
		t.Fatalf("window id %q lacks '@' prefix", id1)
	}

	// Second spawn should attach to the existing session as a new window.
	id2, err := o.NewClaudeWindow(ctx, sleep, []string{"3600"})
	if err != nil {
		t.Fatalf("NewClaudeWindow (second): %v", err)
	}
	if id1 == id2 {
		t.Fatalf("second spawn reused window id %q", id1)
	}

	wins, err := o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	var managedIDs []string
	for _, w := range wins {
		if w.Managed() {
			managedIDs = append(managedIDs, w.ID)
		}
		if w.SessionName != session {
			t.Errorf("window %s has session %q; want %q", w.ID, w.SessionName, session)
		}
	}
	if len(managedIDs) != 2 {
		t.Fatalf("expected 2 managed windows, got %d: %+v", len(managedIDs), wins)
	}

	// Kill one and re-check.
	if err := o.killWindow(ctx, id1); err != nil {
		t.Fatalf("killWindow: %v", err)
	}
	wins, err = o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows (post-kill): %v", err)
	}
	var survivors []string
	for _, w := range wins {
		survivors = append(survivors, w.ID)
	}
	if len(survivors) != 1 || survivors[0] != id2 {
		t.Fatalf("after killing %s, survivors = %v; want [%s]", id1, survivors, id2)
	}

	// Verify the rename window options were applied at global scope
	// (per applySessionRenameOptions's -g -w semantics).
	out, err := exec.Command(tmux, "-L", socket,
		"show-options", "-g", "-w", "allow-rename").Output()
	if err != nil {
		t.Fatalf("show-options allow-rename: %v", err)
	}
	if !strings.Contains(string(out), "on") {
		t.Errorf("allow-rename not set to on: %q", out)
	}
	out, err = exec.Command(tmux, "-L", socket,
		"show-options", "-g", "-w", "automatic-rename").Output()
	if err != nil {
		t.Fatalf("show-options automatic-rename: %v", err)
	}
	if !strings.Contains(string(out), "off") {
		t.Errorf("automatic-rename not set to off: %q", out)
	}
}

// TestIntegration_NewClaudeWindow_ForwardsSimpleArgThroughShell is a
// lighter-weight verification that tmux's internal `sh -c` layer does
// not mangle an argument with spaces and a single quote. It runs a
// throwaway shell-script fake-claude that echoes its $1 into a file,
// and asserts bytes match on the way out.
//
// Exhaustive metacharacter coverage lives in FuzzShEscape; this exists
// only to catch a system-level bug (e.g. a tmux or macOS regression
// that changes how `new-window` invokes sh) that unit tests could not
// see.
func TestIntegration_NewClaudeWindow_ForwardsSimpleArgThroughShell(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-spawn-" + randSuffix(t)
	session := "sm4cspawn-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	tmpDir := t.TempDir()
	outFile := tmpDir + "/received"
	fakeClaude := tmpDir + "/fake.sh"
	body := "#!/bin/sh\nprintf '%s' \"$1\" > " + outFile + "\nsleep 60\n"
	// #nosec G306 -- test fixture requires executable bit.
	if err := os.WriteFile(fakeClaude, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	o := OneShot{
		TmuxBin:     tmux,
		SocketName:  socket,
		SessionName: session,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	payload := "hello 'quoted' world"
	if _, err := o.NewClaudeWindow(ctx, fakeClaude, []string{payload}); err != nil {
		t.Fatalf("NewClaudeWindow: %v", err)
	}

	// The fake claude writes the file synchronously on startup; poll
	// briefly to ride out tmux's async pane spawn.
	deadline := time.Now().Add(5 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got, err = os.ReadFile(outFile) // #nosec G304 -- test-controlled path.
		if err == nil && len(got) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("fake claude never wrote output: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("arg mangled:\n  sent: %q\n  got : %q", payload, got)
	}
}

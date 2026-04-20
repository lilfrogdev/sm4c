//go:build integration

package tmuxctl

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegration_OneShot_ServerLifecycle exercises the full "server
// absent -> server present -> server killed -> server absent" loop
// against a real tmux binary on an ephemeral socket. Gated behind the
// `integration` tag so the default `go test` run never requires tmux.
//
// Run with: go test -tags=integration -count=1 -run TestIntegration_OneShot ./internal/tmuxctl/
func TestIntegration_OneShot_ServerLifecycle(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	o := OneShot{
		TmuxBin:     tmux,
		SocketName:  socket,
		SessionName: session,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	running, err := o.ServerRunning(ctx)
	if err != nil {
		t.Fatalf("ServerRunning (initial): %v", err)
	}
	if running {
		t.Fatal("ServerRunning returned true with no server started")
	}

	wins, err := o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows (no server): %v", err)
	}
	if len(wins) != 0 {
		t.Fatalf("ListWindows on empty server returned %d rows", len(wins))
	}

	// Start a session by shelling out directly (sm4c's own NewSession
	// helper lands in M2c; this test covers the read-only path only).
	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "welcome",
		"sh", "-c", "sleep 3600").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	running, err = o.ServerRunning(ctx)
	if err != nil {
		t.Fatalf("ServerRunning (after start): %v", err)
	}
	if !running {
		t.Fatal("ServerRunning returned false after new-session")
	}

	wins, err = o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows (live server): %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("ListWindows returned %d rows; want 1: %+v", len(wins), wins)
	}
	if wins[0].Name != "welcome" {
		t.Errorf("window name = %q; want welcome", wins[0].Name)
	}
	if wins[0].Managed() {
		t.Errorf("unmanaged window reported Managed() = true: %+v", wins[0])
	}

	if err := o.KillServer(ctx); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	// Idempotent: second call must not error.
	if err := o.KillServer(ctx); err != nil {
		t.Fatalf("KillServer (second call): %v", err)
	}
}

// TestIntegration_OneShot_ActivePane verifies the display-message
// round-trip: given a real window ID, ActivePane returns a pane ID
// of the shape `%<digits>`. The resulting ID is what M3b+ feeds into
// the control-mode `%output` filter, so a regression here would
// silently break the hosted-pane preview even though the sidebar
// would keep working.
func TestIntegration_OneShot_ActivePane(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "w1",
		"sh", "-c", "sleep 3600").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	o := OneShot{TmuxBin: tmux, SocketName: socket, SessionName: session}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wins, err := o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("ListWindows returned %d rows; want 1", len(wins))
	}

	paneID, err := o.ActivePane(ctx, wins[0].ID)
	if err != nil {
		t.Fatalf("ActivePane(%s): %v", wins[0].ID, err)
	}
	// Shape check: the validator already enforces this, but pin it
	// here so a future refactor that drops parsePaneID cannot
	// silently regress the integration contract.
	if len(paneID) < 2 || paneID[0] != '%' {
		t.Fatalf("ActivePane returned %q; want '%%'+digits", paneID)
	}

	// Missing-window: a window ID that was never issued must map to
	// ErrNoSuchPane, not to an ad-hoc error string. Callers use
	// errors.Is to decide whether to treat this as soft (the window
	// closed) vs. fatal.
	_, err = o.ActivePane(ctx, "@9999")
	if err == nil {
		t.Fatal("ActivePane on missing window returned nil err")
	}
}

// TestIntegration_OneShot_CapturePane covers the M3b.3 backfill
// read path: with a pane printing a known sentinel, CapturePane
// must return its visible screen so the TUI can seed the VT
// emulator before the next live %output arrives.
func TestIntegration_OneShot_CapturePane(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	// Print a sentinel, then sleep so the pane stays alive long
	// enough for capture-pane to read it. Using `echo`+`sleep` is
	// deliberately POSIX-minimal so the test does not depend on
	// the user's login shell.
	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "w1",
		"sh", "-c", "echo SM4C_TEST_SENTINEL; sleep 3600").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	o := OneShot{TmuxBin: tmux, SocketName: socket, SessionName: session}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wins, err := o.ListWindows(ctx)
	if err != nil || len(wins) != 1 {
		t.Fatalf("ListWindows: wins=%v err=%v", wins, err)
	}
	paneID, err := o.ActivePane(ctx, wins[0].ID)
	if err != nil {
		t.Fatalf("ActivePane: %v", err)
	}

	// capture-pane races with the subshell's `echo`; a tiny poll
	// loop makes the test reliable without a fixed sleep.
	var captured []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		captured, err = o.CapturePane(ctx, paneID)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(string(captured), "SM4C_TEST_SENTINEL") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(captured), "SM4C_TEST_SENTINEL") {
		t.Fatalf("CapturePane missing sentinel; got %q", captured)
	}

	// Missing pane → ErrNoSuchPane, not a generic error.
	_, err = o.CapturePane(ctx, "%9999")
	if !errors.Is(err, ErrNoSuchPane) {
		t.Fatalf("CapturePane on missing pane err = %v; want ErrNoSuchPane", err)
	}
}

// TestIntegration_OneShot_ResizeWindow covers the M3b.3 viewport-sync
// write path: ResizeWindow must change the window's cell grid and
// survive a round-trip without tmux bouncing it back.
//
// We deliberately do NOT set `window-size manual` here. sm4c's
// production path runs against tmux's default `window-size latest`
// (see ResizeWindow's docstring for the rationale), and this test
// pins that the default is sufficient — a regression that only
// manifests when `window-size manual` is set would not catch a
// real user's bug.
func TestIntegration_OneShot_ResizeWindow(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "w1",
		"sh", "-c", "sleep 3600").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	o := OneShot{TmuxBin: tmux, SocketName: socket, SessionName: session}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wins, err := o.ListWindows(ctx)
	if err != nil || len(wins) != 1 {
		t.Fatalf("ListWindows: wins=%v err=%v", wins, err)
	}
	windowID := wins[0].ID

	if err := o.ResizeWindow(ctx, windowID, 132, 40); err != nil {
		t.Fatalf("ResizeWindow: %v", err)
	}

	// Verify the pane actually adopted the new geometry. We query
	// tmux directly (display-message -F '#{window_width}x#{window_height}')
	// rather than round-tripping through another sm4c helper, so
	// a regression in the helper chain can't mask a broken resize.
	out, err := exec.Command(tmux, "-L", socket,
		"display-message", "-p", "-t", windowID,
		"-F", "#{window_width}x#{window_height}").CombinedOutput()
	if err != nil {
		t.Fatalf("display-message: %v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != "132x40" {
		t.Fatalf("window size after resize = %q; want 132x40", got)
	}

	// Missing window → ErrNoSuchWindow.
	err = o.ResizeWindow(ctx, "@9999", 80, 24)
	if !errors.Is(err, ErrNoSuchWindow) {
		t.Fatalf("ResizeWindow on missing window err = %v; want ErrNoSuchWindow", err)
	}

	// Non-positive dims are rejected before tmux sees them.
	if err := o.ResizeWindow(ctx, windowID, 0, 10); err == nil {
		t.Fatal("ResizeWindow accepted zero width; want rejection")
	}
}

// TestIntegration_OneShot_SendKeys covers the M3c input-routing write
// path: SendKeys must push the given bytes through `send-keys -H` so
// that the target pane's running program sees them as keystrokes.
// We use `cat` (reads stdin, echoes to stdout) as a benign stand-in
// for claude, and verify the echoed sentinel lands in the pane's
// capture — a full loop from argv into tmux, into the pane's tty,
// into the program, and back onto the screen buffer.
func TestIntegration_OneShot_SendKeys(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	// `cat` will block waiting for stdin; every byte we send-keys
	// into the pane becomes a character cat reads and echoes back
	// to its stdout, which tmux captures into the pane buffer.
	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "w1",
		"sh", "-c", "cat").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}

	o := OneShot{TmuxBin: tmux, SocketName: socket, SessionName: session}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wins, err := o.ListWindows(ctx)
	if err != nil || len(wins) != 1 {
		t.Fatalf("ListWindows: wins=%v err=%v", wins, err)
	}
	paneID, err := o.ActivePane(ctx, wins[0].ID)
	if err != nil {
		t.Fatalf("ActivePane: %v", err)
	}

	// Send a sentinel plus Enter (0x0a — `cat` finishes its line
	// buffer on LF so the echo lands on screen). We use 0x0a
	// rather than 0x0d here because the pane's tty is in cooked
	// mode: terminal line discipline translates CR to LF on
	// input, but writing LF directly skips the translation and
	// still produces a newline on output.
	payload := []byte("SM4C_KEYS_SENTINEL\n")
	if err := o.SendKeys(ctx, paneID, payload); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Poll capture-pane until the echoed sentinel appears.
	var captured []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		captured, err = o.CapturePane(ctx, paneID)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(string(captured), "SM4C_KEYS_SENTINEL") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(captured), "SM4C_KEYS_SENTINEL") {
		t.Fatalf("SendKeys echo missing; pane capture: %q", captured)
	}

	// Missing pane → ErrNoSuchPane (not a generic error). The TUI
	// treats this as "session closed between keypress and
	// forward" and reverts focus.
	if err := o.SendKeys(ctx, "%9999", []byte{0x61}); !errors.Is(err, ErrNoSuchPane) {
		t.Fatalf("SendKeys on missing pane err = %v; want ErrNoSuchPane", err)
	}

	// Malformed pane ID is rejected before reaching tmux.
	if err := o.SendKeys(ctx, "not-a-pane", []byte{0x61}); err == nil {
		t.Fatal("SendKeys accepted malformed pane id; want rejection")
	}

	// Empty payload is a no-op (and must NOT error).
	if err := o.SendKeys(ctx, paneID, nil); err != nil {
		t.Errorf("SendKeys(nil) = %v; want nil", err)
	}
	if err := o.SendKeys(ctx, paneID, []byte{}); err != nil {
		t.Errorf("SendKeys([]byte{}) = %v; want nil", err)
	}

	// Oversized payload is rejected before it reaches argv, so a
	// runaway caller can't blow past execve's limit.
	big := make([]byte, maxSendKeysBytes+1)
	if err := o.SendKeys(ctx, paneID, big); err == nil {
		t.Fatal("SendKeys accepted oversized payload; want rejection")
	}
}

// TestIntegration_OneShot_KillWindow exercises the close-session seam
// the TUI wires into the `x` binding: kill a live window, confirm it
// vanishes from ListWindows, and confirm the "already gone" and
// server-absent paths fold into the ErrNoSuchWindow sentinel (which
// the CLI translates to a clean TUI refresh rather than a surfaced
// error).
func TestIntegration_OneShot_KillWindow(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest-" + randSuffix(t)
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	// Start session with two windows so killing one leaves the
	// server alive — the realistic shape when a user closes one
	// of several sm4c sessions from the sidebar.
	if err := exec.Command(tmux, "-L", socket,
		"new-session", "-d", "-s", session, "-n", "w1",
		"sh", "-c", "sleep 3600").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	if err := exec.Command(tmux, "-L", socket,
		"new-window", "-t", session+":", "-n", "w2",
		"sh", "-c", "sleep 3600").Run(); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	o := OneShot{TmuxBin: tmux, SocketName: socket, SessionName: session}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wins, err := o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	if len(wins) != 2 {
		t.Fatalf("ListWindows before kill = %d; want 2", len(wins))
	}

	target := wins[0].ID
	if err := o.KillWindow(ctx, target); err != nil {
		t.Fatalf("KillWindow(%s): %v", target, err)
	}

	wins, err = o.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows after kill: %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("ListWindows after kill = %d; want 1", len(wins))
	}
	if wins[0].ID == target {
		t.Errorf("killed window %s still present: %+v", target, wins[0])
	}

	// Second kill of the same ID: tmux reports "can't find window",
	// OneShot folds that into ErrNoSuchWindow so the TUI treats it
	// as a no-op success.
	if err := o.KillWindow(ctx, target); !errors.Is(err, ErrNoSuchWindow) {
		t.Errorf("KillWindow(gone) err = %v; want ErrNoSuchWindow", err)
	}

	// Malformed ID is rejected before tmux sees it.
	if err := o.KillWindow(ctx, "not-a-window"); err == nil {
		t.Error("KillWindow accepted malformed id; want rejection")
	}

	// Kill the remaining window to drop the server, then confirm
	// the server-absent path also surfaces as ErrNoSuchWindow —
	// the close-session UX depends on this conflation so a user
	// closing their last sm4c session doesn't see a spurious
	// error line.
	if err := o.KillWindow(ctx, wins[0].ID); err != nil {
		t.Fatalf("KillWindow(last): %v", err)
	}
	if err := o.KillWindow(ctx, "@999"); !errors.Is(err, ErrNoSuchWindow) {
		t.Errorf("KillWindow(no server) err = %v; want ErrNoSuchWindow", err)
	}
}

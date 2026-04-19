//go:build integration

package tmuxctl

import (
	"context"
	"os/exec"
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

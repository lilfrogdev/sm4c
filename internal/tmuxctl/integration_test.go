//go:build integration

package tmuxctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestIntegration_LiveTmuxRoundTrip spins up a real tmux on an ephemeral
// isolated socket, sends a no-op command, and asserts the response
// round-trips cleanly. Gated behind the `integration` build tag so the
// default `go test` run never requires tmux on $PATH.
//
// Run with: go test -tags=integration -count=1 -run TestIntegration ./internal/tmuxctl/
func TestIntegration_LiveTmuxRoundTrip(t *testing.T) {
	t.Parallel()

	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}

	socket := "sm4c-test-" + randSuffix(t)
	session := "sm4ctest"
	t.Cleanup(func() {
		_ = exec.Command(tmux, "-L", socket, "kill-server").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Start(ctx, ClientConfig{
		TmuxBin:     tmux,
		SocketName:  socket,
		SessionName: session,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Close()

	res, err := c.Send(ctx, "display-message -p '#{session_name}'")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.IsError {
		t.Fatalf("display-message errored: %s", res.Output)
	}
	if !strings.Contains(string(res.Output), session) {
		t.Fatalf("output does not mention session name %q: %q", session, res.Output)
	}
}

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

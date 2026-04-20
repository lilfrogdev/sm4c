package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
	"github.com/lilfrogdev/sm4c/internal/tui"
	"github.com/spf13/cobra"
)

// launch.go is the wiring for `sm4c [claude-args…]` when positional
// arguments are present. Unlike M2c, this path no longer execs into
// a tmux client — there is no code path in sm4c that hands the
// terminal to tmux directly. Instead:
//
//   1. Preflight resolves claude and tmux (fails fast on missing
//      dependencies, with a readable error from the user's shell).
//
//   2. NewClaudeWindow spawns a tagged tmux window running the
//      requested claude argv on the isolated sm4c socket.
//
//   3. We re-enter the TUI with the freshly-spawned window ID as
//      the initial highlight, so the sidebar opens already focused
//      on the session you just created.
//
// The design decision behind step 3: sm4c's whole value proposition
// is "one surface for multiple concurrent claude sessions". Letting
// the CLI dump the user into a raw tmux attach would undermine that
// — they'd be staring at a tmux prefix, not a sidebar. If they wanted
// a tmux attach, they could use tmux directly. So we route every
// entry point through the TUI.
//
// Security notes carry over from M2c:
//
//   - Claude arguments are forwarded verbatim to NewClaudeWindow,
//     which shell-escapes every element and pins tmux's default-shell
//     to /bin/sh on the sm4c socket. No user input ever reaches a
//     shell outside that escape boundary.
//
//   - We never render arbitrary argv through Printf onto the user's
//     terminal; status messages are fixed strings.
//
//   - On NewClaudeWindow failure, no window is left behind (the
//     spawn helper is atomic). On subsequent TUI failures the window
//     stays around so the user can inspect it via `sm4c ls` rather
//     than silently losing a half-initialized claude pane.

// launchTimeout bounds the two tmux round-trips we make before the
// TUI opens: one to create the window, one to tag it. Neither should
// take more than milliseconds on a healthy system; a longer wait
// indicates a stuck tmux server or a misbehaving disk.
const launchTimeout = 10 * time.Second

// runLaunch implements the `sm4c [claude-args…]` invocation. It
// spawns a new claude window and then hands off to openTUI with the
// new window ID as the initial highlight, so the TUI opens focused
// on the session the user just asked for.
func runLaunch(cmd *cobra.Command, args []string, pf *persistentFlags) error {
	o, report, cfg, err := setupOneShot(pf)
	if err != nil {
		return err
	}
	if report.ClaudePath == "" {
		return fmt.Errorf("claude is not available: %s", summarizeFatals(report))
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	spawnCtx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()

	windowID, err := spawnClaudeWindow(spawnCtx, cmd.OutOrStdout(), o, report.ClaudePath, args)
	if err != nil {
		return err
	}

	return openTUI(cmd, o, report.ClaudePath, windowID, tui.FocusPane, cfg.SessionPollInterval.AsDuration(), cfg.MonitorSilence.AsDuration())
}

// spawnClaudeWindow is the thin wrapper around NewClaudeWindow that
// runLaunch and the TUI's ActionNewSession branch share. Keeping the
// spawn mechanics in one helper means there is exactly one place in
// the codebase where argv flows from user-controlled input into
// claude, whether that input came from the shell command line or
// from the (future) TUI compose form.
//
// The freshly-created window is intentionally left in place on any
// error that happens after NewClaudeWindow returns: if a later step
// fails, the claude process inside the window has already started
// and may have modified files or emitted output. Silently killing
// it would lose that work. The user can recover manually via
// `sm4c ls` and a future close action.
func spawnClaudeWindow(ctx context.Context, out io.Writer, o tmuxctl.OneShot, claudeBin string, args []string) (string, error) {
	windowID, err := o.NewClaudeWindow(ctx, claudeBin, args)
	if err != nil {
		return "", fmt.Errorf("launch: create claude window: %w", err)
	}
	emitLaunching(out, windowID)
	return windowID, nil
}

// emitLaunching writes a single-line status message. Any write error
// on stdout is ignored — we are about to hand the terminal over to
// Bubble Tea either way, and surfacing an fprint error here would
// just mask the real failure the caller cares about.
func emitLaunching(out io.Writer, windowID string) {
	_, _ = fmt.Fprintf(out, "sm4c: spawned claude in %s\n",
		tmuxctl.DefaultSessionName+":"+windowID)
}

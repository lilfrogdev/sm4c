package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
	"github.com/spf13/cobra"
)

// launch.go is the wiring for the bare `sm4c [claude-args…]` invocation.
//
// Design:
//
//   - Preflight is mandatory and must resolve both tmux AND claude; a
//     warn-level resolution (binary found via well-known path rather
//     than $PATH) is still acceptable because we end up exec'ing tmux
//     and having tmux spawn claude by absolute path either way.
//
//   - Claude arguments are forwarded verbatim to NewClaudeWindow, which
//     handles shell-escaping and the POSIX-/bin/sh default-shell pin on
//     the sm4c socket. We never render arbitrary argv through Printf
//     onto the user's terminal; the only thing we print is the message
//     "launching claude…" which never includes user input.
//
//   - The process model at the end of a successful launch is:
//
//       sm4c -> exec -> tmux attach-session -t sm4c:@N
//
//     i.e. sm4c disappears from the process tree and the tmux client
//     owns the terminal. This matches how `sudo` and `ssh` hand off.
//     If the exec fails, we surface a clear error so the user can
//     recover (the tmux window we created stays around — they can
//     inspect it via `sm4c ls`).
//
//   - Rolling back a successful NewClaudeWindow on exec failure is
//     deliberately NOT done: if attach fails, the new claude session
//     has already started and may have already emitted a prompt /
//     modified files; killing the window silently would lose that
//     work. We let the user decide via `sm4c ls` and a future close
//     action.
//
// Testing seam:
//
//   execAttach is a package-level function variable so tests can
//   observe the argv that would have been exec'd without actually
//   replacing the test process. Production code uses execAttachReal
//   which calls syscall.Exec and never returns on success.

// execAttach is invoked to hand the terminal over to tmux. It must
// either exec (not return) or return an error describing why exec
// failed. It is a variable so test code can substitute a recorder.
var execAttach = execAttachReal

// execAttachReal replaces the current process with tmuxBin+args. On
// success it does not return. On failure it returns an error that
// has already been sanitized (exec.Exec's error comes from the
// kernel, not user input, so it is safe to surface).
//
// syscall.Exec is only available on Unix; v1 targets macOS + Linux
// (see README), so this restriction is acceptable. A future Windows
// port would need a different handoff strategy anyway because tmux
// does not run natively there.
func execAttachReal(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("launch: empty exec argv")
	}
	// #nosec G204 -- argv[0] is the preflight-validated absolute tmux
	// path; argv[1..] is sm4c-constructed (flags + window ID of the
	// form @N), never user input. See tmuxctl.OneShot.AttachArgv.
	err := syscall.Exec(argv[0], argv, os.Environ())
	// If Exec returns, it failed. The error is a syscall.Errno value
	// from the kernel; it does not include user-controlled content.
	return fmt.Errorf("launch: exec tmux failed: %w", err)
}

// launchTimeout bounds the two tmux round-trips we make before the
// exec handoff: one to create the window, one to tag it. Neither
// should take more than milliseconds on a healthy system; a longer
// wait indicates a stuck tmux server or a misbehaving disk.
const launchTimeout = 10 * time.Second

// runLaunch implements the bare `sm4c [claude-args…]` path. It is
// called from runRoot (and only from runRoot) so there is exactly one
// place in the codebase where argv flows from Cobra into claude.
func runLaunch(cmd *cobra.Command, args []string, pf *persistentFlags) error {
	o, report, _, err := setupOneShot(pf)
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
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()

	windowID, err := o.NewClaudeWindow(ctx, report.ClaudePath, args)
	if err != nil {
		return fmt.Errorf("launch: create claude window: %w", err)
	}

	// Small UX nicety: tell the user what's happening before tmux
	// paints over the terminal. We deliberately do NOT echo argv,
	// which could contain a long user prompt.
	emitLaunching(cmd.OutOrStdout(), windowID)

	argv := o.AttachArgv(windowID)
	return execAttach(argv)
}

// emitLaunching writes a single-line status message. Any write error
// on stdout is ignored — we are about to hand the terminal to tmux
// either way, and surfacing an fprint error here would just mask
// the real failure (e.g. exec errno) that the caller cares about.
func emitLaunching(out io.Writer, windowID string) {
	_, _ = fmt.Fprintf(out, "sm4c: launching claude in %s (Ctrl-b d to detach)\n",
		tmuxctl.DefaultSessionName+":"+windowID)
}

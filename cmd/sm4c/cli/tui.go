package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/lilfrogdev/sm4c/internal/tui"
	"github.com/spf13/cobra"
	xterm "golang.org/x/term"
)

// tui.go wires bare `sm4c` (no args, no subcommand) to the
// Bubble Tea empty-state view in internal/tui, and realizes any
// Action the TUI returns after the runtime exits.
//
// Why this lives in cmd/sm4c/cli and not internal/tui:
//
//   - internal/tui is deliberately side-effect-free (no tmux, no
//     subprocess, no filesystem) so it can be unit-tested as a pure
//     state machine. The realization of "the user asked for a new
//     session" is a CLI concern — same reason execAttach lives here
//     and not in the spawn package.
//
//   - Preflight is CLI-only too. Running it *before* we hand the
//     terminal to Bubble Tea means a missing tmux / claude produces
//     a readable error from the shell prompt instead of a TUI that
//     flashes for a frame and then dies. This matches the "fail fast
//     with the user's normal error channel" posture we use for
//     `sm4c ls` / `sm4c status`.

// runTUI is the entry point for the bare `sm4c` invocation. It
// performs preflight, hands the terminal to the Bubble Tea runtime,
// and on exit either returns cleanly (ActionNone) or delegates to
// spawnAndAttach (ActionNewSession, which does not return on success).
//
// Stdin MUST be a real TTY for Bubble Tea to work (it needs to put
// the terminal into raw mode). If stdin is a pipe — typical when
// sm4c is invoked from a non-interactive context like a CI runner
// or a shell script — we refuse to open the TUI and instead print a
// short pointer to the shell shortcuts. This prevents sm4c from
// hanging waiting for a keystroke that will never come.
func runTUI(cmd *cobra.Command, pf *persistentFlags) error {
	o, report, _, err := setupOneShot(pf)
	if err != nil {
		return err
	}
	if report.ClaudePath == "" {
		return fmt.Errorf("claude is not available: %s", summarizeFatals(report))
	}

	if !isInteractiveStdin() {
		// Non-interactive stdin: refuse to launch the TUI (it would
		// hang) and give the user a concrete alternative. We return
		// a compact error (the `sm4c: …` prefix is added by
		// Execute, so we do NOT include it here — doing so produces
		// "sm4c: sm4c: …" in the user's shell).
		//
		// We do not print extra lines to stderr ourselves: Execute
		// already surfaces the returned error, and adding our own
		// Fprintln above it would double-report the failure. The
		// error message is self-contained and points at both
		// recovery paths (`sm4c <prompt>` and `sm4c ls`).
		return fmt.Errorf(
			"stdin is not a TTY; refusing to open the TUI. For non-interactive use, run `sm4c <prompt>` or `sm4c ls`")
	}

	final, err := runTUIProgram(cmd)
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}

	switch final.Action() {
	case tui.ActionNone:
		// Clean exit. Bubble Tea already restored the terminal; we
		// have nothing more to do.
		return nil

	case tui.ActionNewSession:
		// The user asked for a new session from the empty-state
		// view. For M2c this is equivalent to the shell shortcut
		// `sm4c` (no args) under the previous behavior — bare
		// claude, no name, cwd inherited from the current process.
		// M3 replaces this branch with the real "pick target
		// folder + name" flow the user described; until then the
		// stopgap keeps the TUI useful rather than a dead-end.
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, launchTimeout)
		defer cancel()
		return spawnAndAttach(ctx, cmd.OutOrStdout(), o, report.ClaudePath, nil)
	}

	// Defensive: an Action we don't know how to realize should never
	// escape the TUI in a released binary, but if a future Action
	// slips past without a switch arm here, we fail loudly rather
	// than silently returning.
	return fmt.Errorf("tui: unhandled action %v", final.Action())
}

// runTUIProgram is a seam so tests can stub the Bubble Tea runtime
// without having a real TTY. Production code calls tui.Run with the
// process's stdin/stdout; tests substitute an in-memory pair.
var runTUIProgram = runTUIProgramReal

func runTUIProgramReal(cmd *cobra.Command) (tui.Model, error) {
	// Bubble Tea wants the actual process stdin/stdout to drive
	// raw-mode input and terminal-level escape sequences. cmd.InOrStdin
	// and cmd.OutOrStdout return interfaces that are os.Stdin /
	// os.Stdout in production (Cobra's defaults) and test buffers in
	// unit tests — the latter don't support raw mode, which is why
	// the TUI path has runTUIProgram as a seam and tests substitute
	// it rather than trying to drive tui.Run against a bytes.Buffer.
	return tui.Run(
		asReader(cmd.InOrStdin()),
		asWriter(cmd.OutOrStdout()),
	)
}

// isInteractiveStdin reports whether the process's standard input
// is a terminal. The check uses golang.org/x/term, which we already
// pull in for `sm4c stop`'s confirmation prompt, so no new dep.
// We deliberately check os.Stdin (not cmd.InOrStdin) because
// Bubble Tea always reads from the process stdin in production;
// test paths bypass this function entirely by stubbing runTUIProgram.
func isInteractiveStdin() bool {
	// #nosec G115 -- file descriptors on Linux/macOS fit in int
	// comfortably; this is the same cast we use in cmd/sm4c/cli/stop.go.
	return xterm.IsTerminal(int(os.Stdin.Fd()))
}

// asReader / asWriter coerce Cobra's io.Reader / io.Writer
// interfaces down to the concrete types internal/tui.Run accepts.
// tui.Run takes structurally-typed interfaces (not io.Reader /
// io.Writer by name) to emphasize that the package is not doing
// anything beyond Read / Write — no seek, no close, no ReadAt.
// These two trampolines exist only so callers using io.Reader /
// io.Writer at the boundary don't need type gymnastics.
func asReader(r io.Reader) interface {
	Read(p []byte) (n int, err error)
} {
	return r
}

func asWriter(w io.Writer) interface {
	Write(p []byte) (n int, err error)
} {
	return w
}

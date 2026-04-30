// Package cli wires the sm4c command surface on top of spf13/cobra.
//
// Design rules enforced here and in sibling files:
//
//   - sm4c owns only a small set of flags: --config, --debug, --help,
//     --version. Every other flag and positional argument is forwarded to
//     the hosted `claude` subprocess by the TUI entry point (M2+).
//   - Subcommands (`ls`, `status`, `stop`, `doctor`, `version`) are
//     read-mostly. There is intentionally no `sm4c new`, `sm4c kill`, or
//     `sm4c rename` — session lifecycle happens inside the TUI or inside
//     claude itself.
//   - Nothing in this package performs I/O at import time. All work is
//     gated behind RunE so tests can exercise the command tree without
//     side effects.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Build metadata injected via -ldflags at release time. Defaults reflect
// "built from a dev checkout". See Makefile for the release values.
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// persistentFlags holds the globally-scoped options. Kept on a struct (not
// package-level vars) so that unit tests can build fresh command trees.
type persistentFlags struct {
	configPath string
	debug      bool
}

// Execute builds the sm4c command tree, runs it with args, and returns a
// shell-style exit code. main() is the only caller; tests should construct
// a fresh tree via newRootCmd and call cmd.Execute directly.
func Execute(args []string) int {
	cmd := newRootCmd(os.Stdout, os.Stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	// cobra prints its own usage on flag errors; we print other errors
	// ourselves so we can control formatting and avoid leaking sensitive
	// content from wrapped errors. Writes to stderr cannot meaningfully
	// fail here: if they do, the process is exiting anyway.
	var flagErr *cobraFlagError
	if !errors.As(err, &flagErr) {
		_, _ = fmt.Fprintf(os.Stderr, "sm4c: %v\n", err)
	}
	return 1
}

// newRootCmd constructs an isolated command tree. Stdout/stderr are
// injected so tests can capture them.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	pf := &persistentFlags{}
	cmd := &cobra.Command{
		Use:   "sm4c [claude-args...]",
		Short: "Session manager for the Claude Code CLI",
		Long: `sm4c is an unofficial TUI session manager that sits on top of the official
Claude Code CLI. It hosts concurrent claude sessions on an isolated tmux
server and surfaces per-session status.

sm4c is not affiliated with Anthropic. You must install the official claude
CLI separately; sm4c does not call Anthropic APIs directly.

Running sm4c with no arguments opens the TUI. If no managed sessions
exist yet, you land on the empty-state view; once one or more
sessions are live, the TUI shows a sidebar with a live preview of
the highlighted session on the right. Use ` + "`j`" + `/` + "`k`" + `
(or arrows) to move the highlight, ` + "`n`" + ` to start a new
session, ` + "`?`" + ` for the full keymap, and ` + "`q`" + ` or
` + "`Ctrl+C`" + ` to exit.

Running sm4c with positional arguments spawns a new claude session
with those arguments AND opens the TUI focused on that new session,
so ` + "`sm4c /help`" + ` and ` + "`sm4c -- -n my-session`" + ` both work.
Use ` + "`--`" + ` before any dash-prefixed flags you want claude to
receive; otherwise Cobra will try to parse them as sm4c flags.

sm4c never exec-attaches into tmux. Every interaction happens inside
the TUI; if you want a raw tmux attach, use tmux directly.

Subcommands (ls, status, stop, doctor, version) are read-mostly and
never spawn a claude session.`,
		Example: `  sm4c                           # open the TUI (empty-state or sidebar)
  sm4c /help                     # spawn a session with /help as first input, then open the TUI focused on it
  sm4c -- -n my-session          # spawn with claude flags, then open the TUI focused on it
  sm4c ls                        # list managed sessions
  sm4c doctor                    # preflight self-check`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// ArbitraryArgs disables Cobra's default "unknown command"
		// error for any positional that doesn't match a registered
		// subcommand. Combined with SilenceUsage this means a typo
		// like `sm4c lss` is handed to claude rather than rejected
		// with a usage dump — which is consistent with the documented
		// "everything after sm4c's own flags goes to claude" model.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRoot(cmd, args, pf)
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetVersionTemplate("{{.Use}} {{.Version}}\n")

	cmd.PersistentFlags().StringVar(&pf.configPath, "config", "",
		"path to sm4c config file (TOML). Must be owned by the current user and not group- or world-writable.")
	cmd.PersistentFlags().BoolVar(&pf.debug, "debug", false,
		"enable debug logging to stderr. May log potentially sensitive diagnostic info; see SECURITY.md.")

	cmd.AddCommand(
		newVersionCmd(),
		newDoctorCmd(pf),
		newLsCmd(pf),
		newStatusCmd(pf),
		newStopCmd(pf),
		newNewCmd(pf),
	)
	return cmd
}

// runRoot dispatches the two TUI-entry modes:
//
//   - No positional args  -> runTUI: open the TUI with no initial
//     highlight hint. If managed sessions already exist the sidebar
//     lands on the first row; otherwise the user sees the empty
//     state and can press `n` to spawn the first session.
//
//   - One or more args    -> runLaunch: preflight, spawn a tagged
//     claude window with the args forwarded verbatim, then open
//     the TUI with that new window pre-selected. Unlike M2c, this
//     path never exec-attaches into tmux — it simply routes the
//     user into the TUI already focused on the session they asked
//     for, so `sm4c /help` is "one command, everything is on
//     screen" without any raw-tmux detour.
//
// Subcommands never reach runRoot — Cobra dispatches them directly.
func runRoot(cmd *cobra.Command, args []string, pf *persistentFlags) error {
	if len(args) == 0 {
		return runTUI(cmd, pf)
	}
	return runLaunch(cmd, args, pf)
}

// cobraFlagError is a sentinel type used by Execute to decide whether to
// print an error prefix. Kept here so main stays small.
type cobraFlagError struct{ err error }

func (e *cobraFlagError) Error() string { return e.err.Error() }
func (e *cobraFlagError) Unwrap() error { return e.err }

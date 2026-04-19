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
		Use:   "sm4c",
		Short: "Session manager for the Claude Code CLI",
		Long: `sm4c is an unofficial TUI session manager that sits on top of the official
Claude Code CLI. It hosts multiple concurrent claude sessions on an isolated
tmux server and surfaces per-session status in a sidebar.

sm4c is not affiliated with Anthropic. You must install the official claude
CLI separately; sm4c does not call Anthropic APIs directly.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
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
	)
	return cmd
}

// runRoot is the bare `sm4c` invocation. In M0 this is a placeholder that
// explains what is not yet implemented. M1 will replace it with the TUI
// launcher.
func runRoot(cmd *cobra.Command, args []string, pf *persistentFlags) error {
	_ = args
	_ = pf
	cmd.Println("sm4c: TUI is not implemented yet (M1 milestone).")
	cmd.Println("Run `sm4c --help` to see available subcommands, or `sm4c doctor` for a self-check.")
	return nil
}

// cobraFlagError is a sentinel type used by Execute to decide whether to
// print an error prefix. Kept here so main stays small.
type cobraFlagError struct{ err error }

func (e *cobraFlagError) Error() string { return e.err.Error() }
func (e *cobraFlagError) Unwrap() error { return e.err }

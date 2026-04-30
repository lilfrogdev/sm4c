package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newFlags are the per-invocation flags for `sm4c new`.
type newFlags struct {
	name string
	dir  string
}

// newNewCmd implements `sm4c new`: spawns a new sm4c-managed claude window
// without opening the TUI. The window appears in the sidebar within the
// next poll tick, making it possible for one claude session to programmatically
// start a sibling session.
func newNewCmd(pf *persistentFlags) *cobra.Command {
	f := &newFlags{}
	cmd := &cobra.Command{
		Use:   "new [claude-args...]",
		Short: "Spawn a new claude session without opening the TUI",
		Long: `new spawns a new sm4c-managed claude window and exits immediately.

The new window appears in the sm4c sidebar within the next poll tick (~1 s).
This command is designed to be called from inside an existing claude session
(via a shell tool) to open a sibling session for a different folder or task.

Flags:
  -n, --name NAME   Session name: sets the tmux window name and passes
                    -n NAME to claude so the session is named consistently.
                    Omit if you are already passing -n in [claude-args].
  -d, --dir  DIR    Working directory for the new session. Defaults to
                    the calling pane's current directory when omitted.

Any additional positional arguments are forwarded verbatim to claude.

Examples:

  # Open a new session named "feature-x" in a specific folder
  sm4c new --name feature-x --dir ~/projects/feature-x

  # Same, short flags
  sm4c new -n feature-x -d ~/projects/feature-x

  # Pass extra claude flags after --
  sm4c new -n refactor -d /path/to/repo -- --model claude-opus-4-7`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNew(cmd, args, pf, f)
		},
	}
	cmd.Flags().StringVarP(&f.name, "name", "n", "", "session name (sets tmux window name and passes -n to claude)")
	cmd.Flags().StringVarP(&f.dir, "dir", "d", "", "working directory for the new session")
	return cmd
}

// runNew implements the `sm4c new` invocation. It spawns a new claude window
// on the existing sm4c socket and exits — no TUI is opened. The sidebar
// discovers the window on the next session poll.
func runNew(cmd *cobra.Command, args []string, pf *persistentFlags, f *newFlags) error {
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
	spawnCtx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()

	fifoPath := filepath.Join(os.TempDir(), "sm4c-"+o.SocketName+"-hooks.fifo")
	o.GlobalEnv = map[string]string{"SM4C_HOOK_FIFO": fifoPath}

	// Prepend -n <name> to claude's args when --name is given and the
	// caller hasn't already supplied their own -n / --name flag. This
	// keeps the tmux window name and claude's internal session name in
	// sync without requiring the user to specify the flag twice.
	claudeArgs := args
	if f.name != "" && !claudeArgsHasName(args) {
		claudeArgs = append([]string{"-n", f.name}, args...)
	}

	_, err = spawnClaudeWindow(spawnCtx, cmd.OutOrStdout(), o, report.ClaudePath, claudeArgs, f.dir)
	return err
}

// claudeArgsHasName reports whether args already contains a -n / --name flag
// so runNew can skip injecting a duplicate.
func claudeArgsHasName(args []string) bool {
	for i, a := range args {
		if a == "--" {
			return false
		}
		if (a == "-n" || a == "--name") && i+1 < len(args) {
			return true
		}
		if strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "--name=") {
			return true
		}
	}
	return false
}

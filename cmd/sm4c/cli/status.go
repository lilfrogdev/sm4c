package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

// newStatusCmd implements `sm4c status`: a single-screen summary of the
// sm4c tmux server state. It is strictly read-only and cannot start or
// stop a server.
//
// The output is plain, deliberately unstable text — scripts should use
// `sm4c ls --json` for structured querying.
func newStatusCmd(pf *persistentFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sm4c tmux server status",
		Long: `status prints a one-screen summary:

  - resolved paths and versions of tmux and claude
  - whether the sm4c tmux server is running
  - number of managed vs unmanaged windows

status never starts the tmux server or modifies any state. If you want
structured, scriptable output, use ` + "`sm4c ls --json`" + ` instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), pf)
		},
	}
}

func runStatus(ctx context.Context, out io.Writer, pf *persistentFlags) error {
	o, report, cfg, err := setupOneShot(pf)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// fprint errors here mean "caller's stdout pipe broke"; the process
	// is exiting next anyway, so the errcheck warnings are suppressed
	// explicitly rather than plumbed upward.
	_, _ = fmt.Fprintln(out, "sm4c status")
	_, _ = fmt.Fprintf(out, "  tmux_bin       : %s\n", report.TmuxPath)
	_, _ = fmt.Fprintf(out, "  tmux_version   : %s\n", orDefault(report.TmuxVersion, "<unknown>"))
	_, _ = fmt.Fprintf(out, "  claude_bin     : %s\n", orDefault(report.ClaudePath, "<not found>"))
	_, _ = fmt.Fprintf(out, "  socket_name    : %s\n", cfg.SocketName)

	running, err := o.ServerRunning(ctx)
	if err != nil {
		return fmt.Errorf("status: probe server: %w", err)
	}
	if !running {
		_, _ = fmt.Fprintln(out, "  server         : not running")
		return nil
	}
	_, _ = fmt.Fprintln(out, "  server         : running")

	wins, err := o.ListWindows(ctx)
	if err != nil {
		return fmt.Errorf("status: list windows: %w", err)
	}
	managed, unmanaged := partitionWindows(wins)
	_, _ = fmt.Fprintf(out, "  managed        : %d\n", len(managed))
	_, _ = fmt.Fprintf(out, "  unmanaged      : %d\n", len(unmanaged))
	return nil
}

package cli

import (
	"fmt"
	"io"

	"github.com/lilfrogdev/sm4c/internal/config"
	"github.com/spf13/cobra"
)

// newDoctorCmd runs sm4c's environment self-check. In M0 this is a stub
// that exercises config loading and reports the defaults. M1 will add
// tmux/claude version probing, socket-directory perm checks, and a
// summary of the active FSM thresholds.
func newDoctorCmd(stdout, stderr io.Writer, pf *persistentFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run environment and security self-checks",
		Long: `doctor verifies the local environment sm4c will run in. It reports:
  - the resolved sm4c config (defaults or from --config)
  - (M1) tmux and claude binary paths + versions
  - (M1) socket directory ownership and permissions
  - (M1) active monitor thresholds

doctor never touches the live tmux server and never writes to the filesystem
outside of transient os.Stat calls.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if out == nil {
				out = stdout
			}
			_ = stderr

			cfg, err := config.Load(pf.configPath)
			if err != nil {
				return fmt.Errorf("doctor: load config: %w", err)
			}

			fmt.Fprintln(out, "sm4c doctor (M0 skeleton)")
			fmt.Fprintf(out, "  config path    : %s\n", orDefault(pf.configPath, "<built-in defaults>"))
			fmt.Fprintf(out, "  socket_name    : %s\n", cfg.SocketName)
			fmt.Fprintf(out, "  prefix_key     : %s\n", cfg.PrefixKey)
			fmt.Fprintf(out, "  monitor_silence: %s\n", cfg.MonitorSilence)
			fmt.Fprintf(out, "  log_level      : %s\n", cfg.LogLevel)
			fmt.Fprintf(out, "  tmux_bin       : %s\n", orDefault(cfg.TmuxBin, "<resolve via PATH>"))
			fmt.Fprintf(out, "  claude_bin     : %s\n", orDefault(cfg.ClaudeBin, "<resolve via PATH>"))
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "NOTE: full tmux/claude preflight is not implemented until M1.")
			return nil
		},
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

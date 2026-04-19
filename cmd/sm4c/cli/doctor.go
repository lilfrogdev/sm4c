package cli

import (
	"fmt"

	"github.com/lilfrogdev/sm4c/internal/config"
	"github.com/spf13/cobra"
)

// newDoctorCmd runs sm4c's environment self-check. It reports the resolved
// config, then runs the shared Preflight routine and prints each finding
// with its severity. The process exits non-zero iff any finding is
// SevFatal, so CI and shell scripts can gate on `sm4c doctor`.
func newDoctorCmd(pf *persistentFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run environment and security self-checks",
		Long: `doctor verifies the local environment sm4c will run in. It reports:
  - the resolved sm4c config (defaults or from --config)
  - the absolute paths of tmux and claude, resolved via $PATH or config
  - the detected tmux version (must be >= 3.2)
  - ownership and permissions of the tmux socket directory

doctor never touches the live tmux server and never writes to the filesystem
outside of transient os.Stat calls and a `+"`tmux -V`"+` probe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(pf.configPath)
			if err != nil {
				return fmt.Errorf("doctor: load config: %w", err)
			}

			cmd.Println("sm4c doctor")
			cmd.Printf("  config path    : %s\n", orDefault(pf.configPath, "<built-in defaults>"))
			cmd.Printf("  socket_name    : %s\n", cfg.SocketName)
			cmd.Printf("  prefix_key     : %s\n", cfg.PrefixKey)
			cmd.Printf("  monitor_silence: %s\n", cfg.MonitorSilence)
			cmd.Printf("  log_level      : %s\n", cfg.LogLevel)
			cmd.Printf("  tmux_bin       : %s\n", orDefault(cfg.TmuxBin, "<resolve via PATH>"))
			cmd.Printf("  claude_bin     : %s\n", orDefault(cfg.ClaudeBin, "<resolve via PATH>"))
			cmd.Println("")

			report := Preflight(cfg)
			cmd.Println("checks:")
			for _, f := range report.Findings {
				cmd.Printf("  [%-5s] %-20s %s\n", f.Severity, f.Check, f.Message)
				if f.Detail != "" {
					cmd.Printf("          %s\n", f.Detail)
				}
			}
			if report.Fatal() {
				return fmt.Errorf("doctor: one or more fatal checks failed")
			}
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

package cli

import (
	"runtime"

	"github.com/spf13/cobra"
)

// newVersionCmd prints version and build-provenance info. Intentionally
// self-contained: does not read config, does not spawn subprocesses,
// cannot fail. Writes go through cmd.Print* which target cmd.OutOrStdout
// and swallow write errors by design — a CLI that can't write to its own
// output has nothing useful to report anyway.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print sm4c version and build info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.Printf("sm4c %s\n", Version)
			cmd.Printf("  commit: %s\n", Commit)
			cmd.Printf("  built:  %s\n", Date)
			cmd.Printf("  go:     %s\n", runtime.Version())
			cmd.Printf("  os:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"
)

// newVersionCmd prints version and build-provenance info. Intentionally
// self-contained: does not read config, does not spawn subprocesses,
// cannot fail.
func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print sm4c version and build info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			if w == nil {
				w = stdout
			}
			fmt.Fprintf(w, "sm4c %s\n", Version)
			fmt.Fprintf(w, "  commit: %s\n", Commit)
			fmt.Fprintf(w, "  built:  %s\n", Date)
			fmt.Fprintf(w, "  go:     %s\n", runtime.Version())
			fmt.Fprintf(w, "  os:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

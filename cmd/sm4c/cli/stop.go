package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// stopFlags are the per-invocation flags for `sm4c stop`.
type stopFlags struct {
	force bool
}

// newStopCmd implements `sm4c stop`: tears down the sm4c tmux server
// and every session it hosts. This is a destructive operation — every
// unsaved claude conversation on the socket dies with it — so it
// requires either an interactive TTY confirmation or `--force`.
//
// The idempotency contract: stopping an already-stopped server is a
// silent success (exit 0, "no server running").
func newStopCmd(pf *persistentFlags) *cobra.Command {
	f := &stopFlags{}
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the sm4c tmux server (destructive)",
		Long: `stop terminates the sm4c tmux server and every window it hosts,
including all running claude processes. There is no "close session"
analogue in this command — it's all or nothing.

Unless --force is passed, stop requires an interactive TTY and the
user must type 'stop' to confirm. A non-TTY stdin without --force
aborts with an error, so scripts that intend to run this command
unattended must pass --force explicitly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop(cmd.Context(), cmd.OutOrStdout(), os.Stdin, pf, f)
		},
	}
	cmd.Flags().BoolVar(&f.force, "force", false, "skip the interactive confirmation prompt")
	return cmd
}

// errStopAborted is returned when the user rejects the confirmation or
// a non-TTY stdin is detected without --force.
var errStopAborted = errors.New("stop aborted by user")

func runStop(ctx context.Context, out io.Writer, stdin io.Reader, pf *persistentFlags, f *stopFlags) error {
	o, _, _, err := setupOneShot(pf)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	running, err := o.ServerRunning(ctx)
	if err != nil {
		return fmt.Errorf("stop: probe server: %w", err)
	}
	if !running {
		_, _ = fmt.Fprintln(out, "sm4c: no server running; nothing to stop")
		return nil
	}

	if !f.force {
		if !stdinIsTTY(stdin) {
			return fmt.Errorf("stop: stdin is not a terminal; re-run with --force to stop non-interactively")
		}
		if err := confirmStop(out, stdin); err != nil {
			return err
		}
	}

	if err := o.KillServer(ctx); err != nil {
		return fmt.Errorf("stop: kill server: %w", err)
	}
	_, _ = fmt.Fprintln(out, "sm4c: server stopped")
	return nil
}

// confirmStop prompts the user for a typed confirmation. We require an
// exact "stop" match (not y/yes) because destroying active claude
// sessions deserves the friction of a word.
func confirmStop(out io.Writer, stdin io.Reader) error {
	_, _ = fmt.Fprintln(out, "This will stop the sm4c tmux server and every window it hosts.")
	_, _ = fmt.Fprint(out, "Type 'stop' to confirm: ")
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return errStopAborted
	}
	if strings.TrimSpace(scanner.Text()) != "stop" {
		return errStopAborted
	}
	return nil
}

// stdinIsTTY reports whether stdin is connected to a terminal. The
// test-injected `io.Reader` path returns false for anything that is
// not an *os.File pointing at a TTY, which is the conservative answer
// for any non-interactive context.
func stdinIsTTY(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	// f.Fd() is a uintptr; term.IsTerminal takes an int. On every
	// platform sm4c supports (macOS, Linux), a file descriptor is a
	// small non-negative integer that fits in an int32, so this
	// narrowing conversion is safe. Suppressing gosec G115 because the
	// domain is bounded by the kernel, not by user input.
	fd := int(f.Fd()) // #nosec G115 -- kernel fds fit in int on POSIX
	return term.IsTerminal(fd)
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lilfrogdev/sm4c/internal/tmuxctl"
	"github.com/spf13/cobra"
)

// lsFlags are the per-invocation flags for `sm4c ls`. Kept on a struct
// so tests can construct fresh command trees without touching global
// state.
type lsFlags struct {
	all  bool
	json bool
}

// newLsCmd implements `sm4c ls`. The command is read-only; it never
// creates, mutates, or kills anything in tmux. If the server is not
// running, it prints an empty list and exits 0.
//
// Ownership: by default the output is restricted to windows tagged
// with tmuxctl.KindKey=tmuxctl.KindClaude — the invariant that any
// window we render was created by sm4c. `--all` reveals untagged
// ("Unmanaged") windows as a separate muted section so a user can see
// rogue windows on the sm4c socket without blurring the ownership
// invariant.
func newLsCmd(pf *persistentFlags) *cobra.Command {
	f := &lsFlags{}
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sm4c sessions",
		Long: `ls prints every managed session on the sm4c tmux server, in tmux's
native ordering. A managed session is any tmux window tagged with
` + "`@sm4c-kind=claude`" + ` — the tag sm4c sets on every window it creates.

By default, unmanaged windows (anything created outside sm4c, e.g. by
running ` + "`tmux -L sm4c new-window bash`" + ` by hand) are hidden. Pass --all
to reveal them in a separate read-only section.

ls never starts a tmux server; if no server is running, output is empty
and the exit code is 0. Output contract: --json produces a stable,
machine-parseable document suitable for scripting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLs(cmd.Context(), cmd.OutOrStdout(), pf, f)
		},
	}
	cmd.Flags().BoolVar(&f.all, "all", false, "also show unmanaged windows (not created by sm4c)")
	cmd.Flags().BoolVar(&f.json, "json", false, "emit a JSON document instead of a human table")
	return cmd
}

// lsJSON is the stable output contract for `sm4c ls --json`. New fields
// may be added; existing fields must remain.
type lsJSON struct {
	ServerRunning bool           `json:"server_running"`
	Managed       []lsJSONWindow `json:"managed"`
	Unmanaged     []lsJSONWindow `json:"unmanaged,omitempty"`
}

type lsJSONWindow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Session string `json:"session"`
	Active  bool   `json:"active"`
	Flags   string `json:"flags"`
	Kind    string `json:"kind,omitempty"`
}

func runLs(ctx context.Context, out io.Writer, pf *persistentFlags, f *lsFlags) error {
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
		return fmt.Errorf("ls: probe server: %w", err)
	}

	var wins []tmuxctl.Window
	if running {
		wins, err = o.ListWindows(ctx)
		if err != nil {
			return fmt.Errorf("ls: list windows: %w", err)
		}
	}

	managed, unmanaged := partitionWindows(wins)

	if f.json {
		return emitLsJSON(out, running, managed, unmanaged, f.all)
	}
	return emitLsText(out, running, managed, unmanaged, f.all)
}

// partitionWindows splits windows by ownership tag. Callers must never
// treat unmanaged windows as if sm4c created them — rendering an
// untagged window's `window_name` is still safe because parseWindows
// sanitizes every name, but writing to an untagged window would break
// the "sm4c only mutates what it tagged" invariant.
func partitionWindows(wins []tmuxctl.Window) (managed, unmanaged []tmuxctl.Window) {
	for _, w := range wins {
		if w.Managed() {
			managed = append(managed, w)
		} else {
			unmanaged = append(unmanaged, w)
		}
	}
	return managed, unmanaged
}

func emitLsText(out io.Writer, running bool, managed, unmanaged []tmuxctl.Window, all bool) error {
	// fprint errors here mean "caller's stdout pipe broke" (e.g. the
	// user's pager exited). The process is about to return to the
	// shell anyway, so the errcheck lint is suppressed with the usual
	// `_, _ =` dance rather than propagated upward.
	if !running {
		_, _ = fmt.Fprintln(out, "sm4c: tmux server not running; no sessions")
		return nil
	}
	if len(managed) == 0 {
		_, _ = fmt.Fprintln(out, "sm4c: no managed sessions")
	} else {
		_, _ = fmt.Fprintln(out, "Sessions:")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  ID\tNAME\tFLAGS\tACTIVE")
		for _, w := range managed {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				w.ID,
				dashIfEmpty(w.Name),
				dashIfEmpty(w.Flags),
				boolLabel(w.Active),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if !all {
		if len(unmanaged) > 0 {
			_, _ = fmt.Fprintf(out, "\n(%d unmanaged window(s) hidden; pass --all to show)\n", len(unmanaged))
		}
		return nil
	}

	if len(unmanaged) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(out, "\nUnmanaged windows (not created by sm4c; read-only):")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  ID\tNAME\tSESSION\tFLAGS")
	for _, w := range unmanaged {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			w.ID,
			dashIfEmpty(w.Name),
			dashIfEmpty(w.SessionName),
			dashIfEmpty(w.Flags),
		)
	}
	return tw.Flush()
}

func emitLsJSON(out io.Writer, running bool, managed, unmanaged []tmuxctl.Window, all bool) error {
	doc := lsJSON{
		ServerRunning: running,
		Managed:       windowsToJSON(managed),
	}
	if all {
		doc.Unmanaged = windowsToJSON(unmanaged)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func windowsToJSON(ws []tmuxctl.Window) []lsJSONWindow {
	if len(ws) == 0 {
		return []lsJSONWindow{}
	}
	out := make([]lsJSONWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, lsJSONWindow{
			ID:      w.ID,
			Name:    w.Name,
			Session: w.SessionName,
			Active:  w.Active,
			Flags:   w.Flags,
			Kind:    w.Kind,
		})
	}
	return out
}

func dashIfEmpty(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

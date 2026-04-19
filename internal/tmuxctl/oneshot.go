package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/lilfrogdev/sm4c/internal/safe"
)

// One-shot tmux invocations (as opposed to the long-lived control-mode
// Client) are the transport shared by every CLI subcommand and by the
// TUI's lifecycle operations. They deliberately do NOT use the `-CC`
// channel — a one-shot command on a fresh socket is faster, simpler,
// and fails closed on any permission issue.
//
// Every one-shot is serialized through runTmux, which enforces:
//
//   - the tmux path is absolute (the caller is expected to resolve it
//     via preflight); no $PATH lookup happens inside this package
//   - stdin is closed so tmux cannot inherit the parent's tty
//   - the context's deadline bounds the call (default 5s if none set)
//   - stderr is captured, bounded, and sanitized via safe.Line before
//     surfacing in any returned error — tmux error text reaches the
//     user terminal, and sm4c refuses to forward raw bytes from
//     subprocesses

// Well-known user-option tags that sm4c writes on every window it
// creates. These turn a hostile tmux socket (someone ran
// `tmux -L sm4c new-window bash` by hand) into a non-issue for the
// sidebar and the CLI: unless the window has KindKey=KindClaude it is
// treated as Unmanaged and hidden by default.
//
// These are user-options (leading '@') per tmux(1). They persist for
// the lifetime of the window.
const (
	KindKey    = "@sm4c-kind"
	KindClaude = "claude"
)

// DefaultSocketName is the socket passed to `tmux -L <name>` for every
// sm4c invocation. It is a deliberately fixed value so that two
// concurrent sm4c processes share the same backend server — that's the
// whole point of running on an isolated socket.
const DefaultSocketName = "sm4c"

// DefaultSessionName is the tmux session sm4c owns inside its isolated
// server. Every window we create lives under this session.
const DefaultSessionName = "sm4c"

// ErrServerNotRunning is returned when a tmux one-shot fails because the
// server is simply not up (the socket file does not exist). Callers
// typically treat this as "zero sessions" rather than an error.
var ErrServerNotRunning = errors.New("tmuxctl: tmux server not running on sm4c socket")

// defaultTimeout bounds any individual tmux one-shot. It is intentionally
// short — list-windows on a live server returns in milliseconds, and a
// hang here would block the CLI on a user's terminal.
const defaultTimeout = 5 * time.Second

// OneShot is the handle for running one-shot tmux commands on the sm4c
// socket. Callers construct it from a preflight-resolved tmux path and
// reuse it across calls.
type OneShot struct {
	TmuxBin     string // absolute path; validated by preflight
	SocketName  string // defaults to DefaultSocketName
	SessionName string // defaults to DefaultSessionName
}

// NewOneShot constructs a OneShot with default socket / session names.
// tmuxBin must be absolute; passing a relative path panics because that
// would mean the caller skipped preflight, which is a programmer error.
func NewOneShot(tmuxBin string) OneShot {
	if tmuxBin == "" {
		panic("tmuxctl: NewOneShot: tmuxBin must be set (preflight first)")
	}
	if !strings.HasPrefix(tmuxBin, "/") {
		panic("tmuxctl: NewOneShot: tmuxBin must be absolute (preflight first): " + tmuxBin)
	}
	return OneShot{
		TmuxBin:     tmuxBin,
		SocketName:  DefaultSocketName,
		SessionName: DefaultSessionName,
	}
}

// run executes `tmux -L <socket> <args...>` and returns stdout. stderr
// is captured up to a small bound and sanitized before being folded
// into the returned error. run never invokes a shell and never sets
// tmux through a wrapper: `exec.Command` splits argv itself.
func (o OneShot) run(ctx context.Context, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}

	full := append([]string{"-L", o.SocketName}, args...)
	cmd := exec.CommandContext(ctx, o.TmuxBin, full...) // #nosec G204 -- tmuxBin is absolute + preflight-validated; args are callers' statically-known tmux arguments
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	// Cap stderr at 8 KiB so a runaway tmux cannot exhaust memory via
	// the error path. That is plenty for any realistic error line.
	cmd.Stderr = limitWriter(&stderr, 8*1024)

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}

	stderrStr := safe.Line(stderr.String())
	// Classify "no server" without matching a localized string literally
	// — tmux's error message contains "error connecting to" on every
	// platform we support, and only in this case.
	if isServerNotRunning(stderrStr) {
		return nil, ErrServerNotRunning
	}

	// Any other failure: wrap the exit error with the sanitized stderr.
	return stdout.Bytes(), fmt.Errorf("tmuxctl: tmux %s failed: %w; stderr=%q",
		strings.Join(args, " "), err, stderrStr)
}

// isServerNotRunning recognizes the stderr tmux prints when the client
// cannot connect to the socket. The literal "error connecting to" is
// stable across tmux 2.x–3.6 (it's the only code path in tmux's
// client.c that logs at that level with that prefix).
func isServerNotRunning(stderr string) bool {
	return strings.Contains(stderr, "error connecting to") ||
		strings.Contains(stderr, "no server running")
}

// ServerRunning returns true if a tmux server is currently listening on
// the sm4c socket. This never spawns a server; it runs
// `tmux -L <socket> has-session -t <session>` and interprets the exit
// status + stderr. A non-running server is reported as (false, nil)
// rather than an error.
func (o OneShot) ServerRunning(ctx context.Context) (bool, error) {
	_, err := o.run(ctx, "has-session", "-t", o.SessionName)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrServerNotRunning) {
		return false, nil
	}
	// `has-session` against an existing server with a missing target
	// exits 1 with "can't find session" on stderr — that's still "server
	// running, just no sm4c session yet".
	if strings.Contains(err.Error(), "can't find session") {
		return true, nil
	}
	return false, err
}

// Window is the projection of a tmux window that sm4c cares about. Name
// is always sanitized via safe.Label before being stored here, so
// callers can render it without further escaping. Kind is the value of
// KindKey; empty means the window was not created by sm4c.
type Window struct {
	ID          string // e.g. "@3"
	Name        string // sanitized
	Flags       string // raw flag string (subset of '*+-!#~M'), no untrusted bytes
	Active      bool
	SessionName string
	Kind        string // e.g. KindClaude, or "" for unmanaged
}

// Managed reports whether this window was created by sm4c (tagged with
// KindKey=KindClaude). Untagged windows are rendered as Unmanaged and
// are read-only from sm4c's perspective.
func (w Window) Managed() bool { return w.Kind == KindClaude }

// listWindowsFormat is the tmux format string we pass to list-windows.
// The untrusted free-form field (window_name) is placed LAST so that any
// tab or stray byte in the name is absorbed into the remainder by the
// splitter below instead of corrupting field boundaries. Every other
// field has a restricted charset:
//
//   - #{window_id}       : "@" + decimal digits
//   - #{window_active}   : "0" or "1"
//   - #{session_name}    : controlled by sm4c ("sm4c")
//   - #{@sm4c-kind}      : controlled by sm4c ("" or "claude")
//   - #{window_flags}    : subset of "*+-!#~M"
//   - #{window_name}     : untrusted
const listWindowsFormat = "#{window_id}\t#{window_active}\t#{session_name}\t#{@sm4c-kind}\t#{window_flags}\t#{window_name}"

// ListWindows returns every window visible on the sm4c tmux server, in
// tmux's native ordering. If the server is not running this returns
// (nil, nil) — not an error — because "no server" maps to "no sessions"
// for the callers in this package.
func (o OneShot) ListWindows(ctx context.Context) ([]Window, error) {
	out, err := o.run(ctx, "list-windows", "-a", "-F", listWindowsFormat)
	if errors.Is(err, ErrServerNotRunning) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseWindows(out)
}

// parseWindows is split out so that golden tests (and eventually a
// fixture-driven test harness) can exercise the splitter without
// spawning tmux.
func parseWindows(out []byte) ([]Window, error) {
	var wins []Window
	for _, rawLine := range bytes.Split(out, []byte{'\n'}) {
		if len(rawLine) == 0 {
			continue
		}
		// Splitn with N=6 so that any tabs present in the free-form
		// window_name stay bundled into the final field.
		parts := strings.SplitN(string(rawLine), "\t", 6)
		if len(parts) != 6 {
			return nil, fmt.Errorf("tmuxctl: malformed list-windows row: %q", safe.Line(string(rawLine)))
		}
		w := Window{
			ID:          parts[0],
			SessionName: parts[2],
			Kind:        parts[3],
			Flags:       parts[4],
			Name:        safe.Label(parts[5]),
			Active:      parts[1] == "1",
		}
		wins = append(wins, w)
	}
	return wins, nil
}

// KillServer terminates the sm4c tmux server. It returns nil if the
// server was already gone (idempotent teardown is the only sensible
// behavior for a `sm4c stop`).
func (o OneShot) KillServer(ctx context.Context) error {
	_, err := o.run(ctx, "kill-server")
	if errors.Is(err, ErrServerNotRunning) {
		return nil
	}
	return err
}

// SessionExists returns true iff the sm4c session is present on a live
// server. The difference from ServerRunning is that a live server with
// no sm4c session returns (false, nil) here.
func (o OneShot) SessionExists(ctx context.Context) (bool, error) {
	_, err := o.run(ctx, "has-session", "-t", o.SessionName)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrServerNotRunning) {
		return false, nil
	}
	if strings.Contains(err.Error(), "can't find session") {
		return false, nil
	}
	return false, err
}

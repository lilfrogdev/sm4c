package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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

	// CurrentPath is the active pane's working directory as tmux
	// reports it via `#{pane_current_path}`. Empty when tmux
	// could not resolve one (e.g. the pane's process has exited
	// between list-windows row generation and field expansion).
	// Sanitized via safe.Line so it is safe to render directly.
	// Used by the TUI sidebar to show a "where am I working?"
	// hint under each session name, in the same spirit as
	// claude-squad and Cursor's grouped-by-repo sidebar.
	CurrentPath string

	// PaneTitle is the terminal title last set by the pane's process via
	// OSC 0 or OSC 2 escape sequences, as captured by tmux in
	// `#{pane_title}`. Claude Code writes its session/project name here
	// on startup and on context changes. When non-empty this takes
	// precedence over Name in the sidebar so sessions show their
	// Claude-assigned label rather than the static tmux window name.
	// Sanitized via safe.Label before storage.
	PaneTitle string

	// PaneCount is the number of panes currently open in this window
	// (#{window_panes}). Used to detect when an editor split has been
	// closed: if the TUI has an editor pane registered for a window but
	// PaneCount drops to 1, the editor pane is gone.
	PaneCount int
}

// Managed reports whether this window was created by sm4c (tagged with
// KindKey=KindClaude). Untagged windows are rendered as Unmanaged and
// are read-only from sm4c's perspective.
func (w Window) Managed() bool { return w.Kind == KindClaude }

// listWindowsFormat is the tmux format string we pass to list-windows.
// The untrusted free-form field (window_name) is placed LAST so that any
// tab or stray byte in the name is absorbed into the remainder by the
// splitter below instead of corrupting field boundaries. Every other
// field has a restricted charset — except #{pane_current_path}, which
// is a filesystem path and CAN in principle contain a tab character.
// In practice tabs in paths are vanishingly rare (no mainstream
// package or repo convention uses them) and our defensive parser
// tolerates the degenerate case: an extra tab in the path would shift
// the tail into window_name's slot and safe.Label/safe.Line would
// strip the embedded control byte on sanitization, producing a
// slightly garbled display rather than corrupting structural state.
// Sanitization + the trailing free-form slot together keep this
// safe-by-construction without adding a dedicated escape dance.
//
//   - #{window_id}           : "@" + decimal digits
//   - #{window_active}       : "0" or "1"
//   - #{session_name}        : controlled by sm4c ("sm4c")
//   - #{@sm4c-kind}          : controlled by sm4c ("" or "claude")
//   - #{window_flags}        : subset of "*+-!#~M"
//   - #{pane_current_path}   : active pane's cwd, absolute path
//   - #{pane_title}          : last OSC 0/2 title set by the pane, untrusted
//   - #{window_name}         : untrusted, must be last (free-form field)
const listWindowsFormat = "#{window_id}\t#{window_active}\t#{session_name}\t#{@sm4c-kind}\t#{window_flags}\t#{pane_current_path}\t#{pane_title}\t#{window_panes}\t#{window_name}"

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
		// Splitn with N=9 so that any tabs present in the free-form
		// window_name (last field) stay bundled into the final slot.
		// pane_title (second-to-last) is also untrusted but rarely
		// contains tabs in practice; a tab would truncate the title
		// at the tab boundary after safe.Label strips the control byte,
		// which is an acceptable cosmetic degradation. See
		// listWindowsFormat for the slot order.
		parts := strings.SplitN(string(rawLine), "\t", 9)
		if len(parts) != 9 {
			return nil, fmt.Errorf("tmuxctl: malformed list-windows row: %q", safe.Line(string(rawLine)))
		}
		paneCount, _ := strconv.Atoi(parts[7])
		w := Window{
			ID:          parts[0],
			SessionName: parts[2],
			Kind:        parts[3],
			Flags:       parts[4],
			CurrentPath: safe.Line(parts[5]),
			PaneTitle:   safe.Label(parts[6]),
			PaneCount:   paneCount,
			Name:        safe.Label(parts[8]),
			Active:      parts[1] == "1",
		}
		wins = append(wins, w)
	}
	return wins, nil
}

// ActivePane returns the tmux pane ID of the active pane inside the
// given window. The returned value is an opaque tmux token like "%4";
// it is validated to match `%` + decimal digits before being returned,
// so callers can pass it to tmux control-mode output filters without
// re-sanitizing.
//
// A missing server or a missing window returns ErrNoSuchPane — the
// TUI translates that into "(pane preview unavailable)" rather than
// surfacing a fatal error, because windows can legitimately be closed
// by the user between the moment we decided to resolve the pane and
// the moment tmux answered.
func (o OneShot) ActivePane(ctx context.Context, windowID string) (string, error) {
	if err := safe.Arg(windowID); err != nil {
		return "", fmt.Errorf("tmuxctl: ActivePane: windowID: %w", err)
	}
	// display-message -p prints the formatted value to stdout; -t
	// scopes the lookup to the window; the format string is a single
	// tmux variable so the entire response is the pane ID plus a
	// trailing newline. We deliberately do NOT use list-panes here:
	// display-message returns exactly one value, which keeps the
	// parse trivial.
	out, err := o.run(ctx, "display-message", "-p", "-t", windowID, "-F", "#{pane_id}")
	if errors.Is(err, ErrServerNotRunning) {
		return "", ErrNoSuchPane
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find window") ||
			strings.Contains(low, "can't find pane") ||
			strings.Contains(low, "window not found") {
			return "", ErrNoSuchPane
		}
		return "", err
	}
	return parsePaneID(out)
}

// parsePaneID validates a single-line tmux pane ID (`%` + decimal
// digits). Factored out so a unit test can exercise the happy and
// malformed paths without needing a live tmux server; the integration
// test in oneshot_integration_test.go pins the real round-trip.
func parsePaneID(out []byte) (string, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("tmuxctl: empty pane id from tmux")
	}
	if !strings.HasPrefix(s, "%") || len(s) < 2 {
		return "", fmt.Errorf("tmuxctl: malformed pane id %q (want '%%' + digits)", safe.Line(s))
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("tmuxctl: malformed pane id %q (non-digit after '%%')", safe.Line(s))
		}
	}
	return s, nil
}

// ErrNoSuchPane is returned by ActivePane when the target window does
// not exist (closed by the user, never existed) or the tmux server is
// not running. Callers should treat this as a soft failure — the
// window may simply have been closed between the list-windows call
// and the active-pane lookup.
var ErrNoSuchPane = errors.New("tmuxctl: no such pane")

// ErrNoSuchWindow is the window-scoped counterpart to ErrNoSuchPane:
// the tmux server is reachable but the requested window ID does not
// exist. ResizeWindow (and any future window-addressed one-shot)
// returns this sentinel so callers can treat "window closed between
// list and op" as a soft, recoverable outcome rather than a fatal
// error. Missing server is also mapped here: from the caller's
// point of view, both cases mean "the target is gone".
var ErrNoSuchWindow = errors.New("tmuxctl: no such window")

// CapturePane reads the currently-visible screen of a tmux pane and
// returns it as raw bytes with ANSI escape sequences preserved (the
// `-e` flag). This is the initial-screen payload the TUI seeds into
// its VT emulator on first resolve so switching to a session shows
// its current state immediately, instead of waiting for the next
// keystroke to trigger a fresh %output.
//
// paneID must be in the `%<digits>` shape (the same format ActivePane
// returns). We validate before passing it to tmux so a malformed or
// empty ID never becomes a shell-like argument to capture-pane.
//
// A missing pane or a missing server is returned as ErrNoSuchPane,
// matching ActivePane's soft-failure semantics: the TUI treats both
// as "skip backfill, fall through to live bytes". Any other tmux
// error is wrapped verbatim (with stderr sanitized).
func (o OneShot) CapturePane(ctx context.Context, paneID string) ([]byte, error) {
	if err := safe.Arg(paneID); err != nil {
		return nil, fmt.Errorf("tmuxctl: CapturePane: paneID: %w", err)
	}
	if _, err := parsePaneID([]byte(paneID)); err != nil {
		return nil, fmt.Errorf("tmuxctl: CapturePane: %w", err)
	}
	// -p prints to stdout, -e preserves ANSI escape sequences,
	// -t scopes to the pane, -S -N starts N lines above the visible
	// screen (i.e., includes the scrollback buffer).
	//
	// Capturing scrollback lets the TUI's in-process emulator
	// replay the full session history so the user can scroll back
	// past the point where sm4c was started. Without -S the emulator
	// only ever holds the visible screen on first connect, meaning
	// sm4c sessions that restart mid-session lose all prior history.
	//
	// 10 000 matches the in-process emulator's DefaultScrollbackSize.
	// Tmux clamps the start automatically if its own history-limit
	// is smaller, so overshooting is safe. Lines older than 10 000
	// are evicted from the emulator's scrollback as replay fills it.
	out, err := o.run(ctx, "capture-pane", "-p", "-e", "-t", paneID, "-S", "-10000")
	if errors.Is(err, ErrServerNotRunning) {
		return nil, ErrNoSuchPane
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find pane") ||
			strings.Contains(low, "can't find window") ||
			strings.Contains(low, "pane not found") ||
			strings.Contains(low, "window not found") {
			return nil, ErrNoSuchPane
		}
		return nil, err
	}
	return out, nil
}

// ResizeWindow tells tmux to resize the given window to width x height
// cells. This is how the TUI keeps each claude pane sized to the
// right-pane viewport: whenever the emulator's body dimensions change
// (terminal resize, highlight moved to a different window that may
// have drifted), sm4c issues one ResizeWindow so wrapping and cursor
// positioning stay honest.
//
// windowID must be in the `@<digits>` shape. width and height are
// cells (columns × rows) and must both be positive; zero or negative
// is rejected before reaching tmux because resize-window accepts them
// silently but produces a useless 1x1 pane.
//
// A missing window or missing server returns ErrNoSuchWindow — the
// caller treats this as "session closed between highlight and
// resize", logs nothing, and moves on. This mirrors ActivePane's
// soft-failure posture and keeps a single closed session from
// blowing up the TUI.
//
// Note on `window-size`: sm4c does NOT pin `window-size manual` on
// its tmux session. On tmux 3.6a that option tears down a
// detached-only server (sm4c's steady state — the sm4c process
// never attaches), with "server exited unexpectedly", in every
// configuration we tested. Empirically `resize-window` already
// sticks with the default `window-size latest` for both standalone
// and control-mode-attached servers, so we leave the option at its
// default and rely on this call being the authoritative source of
// pane geometry.
func (o OneShot) ResizeWindow(ctx context.Context, windowID string, width, height int) error {
	if err := safe.Arg(windowID); err != nil {
		return fmt.Errorf("tmuxctl: ResizeWindow: windowID: %w", err)
	}
	if _, err := parseWindowID([]byte(windowID)); err != nil {
		return fmt.Errorf("tmuxctl: ResizeWindow: %w", err)
	}
	if width < 1 || height < 1 {
		return fmt.Errorf("tmuxctl: ResizeWindow: non-positive dims %dx%d", width, height)
	}
	_, err := o.run(ctx,
		"resize-window",
		"-t", windowID,
		"-x", intToDecimal(width),
		"-y", intToDecimal(height),
	)
	if errors.Is(err, ErrServerNotRunning) {
		return ErrNoSuchWindow
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find window") ||
			strings.Contains(low, "window not found") ||
			strings.Contains(low, "can't find session") {
			return ErrNoSuchWindow
		}
		return err
	}
	return nil
}

// SendKeys forwards raw input bytes into a tmux pane via
// `send-keys -H`. `-H` is tmux's "hex bytes" mode: every argument
// after the target is a two-digit hex representation of a single
// byte, which tmux pushes into the pane's input stream exactly as
// given, WITHOUT re-interpreting named keys like "Enter" or "C-c".
//
// This is the right semantics for M3c input routing: sm4c already
// translated the user's tea.KeyMsg into the exact byte sequence a
// terminal would emit (UTF-8 runes, 0x0d for Enter, 0x03 for
// Ctrl+C, CSI escapes for arrow keys, …). Letting tmux's named-key
// table remap any of that would introduce an extra layer of
// interpretation and break claude's expectation of raw terminal
// input.
//
// Validation:
//
//   - paneID must match `%<digits>` (the same shape ActivePane
//     returns). We reject anything else before it reaches tmux so
//     no user-controlled string flows into argv.
//   - data is bounded to maxSendKeysBytes (4 KiB). A single
//     keypress is a handful of bytes; 4 KiB is a generous ceiling
//     that still catches a runaway call. Larger payloads would
//     also blow past execve's argv length limit (hex-encoded
//     bytes are 3-arguments-per-byte in the worst case).
//
// Missing server / missing pane map to ErrNoSuchPane so callers
// can treat "session closed between keypress and forward" as a
// soft outcome — the TUI reverts focus to the sidebar on that
// signal and the next sessions poll prunes the stale highlight.
func (o OneShot) SendKeys(ctx context.Context, paneID string, data []byte) error {
	if err := safe.Arg(paneID); err != nil {
		return fmt.Errorf("tmuxctl: SendKeys: paneID: %w", err)
	}
	if _, err := parsePaneID([]byte(paneID)); err != nil {
		return fmt.Errorf("tmuxctl: SendKeys: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxSendKeysBytes {
		return fmt.Errorf("tmuxctl: SendKeys: payload too large: %d > %d", len(data), maxSendKeysBytes)
	}

	args := make([]string, 0, 4+len(data))
	args = append(args, "send-keys", "-t", paneID, "-H")
	for _, b := range data {
		args = append(args, hexByte(b))
	}
	_, err := o.run(ctx, args...)
	if errors.Is(err, ErrServerNotRunning) {
		return ErrNoSuchPane
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find pane") ||
			strings.Contains(low, "can't find window") ||
			strings.Contains(low, "pane not found") ||
			strings.Contains(low, "window not found") {
			return ErrNoSuchPane
		}
		return err
	}
	return nil
}

// maxSendKeysBytes bounds a single SendKeys payload. In practice a
// keypress is 1–4 bytes (UTF-8 rune, short CSI escape); the limit
// is far above that so we never truncate a real keystroke, but
// small enough to keep a buggy caller from generating a
// multi-megabyte argv.
const maxSendKeysBytes = 4 * 1024

// hexByte formats b as a lowercase two-digit hex string. Used to
// build the per-byte arguments to `send-keys -H`. Factored out
// instead of using fmt.Sprintf("%02x", b) to avoid the format-
// string interpreter for what is a completely fixed shape.
func hexByte(b byte) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[b>>4], hex[b&0x0f]})
}

// intToDecimal renders a non-negative int as its base-10 string
// without pulling strconv into this file (the rest of the package
// avoids strconv for parity with the intToString helper in
// internal/tui). The caller guarantees n >= 0 (ResizeWindow
// rejects negatives before this point); a defensive 0 fallback
// keeps the function well-defined outside that contract.
func intToDecimal(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
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

// SetGlobalEnv sets a global tmux environment variable on the sm4c socket.
// It is used to propagate SM4C_HOOK_FIFO so that Claude Code lifecycle hooks
// running inside managed panes know where to write their events.
// Best-effort: callers should ignore errors when the server is not yet running.
func (o OneShot) SetGlobalEnv(ctx context.Context, key, value string) error {
	if err := safe.Arg(key); err != nil {
		return fmt.Errorf("tmuxctl: SetGlobalEnv: key %q: %w", key, err)
	}
	_, err := o.run(ctx, "set-environment", "-g", key, value)
	return err
}

// SplitWindow creates a vertical split (side-by-side panes) inside the
// given window, starts editorBin in the new right pane at cwd, and
// returns the new pane ID (e.g. "%7").
//
// The split uses `-h` (horizontal split in tmux terminology) which places
// the new pane to the right of the active pane. The editor binary is run
// via `exec <editorBin> .` so the pane closes when the editor exits.
//
// cwd must be a valid directory path; empty cwd inherits the pane's
// current directory. editorBin must be an absolute path or a resolvable
// command name validated by the caller before SplitWindow is invoked.
func (o OneShot) SplitWindow(ctx context.Context, windowID, cwd, editorBin string) (string, error) {
	if err := safe.Arg(windowID); err != nil {
		return "", fmt.Errorf("tmuxctl: SplitWindow: windowID: %w", err)
	}
	if !strings.HasPrefix(windowID, "@") {
		return "", fmt.Errorf("tmuxctl: SplitWindow: windowID %q is not in @N form", safe.Line(windowID))
	}
	if err := safe.Arg(editorBin); err != nil {
		return "", fmt.Errorf("tmuxctl: SplitWindow: editorBin: %w", err)
	}
	shellCmd := "exec " + shEscape(editorBin) + " ."

	args := []string{"split-window", "-h", "-t", windowID, "-P", "-F", "#{pane_id}"}
	if cwd != "" {
		if err := safe.Arg(cwd); err == nil {
			args = append(args, "-c", cwd)
		}
	}
	args = append(args, shellCmd)

	out, err := o.run(ctx, args...)
	if errors.Is(err, ErrServerNotRunning) {
		return "", ErrNoSuchWindow
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find window") ||
			strings.Contains(low, "window not found") {
			return "", ErrNoSuchWindow
		}
		return "", err
	}
	return parsePaneID(out)
}

// KillPane terminates the tmux pane identified by paneID. This is used
// to close the editor split pane when the user navigates away or closes
// the editor from sm4c's side. If the pane is already gone (closed by
// the editor process exiting) this returns ErrNoSuchPane — callers
// should treat that as a successful outcome.
func (o OneShot) KillPane(ctx context.Context, paneID string) error {
	if err := safe.Arg(paneID); err != nil {
		return fmt.Errorf("tmuxctl: KillPane: paneID: %w", err)
	}
	if _, err := parsePaneID([]byte(paneID)); err != nil {
		return fmt.Errorf("tmuxctl: KillPane: %w", err)
	}
	_, err := o.run(ctx, "kill-pane", "-t", paneID)
	if errors.Is(err, ErrServerNotRunning) {
		return ErrNoSuchPane
	}
	if err != nil {
		low := err.Error()
		if strings.Contains(low, "can't find pane") ||
			strings.Contains(low, "pane not found") {
			return ErrNoSuchPane
		}
		return err
	}
	return nil
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

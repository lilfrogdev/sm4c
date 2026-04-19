package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lilfrogdev/sm4c/internal/safe"
)

// Spawn.go is the tmux-side of sm4c's launch path: it takes a resolved
// claude binary and a slice of claude arguments and produces a single
// tagged tmux window ready to be attached to.
//
// Security notes (read these before editing):
//
//   - tmux's `new-session` / `new-window` commands accept a SHELL command,
//     not an argv. Whatever we pass is handed to `/bin/sh -c` on the
//     other side. That means arbitrary claude args cannot be forwarded
//     naively — a stray `; rm -rf ~` or ` ` + unbalanced quote would run.
//     We shell-escape every single argv element with POSIX
//     single-quoting (see shEscape) so there is exactly one layer of sh
//     interpretation and the interpreted result is bit-identical to the
//     original argv.
//
//   - The claude binary path is also shell-escaped. Preflight guarantees
//     it is an absolute path to an executable regular file, but a
//     filename containing a space or quote must still round-trip
//     through sh without mutation.
//
//   - We prefix the shell command with `exec ` so the shell gets out of
//     the way and claude becomes PID 1 of the tmux pane. This is purely
//     ergonomic (one less process in `ps`, and tmux's `pane_dead_status`
//     reflects claude's exit directly) but has zero security impact.
//
//   - Every arg is additionally run through safe.Arg before escaping,
//     rejecting NUL and C0 controls (except tab). Those would not be
//     unsafe per se after single-quoting — POSIX sh preserves any byte
//     inside single quotes — but they indicate malformed input and we
//     prefer to fail fast than to silently forward something weird into
//     a pane that tmux's control-mode parser will later see echoed in
//     `%output` notifications.

// ErrSpawnNoClaudeBin is returned when NewClaudeWindow is called with
// an empty claudeBin. This always indicates a programmer error (the
// caller forgot to run preflight).
var ErrSpawnNoClaudeBin = errors.New("tmuxctl: NewClaudeWindow: claudeBin is empty (preflight first)")

// NewClaudeWindow spawns a new tmux window running the given claude
// binary with the given arguments, tagged with @sm4c-kind=claude, and
// returns the assigned window ID (e.g. "@3").
//
// If the sm4c tmux session does not yet exist on the socket, it is
// created atomically with this window as its first window. Otherwise
// the window is appended to the existing session.
//
// Every call reapplies the server-wide default-shell (pinned to
// /bin/sh, see posixShell) and the window rename options; these
// set-options are chained into the same tmux CLI invocation as the
// new-session / new-window command so the first window is ALWAYS
// spawned with a POSIX-compliant shell, regardless of the user's
// login shell. This closes a real bug: on machines where the user's
// login shell is nushell, fish, or elvish, tmux's default-shell
// inherits that non-POSIX shell at server start, and our POSIX-escaped
// `exec claude …` command strings are then mangled by the wrong
// parser (nushell in particular preserves `'\''` literally).
//
// On any failure after the window is created, the window is killed
// (best-effort) so the socket doesn't end up with an untagged "half
// managed" window that sm4c would later render as Unmanaged.
func (o OneShot) NewClaudeWindow(ctx context.Context, claudeBin string, args []string) (string, error) {
	if claudeBin == "" {
		return "", ErrSpawnNoClaudeBin
	}
	if err := validateSpawnArgs(claudeBin, args); err != nil {
		return "", err
	}

	cmdline := buildShellCommand(claudeBin, args)

	// Common case (session exists) is one tmux call; fall back to a
	// slightly longer chain only if the server reports the session is
	// missing.
	windowID, err := o.newWindowOnExistingSession(ctx, cmdline)
	if err != nil && errors.Is(err, errNoSuchSession) {
		windowID, err = o.newSessionWithCommand(ctx, cmdline)
	}
	if err != nil {
		return "", err
	}

	// Tagging is a separate round-trip: tmux does not let us set a
	// window-scoped user-option inside the same chain as new-window
	// because we do not know the window ID until new-window returns.
	// The micro-race between "window created untagged" and "window
	// tagged" is internal to this process and bounded to ~1ms; sm4c
	// list paths treat untagged windows as unmanaged (not dangerous).
	if err := o.setWindowOption(ctx, windowID, KindKey, KindClaude); err != nil {
		// Best-effort cleanup: a cancelled caller context must not
		// prevent us from reaping the orphan window.
		_ = o.killWindow(context.Background(), windowID)
		return "", fmt.Errorf("tag window %s: %w", windowID, err)
	}
	return windowID, nil
}

// validateSpawnArgs rejects claudeBin or any argument that contains a
// byte which is either unsafe as subprocess input (NUL) or would
// corrupt tmux's line-oriented parser if it reached tmux's control mode
// via `%output`.
func validateSpawnArgs(claudeBin string, args []string) error {
	if err := safe.Arg(claudeBin); err != nil {
		return fmt.Errorf("tmuxctl: claudeBin rejected: %w", err)
	}
	for i, a := range args {
		if err := safe.Arg(a); err != nil {
			return fmt.Errorf("tmuxctl: claude arg %d rejected: %w", i, err)
		}
	}
	return nil
}

// errNoSuchSession is the internal sentinel for "tmux server is up but
// the sm4c session does not exist on it". It is returned by
// newWindowOnExistingSession so NewClaudeWindow can fall through to
// creating the session.
var errNoSuchSession = errors.New("tmuxctl: sm4c session does not exist")

// newWindowOnExistingSession chains `set-option -g default-shell
// /bin/sh \; new-window …` in one tmux invocation and returns the
// newly-allocated window ID. Reapplying default-shell on every spawn
// is defensive: it costs essentially nothing and protects against a
// third party (e.g. a user attaching to our socket and running
// `set-option -g default-shell`) breaking our escape invariant.
//
// Two failure modes are mapped back to sentinels so the caller can
// branch cleanly:
//
//   - The server is not running at all → errNoSuchSession (the chain
//     cannot even start).
//   - The server is running but the session is missing → errNoSuchSession.
func (o OneShot) newWindowOnExistingSession(ctx context.Context, cmdline string) (string, error) {
	out, err := o.run(ctx,
		"set-option", "-g", "default-shell", posixShell,
		";",
		"new-window",
		"-t", o.SessionName+":",
		"-d",
		"-P",
		"-F", "#{window_id}",
		cmdline,
	)
	if err != nil {
		if errors.Is(err, ErrServerNotRunning) {
			return "", errNoSuchSession
		}
		if strings.Contains(err.Error(), "can't find session") ||
			strings.Contains(err.Error(), "session not found") {
			return "", errNoSuchSession
		}
		return "", err
	}
	return parseWindowID(out)
}

// newSessionWithCommand creates the sm4c session, atomically making
// the given command its first window, and returns the window ID.
//
// Every server-wide option sm4c depends on is chained into the SAME
// tmux CLI invocation and sequenced BEFORE new-session, so the first
// window is spawned with the correct default-shell / rename behavior.
// This is the only chain order that avoids the "first window uses the
// user's login shell" bug on nushell/fish/elvish systems.
//
// We use `-d` (detached) because we never want tmux to steal the
// current TTY — the caller `exec`s into `attach-session` after
// tagging, by which point it is safe for tmux to own the terminal.
//
// The `-s <session>` and `-n <name>` names are sm4c-controlled, NOT
// user input. The window-name starts as "claude" and is expected to
// be updated by claude's own title-writing (OSC 0/2) shortly after
// startup; sm4c does not rename windows itself at any point.
func (o OneShot) newSessionWithCommand(ctx context.Context, cmdline string) (string, error) {
	out, err := o.run(ctx,
		"set-option", "-g", "default-shell", posixShell,
		";",
		"set-option", "-g", "-w", "allow-rename", "on",
		";",
		"set-option", "-g", "-w", "automatic-rename", "off",
		";",
		"new-session",
		"-d",
		"-s", o.SessionName,
		"-n", "claude",
		"-P",
		"-F", "#{window_id}",
		cmdline,
	)
	if err != nil {
		return "", fmt.Errorf("new-session: %w", err)
	}
	return parseWindowID(out)
}

// parseWindowID extracts a single `@N` window ID from tmux stdout. tmux
// prints the formatted value followed by a trailing newline. We reject
// any ID that does not match the expected shape.
func parseWindowID(out []byte) (string, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("tmuxctl: empty window id from tmux")
	}
	if !strings.HasPrefix(s, "@") {
		return "", fmt.Errorf("tmuxctl: malformed window id %q (missing '@')", safe.Line(s))
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("tmuxctl: malformed window id %q (non-digit after '@')", safe.Line(s))
		}
	}
	return s, nil
}

// posixShell is the shell tmux is forced to use for interpreting the
// sm4c-generated `exec claude …` command strings. We deliberately pin
// this to /bin/sh rather than inheriting the user's login shell because
// buildShellCommand emits POSIX single-quote escapes (the
// '…'\''…'-style trick), which non-POSIX shells like nushell, fish,
// and elvish do NOT parse the same way. Letting tmux hand our
// escaped command to, e.g., nushell results in `'it'\''s'` being
// preserved literally instead of becoming `it's` — a silent arg-
// corruption bug that the unit-level fuzzer cannot see (it invokes
// `/bin/sh -c` directly, not through tmux).
//
// Using /bin/sh also narrows the attack surface: claude receives its
// argv through a well-understood POSIX shell, not through whatever
// custom parser the user happens to prefer for their interactive
// terminal.
const posixShell = "/bin/sh"

// setWindowOption sets a tmux window-level user option via
// `set-option -w`. The `-w` flag scopes the option to that specific
// window (not the session or server), which is what we want for the
// @sm4c-kind tag — each window carries its own ownership marker.
func (o OneShot) setWindowOption(ctx context.Context, windowID, key, value string) error {
	_, err := o.run(ctx,
		"set-option",
		"-w",
		"-t", windowID,
		key,
		value,
	)
	return err
}

// killWindow tears down a window by ID. Used on spawn rollback; the
// caller is expected to ignore the error (best-effort cleanup).
func (o OneShot) killWindow(ctx context.Context, windowID string) error {
	_, err := o.run(ctx, "kill-window", "-t", windowID)
	return err
}

// buildShellCommand constructs the string passed to tmux
// new-window / new-session, which tmux hands to `/bin/sh -c`. Every
// argv element is shell-escaped via shEscape; the final command is:
//
//	exec <claude> <arg1> <arg2> ...
//
// `exec` replaces the shell with claude, so the tmux pane's root
// process is claude itself. If claude exits, the pane closes
// immediately, which is what we want for sm4c's lifecycle model.
func buildShellCommand(claudeBin string, args []string) string {
	var b strings.Builder
	b.Grow(len(claudeBin) + 16 + len(args)*16)
	b.WriteString("exec ")
	b.WriteString(shEscape(claudeBin))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shEscape(a))
	}
	return b.String()
}

// shEscape returns s wrapped in POSIX single-quotes such that `sh -c`
// reconstructs the original byte sequence exactly. Single quotes are
// the only POSIX-sh quoting form that disables ALL interpretation
// (including $var, backticks, backslash escapes, and history
// expansion), so they are the right tool for transporting untrusted
// argv through a shell layer.
//
// The only tricky character inside single quotes is the single quote
// itself, which has no escape within single-quoted strings. The
// standard workaround ends the quoted segment, emits a backslash-
// escaped single quote, and restarts the segment: 'it'\''s'. We apply
// that transformation here.
//
// Inputs that consist entirely of POSIX-safe characters are returned
// unquoted. This is purely cosmetic (commands logged to stderr stay
// readable in the common case); the behavior is identical either way.
func shEscape(s string) string {
	if s == "" {
		return "''"
	}
	if isShSafeUnquoted(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShSafeUnquoted reports whether every byte of s is safe to pass to
// /bin/sh without any quoting. The allowed set is deliberately
// conservative (alphanumerics + a short allowlist of punctuation that
// has no special meaning to sh); anything else routes through single-
// quote escaping. Letting marginal cases like `*` or `~` through
// unquoted would be unsafe because sh does glob and tilde expansion on
// them.
func isShSafeUnquoted(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '/' || c == '.' || c == ',' || c == ':' || c == '+' || c == '=' || c == '@' || c == '%':
		default:
			return false
		}
	}
	return true
}

// AttachArgv returns the argv that a caller should exec() to attach the
// current process to a specific sm4c-managed window. The caller is
// expected to pass this to syscall.Exec (or the Windows equivalent),
// which replaces the current process with the tmux client — from that
// point forward, the tmux client owns the terminal and sm4c is no
// longer in the process tree.
//
// The returned argv is safe to pass to os/exec: every element is
// either a constant or a tmux-generated window ID (validated by
// parseWindowID to be `@` + decimal digits), so no shell interpretation
// is needed or performed.
func (o OneShot) AttachArgv(windowID string) []string {
	return []string{
		o.TmuxBin,
		"-L", o.SocketName,
		"attach-session",
		"-t", o.SessionName + ":" + windowID,
	}
}

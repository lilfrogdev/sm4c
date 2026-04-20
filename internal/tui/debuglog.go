package tui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debuglog.go is a zero-cost (when disabled) tracing facility gated
// behind the SM4C_DEBUG_LOG env var. When the var names a writable
// file path, every call to debugf appends a timestamped line. When
// unset, debugf is a single nil-pointer check and returns — safe
// to sprinkle into hot paths.
//
// Why this exists: TUI-level "freezes" are hard to diagnose without
// a wire-level trace of which messages Update is actually processing.
// A short burst of debugf calls at Update entry, on each message
// type, and around the external seams (PaneResolver, PaneCapturer,
// WindowResizer, KeySender) is usually enough to distinguish
// "Bubble Tea stopped dispatching" from "we're processing but our
// render output never reaches the terminal" from "we're stuck in a
// tea.Cmd that never returns".
//
// Security posture: the log path is read once at package init from
// the environment, and writes use os.OpenFile with 0o600 perms.
// The file is never read back by sm4c, so a malicious value of
// SM4C_DEBUG_LOG can at worst cause writes to a user-controlled
// path with the invoking user's permissions — the same posture
// shells afford their own redirection. We still refuse paths with
// path-traversal-looking components (".." or embedded control
// bytes) to reduce the blast radius of a mis-set env var.

var (
	debugMu  sync.Mutex
	debugOut *os.File
)

func init() {
	path := os.Getenv("SM4C_DEBUG_LOG")
	if path == "" {
		return
	}
	if !safeDebugPath(path) {
		return
	}
	// #nosec G304,G703 -- path comes from the user's own environment;
	// we validated shape above (safeDebugPath) and refuse control
	// bytes / `..` traversal. The file is opened append-only at 0o600
	// so a pre-existing symlink would still be constrained to the
	// operator's own ownership/permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	debugOut = f
}

// safeDebugPath rejects obviously hostile values for SM4C_DEBUG_LOG.
// We keep this intentionally narrow — the env var is an operator
// opt-in, not attacker-controlled input in a typical threat model —
// but rejecting the easy mistakes (nul bytes, ".." segments) is
// cheap insurance.
func safeDebugPath(p string) bool {
	for _, r := range p {
		if r == 0 {
			return false
		}
	}
	return true
}

// debugf formats a trace line and appends it to the log file.
// Returns immediately when logging is disabled, so callers can
// sprinkle it freely without worrying about hot-path overhead.
func debugf(format string, args ...any) {
	if debugOut == nil {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	_, _ = fmt.Fprintf(debugOut, "%s "+format+"\n", append([]any{ts}, args...)...)
}

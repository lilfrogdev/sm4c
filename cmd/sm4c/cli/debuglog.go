package cli

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debuglog.go mirrors internal/tui/debuglog.go at the CLI layer so
// the bridge goroutine (which lives in cmd/sm4c/cli because it
// wires tmuxctl into tui) can emit traces into the same log file
// the TUI uses. We do not share the file handle — each package
// opens its own — because making the tui package export the
// handle would bleed an implementation detail through its public
// surface, and the duplication is trivial.
//
// The env var name matches (`SM4C_DEBUG_LOG`) so operators set it
// once and get both sides of the conversation.

var (
	debugBridgeMu  sync.Mutex
	debugBridgeOut *os.File
)

func init() {
	path := os.Getenv("SM4C_DEBUG_LOG")
	if path == "" {
		return
	}
	for _, r := range path {
		if r == 0 {
			return
		}
	}
	// #nosec G304,G703 -- operator-controlled env var; we reject nul
	// bytes above and open with 0o600 perms (append-only).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	debugBridgeOut = f
}

// debugBridgef appends a timestamped trace line to the log file.
// A no-op when SM4C_DEBUG_LOG is unset, so callers can sprinkle
// it freely in hot paths without worrying about production cost.
func debugBridgef(format string, args ...any) {
	if debugBridgeOut == nil {
		return
	}
	debugBridgeMu.Lock()
	defer debugBridgeMu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	_, _ = fmt.Fprintf(debugBridgeOut, "%s bridge: "+format+"\n",
		append([]any{ts}, args...)...)
}

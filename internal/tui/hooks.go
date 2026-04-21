package tui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// hookEvent is the state transition reported by a Claude Code lifecycle hook.
type hookEvent uint8

const (
	hookEventNone    hookEvent = iota
	hookEventWorking           // UserPromptSubmit: Claude received input, now working
	hookEventDone             // Stop: Claude finished responding
	hookEventWaiting          // Notification: Claude needs attention or input
)

// hookMsg is delivered to the Bubble Tea runtime when a hook fires.
type hookMsg struct {
	paneID string
	event  hookEvent
}

// hookStreamClosedMsg fires when the FIFO reader goroutine terminates.
type hookStreamClosedMsg struct{}

// startHookListener creates the FIFO at path (mkfifo), opens it O_RDWR so
// the reader holds both ends — preventing EOF when hook writers close between
// writes — and starts a goroutine that parses lines and sends hookMsg values
// on the returned channel.
//
// Each line written by a hook script must be: "<pane_id> <event_type>\n"
// where event_type is one of: "prompt_submit", "stop", "notification".
// Unrecognized lines are silently skipped.
//
// The hook scripts that write to this FIFO should look up the path from the
// SM4C_HOOK_FIFO tmux environment variable and no-op when it is unset — this
// prevents global ~/.claude/settings.json hooks from cross-talking into
// unrelated sm4c instances or plain claude invocations outside tmux.
// startHookListener creates the FIFO at path (mkfifo), opens it O_RDWR so
// the reader holds both ends — preventing EOF when hook writers close between
// writes — and starts a goroutine that parses lines and sends hookMsg values
// on the returned channel.
//
// The returned stop func closes the file, which unblocks the scanner and
// causes the goroutine to exit. It is safe to call stop more than once (a
// sync.Once guards the close). Callers must call stop when the TUI exits so
// that a subsequent TUI iteration does not spawn a second concurrent reader
// on the same FIFO — FIFO reads are exclusive, not broadcast, and two
// concurrent readers would split writes between them.
func startHookListener(path string) (<-chan hookMsg, func(), error) {
	if err := syscall.Mkfifo(path, 0600); err != nil && !os.IsExist(err) {
		return nil, nil, fmt.Errorf("tui: hook listener: mkfifo %s: %w", path, err)
	}
	// O_RDWR keeps both ends open so we never receive EOF when the last
	// hook writer closes its end between writes.
	f, err := os.OpenFile(path, os.O_RDWR, os.ModeNamedPipe) // #nosec G304 -- path constructed by sm4c, not user input
	if err != nil {
		return nil, nil, fmt.Errorf("tui: hook listener: open %s: %w", path, err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = f.Close() }) }
	ch := make(chan hookMsg, 32)
	go func() {
		defer stop()
		defer close(ch)
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 || parts[0] == "" {
				continue
			}
			paneID, evStr := parts[0], parts[1]
			var ev hookEvent
			switch evStr {
			case "prompt_submit":
				ev = hookEventWorking
			case "stop":
				ev = hookEventDone
			case "notification":
				ev = hookEventWaiting
			default:
				continue
			}
			ch <- hookMsg{paneID: paneID, event: ev}
		}
	}()
	return ch, stop, nil
}

// waitForHookEvent returns a Bubble Tea command that blocks until the next
// hookMsg arrives. Returns nil when hookEvents is nil (hooks disabled).
func (m Model) waitForHookEvent() tea.Cmd {
	if m.hookEvents == nil {
		return nil
	}
	ch := m.hookEvents
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return hookStreamClosedMsg{}
		}
		return msg
	}
}

// applyHookEvent records the hook state for the pane's owning window.
// Unknown panes (not yet resolved via paneToWindow) are silently dropped.
//
// waitingGated suppresses Notification events until the next Working event:
// once a Stop fires (✓), the ? indicator is blocked until the user submits
// another prompt. This prevents both Claude Code's automatic post-completion
// desktop notification and any stale Notification from showing ? immediately
// after a ✓.
func (m Model) applyHookEvent(msg hookMsg) {
	if msg.paneID == "" {
		return
	}
	windowID := m.paneToWindow[msg.paneID]
	if windowID == "" {
		debugf("applyHookEvent: DROP pane=%s event=%d (no window mapping; known panes: %v)", msg.paneID, msg.event, m.paneToWindow)
		return
	}
	debugf("applyHookEvent: pane=%s window=%s event=%d", msg.paneID, windowID, msg.event)
	ps := m.paneStatuses[windowID]
	switch msg.event {
	case hookEventWaiting:
		if ps.waitingGated {
			return
		}
	case hookEventDone:
		ps.waitingGated = true
	case hookEventWorking:
		ps.waitingGated = false
	}
	ps.hookState = msg.event
	ps.everHadHook = true
	m.paneStatuses[windowID] = ps
}

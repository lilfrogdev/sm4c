package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sessions.go carries the sidebar's data path: a Session value
// type, a SessionLister dependency the Model calls into to fetch
// managed windows, and the two message types the Bubble Tea runtime
// delivers as the fetch-and-tick loop progresses.
//
// Everything here stays side-effect free from the Model's point of
// view: fetches are expressed as tea.Cmd values the runtime runs on
// its own goroutines, and the only way a fetch returns to the Model
// is through a typed message handled in Update. That keeps Update a
// pure (Model, Msg) -> (Model, Cmd) function, which is the whole
// reason Model.Update can be unit-tested without a real TTY or a
// live tmux server.

// Session is the TUI-local projection of a managed tmux window. The
// CLI layer builds it from tmuxctl.Window (filtering to windows with
// Kind == KindClaude so the sidebar only shows sm4c-created sessions)
// and passes them through SessionLister. Keeping this type in
// internal/tui — rather than re-exporting tmuxctl.Window — preserves
// the one-way dependency: the TUI package never imports tmuxctl, and
// the CLI never renders tmuxctl types directly.
//
// Fields intentionally mirror only what the sidebar view renders in
// M3a. When M3d adds per-session status badges (idle / running /
// needs-input / done), a Status field will land here alongside the
// existing three; any rendering change will be local to view_list.go
// and the lister in cmd/sm4c/cli/tui.go.
type Session struct {
	// WindowID is tmux's stable identifier for the window (e.g. "@3").
	// It is opaque to the TUI — we pass it back verbatim via
	// SelectedWindowID() when the user commits an attach intent, and
	// cmd/sm4c/cli uses it to build the tmux attach argv. If tmux's
	// window ID format ever changes, sm4c's adapter layer is the
	// only code that needs updating.
	WindowID string

	// Name is the window title as claude / the user has set it via
	// `claude -n <name>` or `/rename`. It has already been sanitized
	// via safe.Label by tmuxctl's list-windows parser, so the TUI
	// renders it directly without further escaping.
	Name string

	// Active reports whether tmux considers this window the active
	// one in the sm4c session. It is a hint for the sidebar (a small
	// dot marker) and is informational only — the highlight index
	// the user drives with j/k is independent of this flag.
	Active bool
}

// SessionLister is the TUI's seam onto tmux. It is injected by the
// caller in cmd/sm4c/cli so this package never imports tmuxctl, and
// so tests can drop in an in-memory stub that returns deterministic
// slices. Errors are returned, not hidden — the sidebar surfaces a
// faint one-line notice when a fetch fails, rather than going
// silently blank.
//
// A nil SessionLister is allowed; the Model treats it as "no backing
// store", skips polling, and stays in the empty-state view. This is
// the default for Model.Update unit tests that exercise key handling
// without caring about session data.
type SessionLister func(ctx context.Context) ([]Session, error)

// listTimeout bounds a single ListWindows round-trip. It mirrors the
// 5s default inside tmuxctl.OneShot.run; we set our own here to make
// the ceiling explicit in the TUI layer and to avoid a scenario where
// the runtime's fetch goroutine is stuck while the user waits for
// the sidebar to refresh.
const listTimeout = 3 * time.Second

// DefaultPollInterval is the cadence at which the sidebar asks
// SessionLister for a fresh snapshot. One second is cheap (a single
// tmux list-windows call takes well under a millisecond on a healthy
// socket) and gives the perceived latency of "live". Users who run
// sm4c in very constrained environments can lower it via a future
// config knob; M3a hard-codes the default.
const DefaultPollInterval = 1 * time.Second

// sessionsMsg is the message the runtime delivers after a
// SessionLister call completes. It carries both the fresh snapshot
// and any error, so Update can distinguish "list refreshed, here's
// the new state" from "fetch failed, keep last state and note the
// failure" without inspecting the Model's lister field.
type sessionsMsg struct {
	sessions []Session
	err      error
}

// pollTickMsg is the ticker that schedules the next fetch. We use a
// separate message (rather than self-scheduling inside sessionsMsg's
// handler) so a paused ticker — say, a future "freeze refresh" key —
// is a single-bit change in the Model instead of a handler rewrite.
type pollTickMsg struct{}

// fetchSessions returns a tea.Cmd that, when run, calls the lister
// with a bounded context and wraps the result in a sessionsMsg. If
// the Model has no lister configured, it returns nil: no work, no
// message, no tick chain, which is exactly the behavior the
// empty-state tests rely on.
func (m Model) fetchSessions() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	lister := m.lister
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
		defer cancel()
		s, err := lister(ctx)
		return sessionsMsg{sessions: s, err: err}
	}
}

// scheduleNextPoll returns a tea.Cmd that waits for the configured
// interval and then emits pollTickMsg. The next fetch is kicked off
// by Update's handler for pollTickMsg, not here, so the fetch-then-
// schedule chain stays linear: Init -> fetch -> sessionsMsg ->
// scheduleNextPoll -> pollTickMsg -> fetch. No overlap, no races.
func (m Model) scheduleNextPoll() tea.Cmd {
	if m.lister == nil || m.pollInterval <= 0 {
		return nil
	}
	return tea.Tick(m.pollInterval, func(time.Time) tea.Msg {
		return pollTickMsg{}
	})
}

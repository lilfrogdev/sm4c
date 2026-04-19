// Package tui is sm4c's Bubble Tea front end.
//
// Scope in this milestone (M2c-refinement):
//
//   - Only the empty-state view is implemented. Bare `sm4c` with no
//     subcommand and no claude args opens this view; `sm4c [claude-args…]`
//     still goes straight through the shell-shortcut path in
//     cmd/sm4c/cli/launch.go and never touches the TUI.
//
//   - Key bindings: `n` to request a new session, `q`/`Ctrl+C` to quit,
//     `?` to toggle help. The `n` action returns a sentinel intent to
//     the caller (not a direct tmux call) so the exec-handoff stays
//     concentrated in cmd/sm4c/cli, keeping this package free of
//     subprocess side effects and trivial to unit-test.
//
//   - The sessions-list view and the "pick target folder + name"
//     new-session flow are intentionally deferred to M3. Until then,
//     the TUI is a skeleton; `n` falls back to spawning bare `claude`,
//     equivalent to running `sm4c` with no args under the old M2c
//     behavior.
//
// Design rules enforced here:
//
//   - Terminal-native colors only. No hex strings, no 256-color
//     indexes. Emphasis is expressed via Bold / Faint / Reverse on the
//     user's own palette. The no-hex-color CI gate enforces this.
//
//   - No goroutines, no timers, no I/O inside Update. Every external
//     effect is expressed as a tea.Cmd that the runtime dispatches,
//     and the runtime is the only goroutine outside tests. This keeps
//     Model.Update a pure function of (Model, Msg) -> (Model, Cmd),
//     which is how the unit tests exercise it.
//
//   - No direct import of tmuxctl or os/exec. The TUI signals "the
//     user asked for a new session" via the ActionNewSession intent,
//     and cmd/sm4c/cli decides how to realize it. This is the same
//     separation we use for the exec handoff in launch.go.
package tui

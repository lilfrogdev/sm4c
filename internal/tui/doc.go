// Package tui is sm4c's Bubble Tea front end.
//
// Scope in this milestone (M3b.3 — read-only VT-emulated pane
// preview with capture-pane backfill and pane-size sync):
//
//   - One unified sidebar layout, shown at all times:
//
//       * Title bar — "sm4c", with a faint pluralized session count
//         appended when any sessions exist.
//
//       * Session list — one row per managed session with an
//         active-marker column, name column, and faint window-id
//         column. The highlighted row is painted in reverse video.
//         When no sessions exist, the list area shows a single
//         faint placeholder ("no sessions yet — press n to start
//         one") so the sidebar never collapses to a bare header.
//
//       * Key bar — the full binding list, always visible so the
//         user can see what's available regardless of session
//         count.
//
//       * Disclaimer footer — "sm4c is not affiliated with
//         Anthropic…" — rendered ONLY in the empty state, where a
//         first-time user benefits from the context. Users with
//         live sessions have already internalized what sm4c is and
//         would just see it as clutter.
//
//     On a terminal wide enough to split (>= minSplitWidth = 60
//     cols), the layout renders as a visible left sidebar column
//     with a vertical border and a live pane-preview column to its
//     right. Below that threshold the view falls back to a full-
//     width stacked form so the sidebar content doesn't get
//     squeezed into an unreadable column. Keeping the empty-state
//     and populated states on the SAME layout avoids a jarring
//     reflow the moment the first session appears.
//
//   - Pane preview (right column). When cmd/sm4c/cli has set up the
//     tmux control-mode bridge, the right column streams %output
//     events from the highlighted session's active pane into a
//     per-pane charmbracelet/x/vt emulator and renders the emulator's
//     ANSI-styled screen on every frame. The emulator is sized to
//     match the right-pane body on every tea.WindowSizeMsg, and
//     cmd/sm4c/cli propagates that size to tmux via resize-window
//     so claude's upstream pane grid tracks what sm4c renders.
//     On the first resolve for each pane, a capture-pane round-trip
//     seeds the emulator with the session's current screen so
//     switching to a long-running session shows its state
//     immediately — live %output bytes that arrive during the
//     capture are buffered and flushed in-order after the capture
//     result lands, so the user never sees a scrambled replay.
//
//     Every state the preview can be in has an explicit rendering:
//
//       * no sessions     — "press n to start a new session"
//       * no stream       — "pane preview unavailable"
//       * stream closed   — "preview disconnected — tmux control
//                           channel closed"
//       * resolving       — "resolving pane…"
//       * resolver error  — "pane lookup failed: <reason>"
//       * no bytes yet    — "waiting for output  <pane-id>"
//       * bytes present   — the emulator's Render() output, which
//                           re-emits SGR escapes so the user's
//                           native terminal palette carries through
//
//     The preview is intentionally read-only. Input routing and the
//     focus toggle land in M3c; nothing about M3c changes the
//     sidebar, the layout, or the rendering contract above.
//
//   - Key bindings:
//
//       * `j` / `↓`    move highlight down (no wrap)
//       * `k` / `↑`    move highlight up   (no wrap)
//       * `enter`      deliberate no-op; reserved for a future focus
//                      shortcut. sm4c has no exec-into-tmux path by
//                      design — every session is reached through
//                      this TUI.
//       * `n`          request a new session (emits
//                      ActionNewSession). The caller spawns the
//                      claude window and re-enters the TUI with the
//                      new window ID as initial highlight, so the
//                      sidebar reopens focused on the session that
//                      was just created.
//       * `ctrl+b`     placeholder for M3c's focus toggle (VSCode-
//                      style move of focus from sidebar to the
//                      right pane); no-op here so the binding is
//                      pinned before its behavior lands.
//       * `?`          toggle the expanded help block
//       * `q` / `ctrl+c`  quit (emits ActionNone)
//
//   - Live session data is fetched via an injected SessionLister.
//     cmd/sm4c/cli wraps tmuxctl.OneShot.ListWindows and filters to
//     windows tagged by sm4c; the TUI never imports tmuxctl. The
//     fetch-tick-fetch loop is strictly serial: Init kicks off the
//     first fetch, every sessionsMsg schedules the next pollTickMsg,
//     and every tick issues a fresh fetch. No overlap.
//
//   - The compose ("pick cwd + name") sub-view for `n` is
//     intentionally deferred to M3e. Until then, `n` asks the CLI
//     layer to spawn a bare claude in the process's current working
//     directory, and the TUI reopens with the new window
//     highlighted.
//
//   - Input routing (M3c) and the status FSM (M3d) are still
//     deferred. The pane preview is read-only until M3c lands the
//     focus toggle + keystroke forwarding path.
//
// Design rules enforced here:
//
//   - Terminal-native colors only. No hex strings, no 256-color
//     indexes. Emphasis is expressed via Bold / Faint / Reverse on
//     the user's own palette. The no-hex-color CI gate enforces this.
//
//   - No goroutines, no timers, no I/O inside Update. Every external
//     effect is expressed as a tea.Cmd that the runtime dispatches,
//     and the runtime is the only goroutine outside tests. This
//     keeps Model.Update a pure function of (Model, Msg) -> (Model,
//     Cmd), which is how the unit tests exercise it.
//
//   - No direct import of tmuxctl or os/exec. The TUI signals
//     "spawn a new session" via a typed Action intent, and
//     cmd/sm4c/cli decides how to realize it. There is no "attach"
//     intent by design: the right pane IS how sessions are reached,
//     and no code path in sm4c execs into tmux.
package tui

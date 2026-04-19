// Package tui is sm4c's Bubble Tea front end.
//
// Scope in this milestone (M3a):
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
//     The sidebar is full-width in M3a; M3b will split the layout
//     into a left-column sidebar and a right-column hosted pane so
//     the sidebar remains visible while a session is live. Keeping
//     the empty-state and populated states on the SAME layout
//     avoids a jarring reflow the moment the first session appears.
//
//   - Key bindings:
//
//       * `j` / `↓`    move highlight down (no wrap)
//       * `k` / `↑`    move highlight up   (no wrap)
//       * `enter`      commit the highlighted session as the attach
//                      target (emits ActionAttachSession)
//       * `n`          request a new session (emits
//                      ActionNewSession)
//       * `ctrl+b`     placeholder for M3b's focus toggle; no-op
//                      here so the binding is pinned before its
//                      behavior lands
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
//     intentionally deferred to M3e. For M3a, `n` is a stopgap that
//     still falls through to a bare claude launch via the CLI.
//
//   - The embedded pane (claude output rendered next to the sidebar,
//     with input routing in M3c and a status FSM in M3d) is
//     intentionally deferred. ActionAttachSession is realized by
//     cmd/sm4c/cli via syscall.Exec into tmux attach-session until
//     M3b lands the hosted viewport.
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
//     "attach to this session" / "spawn a new session" via typed
//     Action intents, and cmd/sm4c/cli decides how to realize them.
//     This is the same separation we use for the exec handoff in
//     launch.go.
package tui

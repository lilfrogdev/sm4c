// Package tui is sm4c's Bubble Tea front end.
//
// Scope in this milestone (M3e — sidebar zoom, session-card
// layout with cwd, and card-style full-width highlight, on
// top of the M3d status-glyph surface):
//
//   - One unified sidebar layout, shown at all times (unless
//     the user has zoomed it away — see "Sidebar zoom" below):
//
//       * Title bar — "sm4c", with a faint pluralized session count
//         appended when any sessions exist.
//
//       * Session list — one "card" per managed session. A card
//         is up to two lines: a header line (status glyph + name)
//         and a faint second line showing the active pane's
//         working directory, shortened with "~/…" when it lives
//         under the user's home directory and truncated from the
//         head with "…" when it would overflow the column. Cards
//         are separated by a blank line so the list reads as
//         discrete items. The highlighted card is painted as a
//         solid full-width band using configurable color indices
//         (default ANSI "8" bg + "15" fg; sm4c.toml allows 0–255
//         with lipgloss/termenv mapping to the terminal profile).
//         An earlier
//         round wrapped the highlight in a rounded border; live
//         use surfaced two failure modes (the background color
//         stopped at the border interior, and the border vs.
//         no-border width mismatch made the list visibly jump
//         horizontally as the cursor moved). Both variants now
//         share Padding(0, 1) and no border or margin, so the
//         only visible difference between selected and
//         unselected rows is the background fill. The unit-test
//         path (no WindowSizeMsg, unknown content width) falls
//         back to the pre-M3e single-run reverse-video
//         highlight, so substring-based tests keep working
//         unchanged.
//         The status glyph (M3d) reflects that session's live
//         state: faint `·` (Quiet), solid `●` (Idle), animated
//         braille spinner (⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏ — Working), `✓`
//         (Attention — claude finished AND rang the bell at
//         some point during the run; the user hasn't
//         acknowledged it yet). The Attention glyph is a
//         checkmark (not a dot) so the "this one's done, your
//         turn" signal carries through the reverse-video
//         attribute that paints the highlighted row, since
//         reverse swaps foreground and background and can
//         hide a foreground-only color change. The checkmark
//         is rendered in the sidebar's native weight — not
//         bold, not colored — so Attention reads as a calm
//         "done, your turn" rather than an alarm. Earlier
//         iterations used bold and/or red; both over-weighted
//         the signal relative to Idle and Working. The shape
//         is the signal. The spinner only runs while at least
//         one session is Working, so an all-idle TUI is
//         visually still. The Working state is suppressed for
//         keystrokeEchoWindow (400ms) after every forwarded
//         keystroke, so claude's per-keypress redraw bytes do
//         not masquerade as "claude is thinking" — only bytes
//         that arrive outside the echo window (response
//         streams, tool output, claude's own thinking
//         spinner) actually animate the glyph. When no sessions exist, the list area
//         shows a single faint placeholder ("no sessions yet —
//         press n to start one") so the sidebar never collapses
//         to a bare header.
//
//       * Help hint — a single "? help" row anchored at the
//         bottom of the sidebar. Pressing `?` toggles an
//         expanded block of every binding for the current
//         focus, still at the bottom of the column, and
//         pressing `?` again collapses it back to the hint.
//         Earlier iterations kept the full binding list
//         visible at all times under the session list; live
//         usage found that the permanent key bar stole
//         vertical space from sessions and that users who
//         wanted the keys reached for `?` anyway. Hiding the
//         list behind `?` preserves discovery ("press this
//         one visible key to see everything") without the
//         permanent vertical cost. The close-confirmation
//         prompt ("close <name>? y …") replaces the hint
//         while a kill is armed so the user sees exactly one
//         question at a time.
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
//     Per-pane emulator state, the preview rendering contract, and
//     the layout itself are unchanged in M3c. What M3c adds is a
//     focus state that decides where keystrokes go.
//
//   - Focus state (M3c). Model.focus is one of FocusSidebar (default)
//     or FocusPane. The right-pane header shows "[focus]" when the
//     pane has focus and "[ctrl+b to focus]" otherwise, so the user
//     can always see where keystrokes will land. Deps.InitialFocus
//     lets the CLI pick the starting focus per entry point:
//
//       * bare `sm4c` — starts on the sidebar (there's nothing to
//         type into yet).
//       * `sm4c [claude-args…]` and the `n` re-entry — start on the
//         pane so the user can type into the freshly-spawned session
//         immediately.
//
//     Focus reverts to the sidebar automatically if the highlighted
//     session disappears or if the active pane returns ErrPaneGone
//     mid-turn. Transient send errors are absorbed silently to avoid
//     yanking the user around on a single tmux hiccup.
//
//   - Key bindings (focus-sensitive):
//
//     Sidebar focus (sm4c owns the keyboard):
//       * `j` / `↓`    move highlight down (no wrap)
//       * `k` / `↑`    move highlight up   (no wrap)
//       * `enter`      shortcut for ctrl+b: move focus into the
//                      highlighted session's pane. On an empty
//                      sidebar, enter is a no-op.
//       * `n`          request a new session (emits
//                      ActionNewSession). The caller spawns the
//                      claude window and re-enters the TUI with the
//                      new window ID as initial highlight AND
//                      InitialFocus = FocusPane, so the sidebar
//                      reopens focused on the new session and the
//                      user can start typing.
//       * `ctrl+b`     toggle focus: move into the highlighted
//                      pane (no-op if no session is highlighted).
//       * `x`          arm a close on the highlighted session.
//                      The key bar is replaced by a
//                      "close <name>? y (any other key cancels)"
//                      hint; the next keystroke either confirms
//                      with `y` / `Y` (which calls through
//                      WindowCloser to kill the tmux window,
//                      sending SIGHUP to claude — lifecycle-
//                      identical to /exit) or cancels on anything
//                      else. No tmux round-trip happens before
//                      confirmation, so a mis-typed `x` is a safe
//                      one-step rollback.
//       * `z`          hide the sidebar (tmux-style zoom). The
//                      flip is atomic: sidebarHidden = true AND
//                      focus = FocusPane both take effect in the
//                      same Update tick, the pane viewport is
//                      re-measured to full terminal width, and
//                      tmux is issued a resize-window so claude
//                      redraws for the new grid. Requires at
//                      least one highlightable session — zoom
//                      on an empty sidebar is a no-op so the
//                      user cannot trap themselves in a blank
//                      viewport with no binding to spawn a
//                      session. `Ctrl+Shift+B` was the user's
//                      first-pass request, but in terminals
//                      without the Kitty keyboard protocol
//                      enabled it collapses to the same byte
//                      as `Ctrl+B` and bubbletea can't tell
//                      them apart.
//       * `?`          toggle the expanded help block
//       * `q` / `ctrl+c`  quit (emits ActionNone). Claude sessions
//                      keep running on the sm4c tmux socket.
//
//     Pane focus (claude owns the keyboard):
//       * `ctrl+b`     toggle focus back to the sidebar. If the
//                      sidebar was zoomed away via `z`, this
//                      also un-hides it and rescales the pane
//                      viewport back to the split layout in the
//                      same tick — single-binding symmetry so
//                      users do not need to learn a second
//                      shortcut for restoration.
//       * any other key — translated to its terminal byte sequence
//                      by keyMsgToBytes and forwarded to the active
//                      pane via the KeySender seam. `ctrl+c` is
//                      forwarded to claude (interrupts the running
//                      turn); to quit sm4c from pane focus, press
//                      ctrl+b first, then q or ^C.
//
//   - Live session data is fetched via an injected SessionLister.
//     cmd/sm4c/cli wraps tmuxctl.OneShot.ListWindows and filters to
//     windows tagged by sm4c; the TUI never imports tmuxctl. The
//     fetch-tick-fetch loop is strictly serial: Init kicks off the
//     first fetch, every sessionsMsg schedules the next pollTickMsg,
//     and every tick issues a fresh fetch. No overlap. Cadence is
//     configurable via Deps.PollInterval; the CLI layer reads
//     Config.SessionPollInterval (TOML key session_poll_interval,
//     default "1s") and plumbs it through openTUI.
//
//   - Sidebar zoom (M3e). Model.sidebarHidden collapses the
//     sidebar column so the right pane takes the entire
//     viewport, and is always paired with FocusPane while set.
//     Toggled via `z` (in sidebar focus) to enter zoom and
//     `ctrl+b` (from pane focus) to leave — both transitions
//     re-run rightPaneBodyDims, re-size every live emulator,
//     and emit a resize-window so claude's upstream grid
//     matches what the TUI draws. The flag does NOT persist
//     across sm4c launches: every fresh TUI starts with the
//     sidebar visible so a first-time observer is never handed
//     a blank pane with no obvious way back to the session
//     list. The right-pane header grows a "[ctrl+b: show
//     sidebar]" hint while zoomed so the restoration path is
//     always self-explanatory.
//
//   - Session working directory (M3e). tmuxctl's list-windows
//     format string now includes #{pane_current_path}, surfaced
//     to the TUI as Session.Cwd. The sidebar renders this as a
//     faint second line per card, shortPath()'d to "~/…" under
//     the home directory and truncLeft()-truncated when the
//     path would overflow the sidebar column. Empty Cwd (tmux
//     couldn't resolve a path, or the test stub doesn't set it)
//     omits the line cleanly so no blank-indent row leaks into
//     the view.
//
//   - The compose ("pick cwd + name") sub-view for `n` is
//     intentionally deferred to a later milestone. Until then, `n` asks the CLI
//     layer to spawn a bare claude in the process's current working
//     directory, and the TUI reopens with the new window
//     highlighted.
//
//   - Status FSM (M3d). Per-session status is derived from the
//     pane byte stream sm4c already consumes, NOT from tmux's
//     monitor-bell / monitor-activity / monitor-silence flags.
//     Those flags are sticky in tmux (cleared only when a client
//     "sees" the window) and fit a "notify me about a window
//     I'm not looking at" use case, not sm4c's reactive sidebar.
//     The central model is that "claude is working" (bytes
//     flowing vs. silent) and "claude wants you" (bell rang)
//     are LAYERED signals, not competing ones. Working is
//     answered by the byte stream directly; Attention is
//     answered only AFTER the stream has quieted down.
//     Transitions per pane:
//       * Quiet → Working on first byte.
//       * Working → Idle after Deps.SilenceThreshold of no
//         bytes (plumbed from Config.MonitorSilence,
//         default 1.5s) and no bell was rung during the run.
//       * Working → Attention after the same silence, when a
//         bell WAS rung during the run. The bell is
//         remembered in paneStatus.bell from the moment the
//         BEL byte (0x07) arrives, but it is not surfaced
//         as Attention while bytes are still flowing — doing
//         that would yank the spinner mid-stream and tell the
//         user "done" when claude is still generating (the
//         "the bang appears too soon" failure mode).
//       * Idle/Attention → Working on any new byte (assuming
//         the bytes are not inside the echo window below).
//       * Attention → derived-from-activity once the user
//         types into that pane (the keystroke clears bell
//         and opens the echo window in one step).
//     Bytes arriving within keystrokeEchoWindow (400ms) of
//     the most recent keystroke are treated as claude's
//     prompt redraw rather than claude working — they leave
//     the glyph on Idle instead of flipping it to Working.
//     A pending bell inside the echo window is also treated
//     as implicitly acknowledged, because the user's hands
//     are already on the pane and a flashing bang would be
//     noise. Without the echo-window arc, every keypress
//     the user sent would spin the glyph for 3s while
//     nothing was actually happening (claude was not
//     thinking, the bytes were just echo).
//     The animation ticker (statusFrameInterval = 100ms,
//     for a 1s rotation across the 10 braille frames) only
//     runs while at least one pane is Working, so a TUI full
//     of idle sessions does not burn a background re-render
//     budget. Status is keyed by tmux window ID in
//     paneStatuses (not pane ID) so the close-session churn
//     that invalidates paneByWindow for survivors does not
//     flicker the sidebar glyphs.
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

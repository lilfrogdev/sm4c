# sm4c — Session Manager for Claude

**Status: v0, pre-alpha. Not ready for use.**

sm4c is an unofficial, community-built TUI session manager that sits on top of the official [Claude Code](https://docs.anthropic.com/en/docs/claude-code/overview) CLI. It gives you a Cursor-style sidebar of concurrent claude sessions with per-session status indicators, so you can run multiple claude conversations in parallel and glance at which ones need your attention — without juggling terminal windows or tabs.

sm4c is **not affiliated with, endorsed by, or sponsored by Anthropic**. It does not speak to Anthropic's APIs directly. All model interactions happen inside a child `claude` process that sm4c hosts through [tmux](https://github.com/tmux/tmux); sm4c is purely a local UX layer on top of the official CLI.

## What sm4c does

- Runs multiple concurrent `claude` sessions on an isolated tmux server (`tmux -L sm4c`), kept separate from your personal tmux.
- Shows all sessions in a sidebar with a per-session status glyph (idle, activity, needs-attention, silence/done, exited).
- Lets you switch between sessions instantly without losing state.
- Uses your terminal's native color scheme — no bundled theme; whatever your terminal looks like, sm4c looks like.
- Session names are owned by claude (`claude -n NAME` or `/rename` inside a session). sm4c just displays them.

## What sm4c is not

- Not a replacement for the Claude Code CLI. You need `claude` installed and working separately.
- Not a tmux wrapper. sm4c uses tmux internally, but you never interact with tmux directly.
- Not an automation tool. sm4c does not synthesize prompts, drive claude non-interactively, or make network calls.
- Not a plain-tmux shell. sm4c does one thing: session management.

## Requirements

- **macOS or Linux** (Windows is not supported in v1).
- **[Claude Code CLI](https://docs.claude.com/en/docs/claude-code/setup) installed.** Install separately — sm4c does not bundle it. Both install methods are supported:
  - The official native installer (`curl -fsSL https://claude.ai/install.sh | bash`) — drops a launcher at `~/.local/bin/claude`.
  - Homebrew: `brew install claude` — drops a launcher at `/opt/homebrew/bin/claude` (Apple Silicon) or `/usr/local/bin/claude` (Intel).
  - npm / bun global installs are also detected.
- **[tmux](https://github.com/tmux/tmux) ≥ 3.2.** Install via your package manager (`brew install tmux`, `apt install tmux`, etc.).
- **Go 1.22+** (for building from source; not needed if you use a released binary).

### Shell compatibility

sm4c is shell-agnostic. It works identically under **bash**, **zsh**, **fish**, **nushell**, and any other POSIX-ish shell, because:

- sm4c resolves `claude` and `tmux` by (1) explicit config, then (2) the inherited `$PATH`, then (3) a small allowlist of well-known install paths (`~/.local/bin`, `~/.bun/bin`, `~/.npm-global/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`). You do not need `claude` on `$PATH` for sm4c to find it — though `sm4c doctor` will emit a **warn** if the fallback is used, since other tools may still expect it on `$PATH`.
- Each tmux pane inherits your `$SHELL`, so whatever shell you normally use is the shell you'll see inside sm4c.
- `claude` is spawned directly (never through a shell wrapper), so your `.bashrc` / `.zshrc` / `config.nu` never affect sm4c's plumbing.

If you installed `claude` via the curl installer and your shell can't find it yet, add `~/.local/bin` to your `$PATH`:

```bash
# bash: append to ~/.bashrc
export PATH="$HOME/.local/bin:$PATH"
```

```zsh
# zsh: append to ~/.zshrc
export PATH="$HOME/.local/bin:$PATH"
```

```fish
# fish: run once; fish_add_path persists across sessions
fish_add_path -U ~/.local/bin
```

```nushell
# nushell: append to ~/.config/nushell/env.nu
$env.PATH = ($env.PATH | split row (char esep) | prepend $"($env.HOME)/.local/bin")
```

Restart your shell after editing its config.

## Install

Releases are not yet available. To build from source:

```bash
git clone https://github.com/lilfrogdev/sm4c.git
cd sm4c
make build
./sm4c --version
```

## Configuration (optional)

sm4c reads `sm4c.toml` when you pass `--config` (see `sm4c setup` / `docs` in the repo). Useful keys:

- **`session_poll_interval`** — how often the sidebar refreshes from tmux (default `1s`).
- **`monitor_silence`** — how long a pane must be quiet before the status glyph treats it as idle (default `1.5s`).
- **`sidebar_highlight_bg`** / **`sidebar_highlight_fg`** — decimal strings **`"0"`** through **`"255"`**. Indices **`0`–`15`** are the classic ANSI colors; **`16`–`255`** are the xterm 256-color palette. Lipgloss maps them to what your terminal supports (on 16-color-only sessions, higher indices are approximated). Defaults are **`8`** (gray bar) and **`15`** (bright white text). **Opacity / alpha is not available** in the terminal color model. Sm4c does not use hex colors in the TUI (CI-enforced).

## Quickstart

*(M3e polish: session rows are now two-line cards (name + faint working directory), the selection highlight spans the full sidebar column, and `z` hides the sidebar entirely so claude takes the full viewport. Status badges, close-session confirmation, configurable poll cadence, and input routing carried over from M3c/M3d. The compose flow for `n` is the last remaining M3e item.)*

```bash
# Open the TUI. The sidebar is always visible:
#   - no sessions yet   → right pane shows "press n to start a new session"
#   - one or more       → highlighted row streams a live VT-emulated preview on the right
#
# Focus model (M3c):
#   - Sidebar focus: sm4c owns the keyboard.
#       j / k / ↑ / ↓   move highlight
#       n               new claude session (bare; compose flow comes in M3e)
#       enter           shortcut for ctrl+b: move focus into the highlighted pane
#       ctrl+b          toggle focus: move into the highlighted pane
#       x               arm a close on the highlighted session; press y to confirm,
#                       any other key cancels. A close sends SIGHUP to claude, same
#                       lifecycle effect as running /exit inside the pane.
#       z               hide the sidebar ("zoom"): the right pane takes the whole
#                       viewport and focus moves to the pane. Requires at least one
#                       session (zooming an empty sidebar would trap you). Press
#                       ctrl+b to bring the sidebar back.
#       ?               toggle help
#       q / ^C          quit sm4c (claude sessions keep running on the sm4c socket)
#   - Pane focus: claude owns the keyboard.
#       (every keystroke, including ^C, is forwarded to claude)
#       ctrl+b          toggle focus back to the sidebar. If the sidebar was
#                       zoomed away via z, ctrl+b also un-hides it.
#       (to quit sm4c from pane focus, press ctrl+b then q or ^C)
sm4c

# Launch with claude args. This ALSO opens the TUI — focused on the new session.
# sm4c never exec-attaches into tmux: every surface is the TUI.
sm4c /help                    # start with /help as the first input
sm4c -- -n api-refactor       # claude's own -n flag, past sm4c's flag parser
sm4c -- -c                    # continue most recent conversation
sm4c -- --model opus-4.1

# Read-only CLI for scripting.
sm4c ls           # list managed sessions (sm4c-tagged tmux windows)
sm4c status       # server and install status
sm4c doctor       # security and environment self-check
sm4c version      # version info
sm4c stop         # stop the sm4c tmux server (destructive)
```

Anything after `--` is forwarded verbatim to `claude`. If a claude arg does not start with a dash, you can omit the `--` (e.g. `sm4c /help` just works).

To rename a session, type `/rename <newname>` directly inside the claude pane — sm4c does not mediate rename. sm4c does not expose a tmux detach shortcut; every interaction with a session happens inside the TUI.

Session cards (M3e). Each sidebar row is a two-line card:

```
⠋ refactor-auth
  ~/Repos/auth-svc
```

The first line shows the live status glyph (see below) followed by the session name; the second line shows the active pane's working directory, shortened to `~/…` when it lives under your home and truncated from the head with `…` when it would overflow the sidebar column. When tmux cannot resolve a cwd (e.g. the pane's process is mid-exit), the second line is omitted cleanly. The highlighted card is painted with a full-width reverse-video band, so the selection reads as a visible block rather than a tight text run — matching the convention used by claude-squad and Cursor's session pickers.

Session status glyphs (M3d). Each session's status glyph occupies a two-column cell at the start of the card:

- `·` (faint) — Quiet: the session is alive but hasn't produced any output yet.
- `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` (animated braille spinner) — Working: claude is streaming output right now. The spinner keeps animating through tool-call boundaries and mid-response bells; it only stops once the pane goes quiet for `monitor_silence` (default 1.5s).
- `●` — Idle: claude finished its response and the pane has gone quiet.
- `✓` — Attention: claude rang the terminal bell at some point during the run AND the pane has since gone quiet. The check surfaces only after the byte stream settles, so it genuinely means "claude is done and wants you" rather than "claude rang the bell mid-stream". The check stays up until you type into that session's pane, which acknowledges the bell. The shape is deliberately different from the Idle/Quiet dots so the signal carries even on the highlighted row (which paints in reverse video). The check is rendered in the sidebar's native weight — not bold, not colored — so Attention reads as a calm "done, your turn" rather than an alarm.

The Working spinner is deliberately silenced for ~400ms after every keystroke you forward to a pane. Claude's TUI redraws its prompt line on every keypress, which shows up as a burst of bytes in the pane stream; without this echo-suppression window the spinner would animate while you type and freeze the moment you stop, which is exactly backwards of what "working" should mean.

The animation only runs while at least one session is Working, so a TUI full of idle sessions is visually still. Status is derived entirely from the byte stream sm4c already consumes; no tmux `monitor-*` options are toggled.

Sidebar zoom (M3e). Press `z` with the sidebar focused to hide the sidebar column entirely — the right pane stretches to the full viewport width and focus flips to the pane in the same tick, so claude sees the new grid size and can redraw immediately. This is useful when you want to treat a single session as a fullscreen claude view. Press `ctrl+b` from the pane to un-hide the sidebar and return focus in one keystroke. The right-pane header shows a `[ctrl+b: show sidebar]` hint while zoomed so the restoration path is always visible.

`Ctrl+Shift+B` would have been a more obvious binding given the mnemonic with `Ctrl+B`, but most terminals without the Kitty keyboard protocol collapse `Ctrl+Shift+B` to the same byte as `Ctrl+B`, leaving bubbletea unable to tell them apart. `z` is the tmux-zoom convention and only takes effect with the sidebar focused, so it never steals keystrokes from claude.

Planned for M3e (not yet shipped):

- Compose flow for `n`. Pick the target working directory and set a session name before spawning, replacing today's bare-`claude` stopgap.

## Security

sm4c is designed with security as a first-class concern. See [SECURITY.md](SECURITY.md) for the threat model, secure-coding practices, and responsible-disclosure policy.

Highlights:

- No network access. sm4c does not call home, auto-update, or report telemetry.
- No shell interpolation. All `tmux` / `claude` invocations use `exec.Command` with separate arguments.
- All strings that are rendered or passed to tmux are sanitized through a single `internal/safe` package.
- Logs default to INFO and never contain keystrokes or model output.
- Reproducible builds via `go build -trimpath -buildvcs=true`.

## Troubleshooting

If `sm4c` itself refuses to start and you need to recover live sessions:

```bash
tmux -L sm4c attach -t sm4c
```

This is a last-resort recovery path, not a supported workflow. Normal usage is through `sm4c` only.

## License

[MIT](LICENSE).

## Trademarks

"Claude" and "Claude Code" are trademarks of Anthropic, PBC. sm4c is an independent, unaffiliated project.
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

## Quickstart

_(M3c lands input routing: keystrokes in the highlighted pane flow through to claude, `Ctrl+B` toggles focus between sidebar and pane, startup focus is context-aware, `x` closes the highlighted session with a one-key confirmation, and the session-list poll cadence is configurable via `session_poll_interval` in `sm4c.toml`. Status badges and the compose flow land in M3d–M3e.)_

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
#       ?               toggle help
#       q / ^C          quit sm4c (claude sessions keep running on the sm4c socket)
#   - Pane focus: claude owns the keyboard.
#       (every keystroke, including ^C, is forwarded to claude)
#       ctrl+b          toggle focus back to the sidebar
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

Session status glyphs (M3d). Each sidebar row starts with a two-column cell that reflects that session's live state:

- `·` (faint) — Quiet: the session is alive but hasn't produced any output yet.
- `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` (animated braille spinner) — Working: claude is streaming output right now.
- `●` — Idle: claude finished its response and is waiting for you. Threshold is `monitor_silence` from `config.toml` (default 3s).
- `●` (amber) — Attention: claude rang the terminal bell (permission prompt, error). The glyph stays amber until you type into that session's pane, which acknowledges the bell.

The animation only runs while at least one session is Working, so a TUI full of idle sessions is visually still. Status is derived entirely from the byte stream sm4c already consumes; no tmux `monitor-*` options are toggled.

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

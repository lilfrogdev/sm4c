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

_(TUI not yet implemented — this is the v1 target behavior.)_

```bash
# Launch the TUI; reattaches to any existing sessions.
sm4c

# Launch and create a new named claude session (claude's own -n flag).
sm4c -n api-refactor

# Pass any claude args through — sm4c forwards everything it doesn't own.
sm4c -c                       # continue most recent conversation
sm4c --model opus-4.1
sm4c -n feature-x -c

# Read-only CLI for scripting.
sm4c ls           # list sessions
sm4c status       # server and install status
sm4c doctor       # security and environment self-check
sm4c version      # version info
sm4c stop         # stop the sm4c tmux server (destructive)
```

In the TUI, the prefix key is `Ctrl-a` by default. After pressing the prefix:

- `n` — new session (prompts for optional claude args)
- `c` — close active session (sends `/exit`, 5s timeout, then force-confirm)
- `1..9` / `j`/`k` — switch sessions
- `s` — fuzzy switcher
- `u` — toggle the "Unmanaged" section (windows on the sm4c socket that sm4c didn't create)
- `?` — help
- `Ctrl-a` — send a literal `Ctrl-a` to the active claude

To rename a session, type `/rename <newname>` directly inside the claude pane — sm4c does not mediate rename.

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

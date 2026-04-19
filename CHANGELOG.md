# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) starting with v1.0.0.

## [Unreleased]

### Added

- M0: repository skeleton, license, docs, and security baseline.
- M0: cobra CLI scaffold with `sm4c version` and `sm4c doctor` stubs.
- M0: `internal/safe` input sanitizer with unit tests.
- M0: `internal/config` TOML loader with permission checks.
- M0: CI pipeline (`go vet`, `staticcheck`, `golangci-lint` with `gosec`, `govulncheck`, no-network grep, no-hex-color grep).
- M0: reproducible-build flags (`-trimpath -buildvcs=true`).
- M1: `internal/tmuxctl` control-mode client — strict `\ooo` octal unescape for `%output`, line-oriented event parser for `%begin`/`%end`/`%error`/`%output`/`%exit`, golden-file fixture test, and a `Client` that spawns `tmux -L sm4c -C` on an isolated socket with request-pairing, handshake absorption, and backpressure-safe async-event fan-out.
- M1: unit + fuzz coverage (`FuzzUnescape`, `FuzzParser`) and an opt-in `-tags=integration` live-tmux round-trip test.
- M1: full `cmd/sm4c/cli/preflight.go` — resolves tmux/claude via config override or `$PATH`, enforces absolute paths + executable bit, probes `tmux -V` and enforces `>= 3.2`, and validates the tmux socket directory ownership + `0700` permissions.
- M1: `sm4c doctor` now prints each preflight finding with severity and exits non-zero on any fatal finding.
- M1: `internal/platform` helper package for OS-specific file-owner lookup (shared by `internal/config` and `cmd/sm4c/cli`).
- M1: `internal/platform.KnownClaudeLocations` / `KnownTmuxLocations` / `FindKnownBinary` — shell-agnostic fallback that finds `claude` / `tmux` when they're installed but not on the sm4c process's inherited `$PATH` (curl-installer default `~/.local/bin`, bun/npm global prefixes, system bin dirs). `sm4c doctor` surfaces the fallback as a **warn** so users can still fix their `$PATH` at leisure.
- M1: `internal/safe.Line` — sister of `safe.Label` with a 1 KiB cap, used for diagnostic detail strings (resolved paths, tmux version output) that are longer than a sidebar label but still bounded.
- docs: README now covers both install methods (curl + Homebrew) and shows `$PATH` setup snippets for bash, zsh, fish, and nushell.
- M2a: `internal/tmuxctl/oneshot.go` — one-shot `tmux -L sm4c <cmd>` helpers (`ServerRunning`, `ListWindows`, `SessionExists`, `KillServer`) on top of a shared `run` that enforces absolute `tmux` paths, bounds stderr at 8 KiB, sanitizes error text via `safe.Line`, and classifies "server not running" as a non-error sentinel (`ErrServerNotRunning`). Window parsing uses an anchored tab-delimited format where the only free-form field (`window_name`) is placed last and sanitized via `safe.Label` before being stored.
- M2a: window-ownership invariant — constants `tmuxctl.KindKey = "@sm4c-kind"` and `tmuxctl.KindClaude = "claude"` plus `Window.Managed()` helper. M2b/M2c consume this to filter rogue windows out of every read path by default.
- M2b: `sm4c ls [--all] [--json]` — lists managed sessions; unmanaged windows are hidden by default with a `(N hidden; pass --all to show)` hint and revealed in a separate read-only "Unmanaged" section when requested. JSON output has a stable shape (`{server_running, managed, unmanaged?}`) so scripts can gate on `managed[].id` without fearing churn.
- M2b: `sm4c status` — one-screen summary of tmux/claude paths and versions, server liveness, and managed/unmanaged window counts. Strictly read-only.
- M2b: `sm4c stop [--force]` — tears down the sm4c tmux server; requires an interactive `stop`-typed confirmation unless `--force` is set; non-TTY stdin without `--force` aborts with a clear error so unattended scripts fail safely.
- M2b: `cmd/sm4c/cli/oneshot.go` — `setupOneShot` helper that every CLI subcommand uses to load config, run preflight, refuse to proceed on unresolvable tmux, and construct a `tmuxctl.OneShot` bound to the preflight-resolved absolute tmux path.
- M2c: `internal/tmuxctl/spawn.go` — `NewClaudeWindow` creates a new `@sm4c-kind=claude`-tagged tmux window running the given claude binary with verbatim-forwarded args, falling back to `new-session` when the sm4c session doesn't yet exist. Every spawn chain pins tmux's `default-shell` to `/bin/sh` on the sm4c socket (defense against nushell/fish/elvish login shells mangling POSIX single-quote escapes) and sets `allow-rename on` / `automatic-rename off` so claude's `/rename` and OSC 0/2 title writes become the authoritative source of window names. Argument transport uses `shEscape` (POSIX single-quote with `'\''` escaping) with a fuzz test that asserts byte-identical round-trip through `/bin/sh -c`.
- M2c: `tmuxctl.OneShot.AttachArgv` — returns the `argv` for `exec()` handoff to `tmux -L sm4c attach-session -t sm4c:@N`. Every element is sm4c-constructed (preflight-validated tmux path + sm4c-owned session name + window ID of the form `@` + digits) so no shell layer is ever involved.
- M2c: `cmd/sm4c/cli/launch.go` + `root.go` routing — `sm4c [claude-args…]` (with positional args) runs preflight, spawns a tagged claude window via `NewClaudeWindow`, and `syscall.Exec`s into `tmux attach-session`. Root command uses `cobra.ArbitraryArgs` so non-subcommand positionals (`sm4c /help`, `sm4c -- -n my-session`) are forwarded verbatim rather than rejected with "unknown command". The exec handoff is isolated behind an `execAttach` function variable so unit tests can exercise the routing without replacing the test process. The shared spawn+attach core is factored into `spawnAndAttach` so both the shell shortcut and the TUI's new-session action realize sessions through a single code path.
- M2c-refinement: `internal/tui` — first Bubble Tea front end. Bare `sm4c` (no args, no subcommand) now opens a minimal empty-state view with `n` (new session), `?` (toggle help), and `q` / `Ctrl+C` (quit) bindings rather than silently spawning a claude session. The TUI is side-effect-free (no tmux, no subprocess, no filesystem) and signals "new session" to its caller via a typed `Action` intent; `cmd/sm4c/cli/tui.go` realizes the intent through `spawnAndAttach`. All styling uses `Bold` / `Faint` / `Reverse` on the user's terminal palette — zero hex colors, enforced by the existing CI gate. Unit tests cover every key binding, the help-toggle flow, unknown-key no-op, window-resize no-op, and the post-quit empty-view contract. CLI dispatch tests pin that bare `sm4c` routes through the TUI path (not the launch path) and that a missing claude fails fast before the TUI ever paints. Sessions-list view and the "pick target folder + name" new-session flow are deferred to M3; until then `n` spawns a bare `claude` (equivalent to the previous M2c bare-shell behavior) with a `TODO(M3)` marker in code.


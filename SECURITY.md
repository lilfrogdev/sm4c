# Security Policy

sm4c is a local TUI that hosts the Claude Code CLI inside an isolated `tmux` server. This file documents the threat model, the secure-coding practices enforced in the codebase and CI, and how to report security issues.

## Reporting a Vulnerability

Please **do not** open public GitHub issues for suspected security problems. A public issue can tip off attackers before a fix is available.

Report privately via either of these channels:

- Email **dev@lilfrogdev.com** (preferred).
- Use GitHub's private vulnerability reporting flow ("Report a vulnerability" under this repository's Security tab).

We aim to acknowledge reports within 5 business days and to provide a remediation or status update within 30 days. Please include:

- sm4c version (`sm4c version`)
- tmux version (`tmux -V`)
- OS and architecture
- Steps to reproduce, expected vs. observed behavior
- Any logs, with sensitive content redacted

## Supported Versions

During v0 / pre-alpha, only the latest commit on `main` is supported. Starting with v1.0 we will document supported release branches here.

## Scope

In scope:

- The `sm4c` binary (TUI and CLI).
- Everything under `cmd/` and `internal/`.
- The CI supply chain (GitHub Actions workflows, Makefile, dependency manifests).
- Documented config file formats.

Out of scope:

- Vulnerabilities in `tmux`, `claude`, `go`, or third-party dependencies themselves. Report those upstream. sm4c will bump once a fix is available.
- Local-user-to-local-user attacks where the attacker already has the same UID as the victim. sm4c assumes the local UID is the trust boundary; it does not defend against an adversary who already owns your account.
- Terminal emulator bugs. sm4c sanitizes outgoing output to mitigate terminal escape-sequence attacks, but we can't fix a broken terminal.

## Threat Model

sm4c's attack surface is shaped by the fact that it proxies a third-party process (`claude`) whose output can be indirectly influenced by remote content (user prompts, web search results, tool output). We treat everything coming from `claude` as untrusted input.

Primary threats we explicitly defend against:

1. **Outer-terminal escape-sequence injection.** A hostile `claude` output (e.g. reflected from a poisoned tool result) could try to emit `ESC]` / `ESC[` sequences that change the user's outer terminal state (clipboard, title, cursor, `DECSET` modes). sm4c renders `claude` output through a VT emulator and only forwards a whitelisted subset of escapes to the outer terminal.
2. **Command injection into `tmux` / `claude`.** Session names, window names, user-provided args, and config values are the primary injection vectors. sm4c never uses `sh -c`; every subprocess is built with `exec.Command(bin, args...)` where each argument is its own slice element. All strings bound for `tmux` arguments pass through `internal/safe`.
3. **Path hijacking / TOCTOU on binaries.** `tmux` and `claude` are resolved via `exec.LookPath` once at startup, the resolved absolute path is recorded, and subsequent invocations use that absolute path. `PATH` is not re-consulted per-call.
4. **Socket squatting.** sm4c's tmux socket lives under a per-user directory whose path and mode are validated at startup (owner = current UID, mode `0700`). sm4c refuses to start if the socket directory is world- or group-writable, or owned by another user.
5. **Keystroke cross-routing.** Keys intended for one `claude` session must never reach another. sm4c binds every `send-keys` call to an explicit `{sessionID, windowID}` target captured from a verified `%window-add` event; stale targets are rejected.
6. **Sensitive data leakage via logs.** At default log level (`INFO`) sm4c never writes keystrokes, session content, or model output to disk. `--debug` raises the level and prints a conspicuous warning; even at `--debug` we redact common secret patterns before logging.
7. **Supply-chain risks.** Dependencies are minimized, pinned via `go.sum`, scanned by `govulncheck` in CI, and built with `-trimpath -buildvcs=true` for reproducibility.

## Secure-Coding Practices (enforced in CI)

The following rules are enforced by CI and/or `golangci-lint`:

- **No shell interpolation.** `os/exec` with `sh -c` / `bash -c` is banned. A repo grep in CI flags any new occurrence.
- **No network.** sm4c must not import `net/http`, `net/smtp`, `net/rpc`, call `net.Dial*`, or open listeners. A repo grep in CI flags imports outside the allowlist.
- **No hex colors.** Terminal-native theming is mandatory; `lipgloss.Color("#...")` is banned via a repo grep.
- **`go vet`, `staticcheck`, `golangci-lint` (with `gosec`)** must pass with zero findings.
- **`govulncheck`** must pass with zero findings on `main`.
- **Reproducible builds.** Release builds use `go build -trimpath -buildvcs=true -ldflags='-s -w'`.
- **No `init()` side effects** outside of cobra subcommand registration.
- **`internal/safe`** is the single entry point for user-controlled strings crossing into tmux args or terminal output. Unit tests cover escape injection, BIDI overrides, C0/C1 controls, and length limits.

## What sm4c Does Not Do

- sm4c does not read, store, forward, or log your Anthropic API key. The key is handled entirely by `claude`.
- sm4c does not auto-update, phone home, or emit telemetry.
- sm4c does not persist any session transcripts. Scrollback lives in tmux's memory and is scrubbed on session close.
- sm4c does not accept input on any network socket.

## Cryptographic Material

sm4c does not generate, store, or rotate cryptographic keys. Session identifiers are tmux-assigned and treated as opaque.

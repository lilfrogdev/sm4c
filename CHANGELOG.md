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

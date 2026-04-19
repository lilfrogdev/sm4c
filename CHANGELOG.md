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

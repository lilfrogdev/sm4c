#!/usr/bin/env bash
# check-no-hex-colors.sh — enforce terminal-native theming.
#
# sm4c intentionally renders in the user's terminal colors. Adding hex
# colors breaks that promise silently. This guard catches the usual culprit:
# lipgloss.Color("#rrggbb"). If you believe you need a hex color, update
# this script AND update README.md's theming statement in the same PR.

set -euo pipefail

cd "$(dirname "$0")/.."

PATTERN='lipgloss\.Color\("#'

if command -v rg >/dev/null 2>&1; then
  hits=$(rg --no-heading --line-number --glob '*.go' --glob '!vendor/**' -- "$PATTERN" . || true)
else
  hits=$(grep -REn --include='*.go' --exclude-dir=vendor -- "$PATTERN" . || true)
fi

if [[ -n "$hits" ]]; then
  echo "hex-color check: banned call found:"
  echo "$hits"
  echo
  echo "sm4c must use terminal-native colors only. Prefer lipgloss.Color(\"9\"),"
  echo "named ANSI colors (0-15), or style.Bold/Faint/Reverse for emphasis."
  exit 1
fi

echo "hex-color check: OK"

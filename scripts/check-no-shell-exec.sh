#!/usr/bin/env bash
# check-no-shell-exec.sh — fail if sm4c ever shells out through /bin/sh.
#
# sm4c's command-injection posture relies on never invoking a shell. All
# subprocess calls must use exec.Command(bin, args...) directly, with every
# argument as its own slice element. If you legitimately need to run a
# user-provided string through a shell, that is a scope change — update
# SECURITY.md's "No shell interpolation" clause first.

set -euo pipefail

cd "$(dirname "$0")/.."

# Match common shell-exec patterns in Go source.
BANNED_PATTERNS=(
  'exec\.Command\("sh"'
  'exec\.Command\("/bin/sh"'
  'exec\.Command\("bash"'
  'exec\.Command\("/bin/bash"'
  'exec\.CommandContext\([^,]+,\s*"sh"'
  'exec\.CommandContext\([^,]+,\s*"/bin/sh"'
  'exec\.CommandContext\([^,]+,\s*"bash"'
  'exec\.CommandContext\([^,]+,\s*"/bin/bash"'
)

fail=0
for pat in "${BANNED_PATTERNS[@]}"; do
  if command -v rg >/dev/null 2>&1; then
    hits=$(rg --no-heading --line-number --glob '*.go' --glob '!vendor/**' -- "$pat" . || true)
  else
    hits=$(grep -REn --include='*.go' --exclude-dir=vendor -- "$pat" . || true)
  fi
  if [[ -n "$hits" ]]; then
    echo "no-shell-exec check: banned pattern $pat found:"
    echo "$hits"
    fail=1
  fi
done

if [[ "$fail" -ne 0 ]]; then
  echo
  echo "sm4c must not invoke a shell. Use exec.Command(bin, arg1, arg2, ...)"
  echo "with every argument as its own slice element."
  exit 1
fi

echo "no-shell-exec check: OK"

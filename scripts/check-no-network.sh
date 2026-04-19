#!/usr/bin/env bash
# check-no-network.sh — fail if sm4c gains a network dependency.
#
# sm4c's security story depends on "this process does not make network calls".
# This guard catches accidental imports (e.g. a dependency pulling in an HTTP
# client) during code review, before CI bless.
#
# Allowlist: nothing. If you need to add a net-adjacent stdlib import, update
# the allowlist *and* the threat model in SECURITY.md in the same PR.

set -euo pipefail

cd "$(dirname "$0")/.."

# Imports that prove network capability. Match the literal import path
# (quoted) inside .go files only.
BANNED_PATTERNS=(
  '"net/http"'
  '"net/http/httptest"'
  '"net/rpc"'
  '"net/smtp"'
  '"net/mail"'
  '"net/url"'
  '"net"'
)

RG="rg"
if ! command -v rg >/dev/null 2>&1; then
  RG="grep -R"
fi

fail=0
for pat in "${BANNED_PATTERNS[@]}"; do
  if command -v rg >/dev/null 2>&1; then
    hits=$(rg --no-heading --line-number --glob '*.go' --glob '!vendor/**' -F -- "$pat" . || true)
  else
    hits=$(grep -Rn --include='*.go' --exclude-dir=vendor -- "$pat" . || true)
  fi
  if [[ -n "$hits" ]]; then
    echo "no-network check: banned import $pat found:"
    echo "$hits"
    fail=1
  fi
done

if [[ "$fail" -ne 0 ]]; then
  echo
  echo "sm4c must not import network-capable packages. If this is intentional,"
  echo "update scripts/check-no-network.sh *and* SECURITY.md together."
  exit 1
fi

echo "no-network check: OK"

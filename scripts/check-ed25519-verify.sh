#!/usr/bin/env bash
# Gate: crypto/ed25519 Verify must only be called from the vetted verification
# packages, so every consumer goes through security/signedenvelope (domain
# separation, key scoping, typed errors) instead of re-implementing it.
#
# Usage (from any repo root; works for kernel, ops and hub):
#   check-ed25519-verify.sh [--baseline FILE] [--allow REGEX]
#
#   --allow     extended regex of paths allowed to call ed25519.Verify
#               (default: ^(security|licensing/sigv2)/ ; ops/hub pass --allow "^$" so nothing is exempt).
#   --baseline  file with one tolerated path per line (# comments allowed).
#               Transitional: ops/hub start with their current sites listed
#               and the file only ever shrinks. A path in the baseline that no
#               longer calls ed25519.Verify is reported as STALE and fails, so
#               the list cannot rot.
#
# Test files are ignored. Comment-only lines are ignored.
set -euo pipefail

allow='^(security|licensing/sigv2)/'
baseline=''
while [ $# -gt 0 ]; do
  case "$1" in
    --allow) allow="$2"; shift 2 ;;
    --baseline) baseline="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

root="$(git rev-parse --show-toplevel)"
cd "$root"

hits="$(git grep -nE 'ed25519\.Verify\(' -- '*.go' ':!*_test.go' ':!vendor/' \
  | awk -F: '{ line=$0; sub(/^[^:]*:[^:]*:/, "", line); if (line ~ /^[[:space:]]*\/\//) next; print $1 }' \
  | sort -u || true)"

tolerated=''
if [ -n "$baseline" ] && [ -f "$baseline" ]; then
  tolerated="$(grep -vE '^[[:space:]]*(#|$)' "$baseline" | sort -u || true)"
fi

fail=0
while IFS= read -r f; do
  [ -z "$f" ] && continue
  if echo "$f" | grep -Eq "$allow"; then continue; fi
  if [ -n "$tolerated" ] && echo "$tolerated" | grep -Fxq "$f"; then continue; fi
  echo "FORBIDDEN ed25519.Verify outside security/: $f" >&2
  fail=1
done <<< "$hits"

if [ -n "$tolerated" ]; then
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if ! echo "$hits" | grep -Fxq "$f"; then
      echo "STALE baseline entry (no longer calls ed25519.Verify, remove it): $f" >&2
      fail=1
    fi
  done <<< "$tolerated"
fi

if [ "$fail" -ne 0 ]; then
  echo "Use security/signedenvelope (Verifier.Open / VerifyLegacy / VerifyDetached)." >&2
  exit 1
fi
echo "ed25519.Verify gate: OK"

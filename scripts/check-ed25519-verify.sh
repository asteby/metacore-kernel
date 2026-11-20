#!/usr/bin/env bash
# Gate wrapper. Usage: scripts/check-ed25519-verify.sh [base-ref]
# base-ref (default origin/main when it exists) makes "baseline only shrinks" real.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
base="${1:-}"
if [ -z "$base" ] && git rev-parse --verify -q origin/main >/dev/null; then base=origin/main; fi
args=(-allowed scripts/ed25519-verify-allowed.txt -baseline scripts/ed25519-verify-baseline.txt)
[ -n "$base" ] && args+=(-base "$base")
GOWORK=off exec go run ./scripts/ed25519gate "${args[@]}"

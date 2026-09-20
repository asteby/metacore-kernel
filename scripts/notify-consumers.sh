#!/usr/bin/env bash
# Manual release-train helper: open bump-tracking issues in consumer repos
# for an already-pushed metacore-kernel tag. Mirrors notify-consumers.yml.
#
# Usage:
#   ./scripts/notify-consumers.sh v0.144.0
#
# Requires: gh auth with issues:write on asteby-hq/ops and asteby-hq/hub
#           (or GH_TOKEN / CONSUMER_BUMP_TOKEN in the environment).
set -euo pipefail

TAG="${1:-}"
if [ -z "$TAG" ]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 2
fi

MODULE="github.com/asteby/metacore-kernel"
TITLE="chore: bump metacore-kernel to ${TAG}"
BODY=$(cat <<EOF
## Kernel release \`${TAG}\`

A new \`metacore-kernel\` tag was pushed. Please bump the Go module pin.

\`\`\`bash
go env -w GOPRIVATE="github.com/asteby/*"
go get ${MODULE}@${TAG}
go mod tidy
\`\`\`

### Suggested PR title
\`chore: bump metacore-kernel to ${TAG}\`

— opened by metacore-kernel/scripts/notify-consumers.sh
EOF
)

CONSUMERS=("asteby-hq/ops" "asteby-hq/hub")
for repo in "${CONSUMERS[@]}"; do
  echo "==> ${repo}"
  existing=$(gh issue list --repo "$repo" --state open --search "$TITLE in:title" --json number --jq '.[0].number // empty' || true)
  if [ -n "$existing" ]; then
    gh issue comment "$existing" --repo "$repo" --body "Re-notified for \`${TAG}\`."
    echo "commented on #${existing}"
  else
    gh issue create --repo "$repo" --title "$TITLE" --body "$BODY" || \
      gh issue create --repo "$repo" --title "$TITLE" --body "$BODY"
  fi
done

#!/usr/bin/env bash
# Copia el esquema y los vectores de conformidad de genui/v2 desde el SDK
# (fuente unica: packages/genui/schema + packages/genui/conformance) y fija su
# hash en genui/v2/testdata/conformance.lock. `go test ./genui/v2` falla si los
# archivos no coinciden con el lock (edicion a mano o deriva).
#   scripts/sync-genui-vectors.sh /ruta/a/metacore-sdk
set -euo pipefail
SDK="${1:?uso: $0 /ruta/a/metacore-sdk}"
SRC="$SDK/packages/genui"
KERNEL="$(cd "$(dirname "$0")/.." && pwd)"
DST="$KERNEL/genui/v2"
[ -f "$SRC/schema/genui-v2.schema.json" ] || { echo "no encuentro $SRC/schema" >&2; exit 1; }
mkdir -p "$DST/schema" "$DST/testdata/conformance"
cp "$SRC/schema/genui-v2.schema.json" "$DST/schema/"
for d in valid invalid upgrade downgrade; do
  rm -rf "$DST/testdata/conformance/$d"
  cp -R "$SRC/conformance/$d" "$DST/testdata/conformance/$d"
done
cp "$SRC/conformance/README.md" "$DST/testdata/conformance/README.md"
SHA="$(git -C "$SDK" rev-parse HEAD 2>/dev/null || echo unknown)"
HASH="$(cd "$DST" && { find schema testdata/conformance -type f -name '*.json' | LC_ALL=C sort | while read -r f; do printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$f"; done; } | sha256sum | cut -d' ' -f1)"
printf 'sdk_commit %s\ncontent_sha256 %s\n' "$SHA" "$HASH" > "$DST/testdata/conformance.lock"
echo "genui/v2: vectores sincronizados (sdk $SHA, sha256 $HASH)"

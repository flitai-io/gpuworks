#!/bin/sh
set -eu

# Soft budget on the size of the built web UI (ui_bundle_gzip_kb, a "warn" key
# in scripts/arch-constraints.txt). Run after `npm run build`: it measures the
# gzip-compressed tar of internal/server/webui/dist, the same way
# scripts/package_release.sh packs it, and warns (never fails) when the total
# is over budget, listing the largest assets. Rationale and how to update the
# budget: scripts/arch-constraints.md (ui_bundle_gzip_kb).

cd "$(dirname "$0")/.."

DIST="internal/server/webui/dist"
BUDGET_FILE="scripts/arch-constraints.txt"
KEY="ui_bundle_gzip_kb"

if [ ! -f "$DIST/index.html" ]; then
  echo "ui bundle size: $DIST is not built; run 'make ui' first (skipped)"
  exit 0
fi

limit=$(awk -F'\t' -v k="$KEY" '$1 == k {print $2; exit}' "$BUDGET_FILE")
bytes=$(tar -C "$DIST" -czf - . | wc -c | tr -d '[:space:]')
kb=$(( (bytes + 1023) / 1024 ))

if [ -z "$limit" ]; then
  echo "ui bundle size: ${kb} KB gzip (no $KEY budget in $BUDGET_FILE)"
  exit 0
fi

echo "ui bundle size: ${kb}/${limit} KB gzip (warn)"
if [ "$kb" -gt "$limit" ]; then
  echo "WARNING: the UI bundle is over its soft budget (${kb} KB > ${limit} KB gzip)." >&2
  echo "  Largest assets (raw KB / gzip KB):" >&2
  find "$DIST" -type f -exec wc -c {} + | awk '$2 != "total" {print $1 "\t" $2}' \
    | sort -rn | head -5 | while IFS="$(printf '\t')" read -r size path; do
      gz=$(gzip -c "$path" | wc -c | tr -d '[:space:]')
      printf '    %6d / %6d  %s\n' $(( (size + 1023) / 1024 )) $(( (gz + 1023) / 1024 )) "${path#"$DIST"/}" >&2
    done
  echo "  Split or trim the bundle; if the growth is intended, raise $KEY in $BUDGET_FILE with a '# raised: <reason>' comment." >&2
fi
exit 0

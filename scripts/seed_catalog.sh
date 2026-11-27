#!/bin/sh
# Run from the module root. "manifest" writes the canonical CI artifact;
# builds and tests hold a private copy for their entire compiler invocation.
set -eu

seed_dir=internal/server/inferencecatalog/seed
go=${GO:-go}

catalog_date() {
  if [ -n "${SEED_CATALOG_PUBLISHED_AT:-}" ]; then
    printf '%s\n' "$SEED_CATALOG_PUBLISHED_AT"
    return
  fi
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    if [ -e .git ]; then
      echo "cannot read Git history for the seed catalog; set SEED_CATALOG_PUBLISHED_AT" >&2
      return 1
    fi
    # An unpublished source archive has no commit timestamp.
    return 0
  fi
  revision=$(git log -1 --format=%H -- "$seed_dir/catalog") || return 1
  if [ -z "$revision" ]; then
    echo "no commit found for $seed_dir/catalog; set SEED_CATALOG_PUBLISHED_AT" >&2
    return 1
  fi
  shallow=$(git rev-parse --git-path shallow) || return 1
  if [ -s "$shallow" ] && grep -Fxq "$revision" "$shallow"; then
    echo "catalog history ends at a shallow boundary; deepen the checkout or set SEED_CATALOG_PUBLISHED_AT" >&2
    return 1
  fi
  # Rebases and backports can change the committer date of the same model edit.
  git show -s --format=%aI "$revision"
}

generate() {
  timestamp=$(catalog_date) || return 1
  CGO_ENABLED=0 GOOS= GOARCH= "$go" run ./cmd/gpuworks-pack-catalog seed-manifest --published-at "$timestamp"
}

if [ "$1" = manifest ]; then
  generate
  exit
fi

mkdir -p bin
stage=$(mktemp -d bin/seed-catalog.XXXXXX)
trap 'rm -rf "$stage"' EXIT
if [ -n "${SEED_CATALOG_MANIFEST:-}" ]; then
  manifest_source="SEED_CATALOG_MANIFEST=$SEED_CATALOG_MANIFEST"
  if [ ! -f "$SEED_CATALOG_MANIFEST" ] || [ ! -r "$SEED_CATALOG_MANIFEST" ]; then
    echo "$manifest_source must name a readable file" >&2
    exit 1
  fi
  cp "$SEED_CATALOG_MANIFEST" "$stage/manifest.yaml"
else
  manifest_source="the seed manifest generator"
  generate > "$stage/manifest.yaml"
fi
if [ ! -s "$stage/manifest.yaml" ]; then
  echo "empty seed manifest from $manifest_source" >&2
  exit 1
fi
# The child may change directories. The compiler overlay itself uses paths
# relative to the module root; both substitutions below are controlled names.
export SEED_CATALOG_MANIFEST="$PWD/$stage/manifest.yaml"
case "$1" in
  build|test)
    action=$1
    shift
    printf '{"Replace":{"%s/release-manifest.yaml":"%s/manifest.yaml"}}\n' "$seed_dir" "$stage" > "$stage/overlay.json"
    "$go" "$action" -overlay="$stage/overlay.json" "$@"
    ;;
  *) "$@" ;;
esac

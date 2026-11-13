#!/bin/sh
# Publish a release and the embedded catalogs to every distribution source
# (ADR 0040, docs/dev/release.md#发布通道).
#
#   scripts/publish_dist.sh <channel> <dist-dir>
#
# 0. A release id names one set of archives for good: files under releases/
#    are served as immutable and a CDN keeps the first copy it sees. When a
#    source already holds releases/<id>/ (the commit was built and published
#    before), its archives and build time are republished, not this build's.
# 1. Pipelines can finish out of order: when a source already serves a build
#    of the channel created after this one, nothing is published. Servers
#    order releases by build time, so the release channel also refuses a
#    version tag below the one it names: a patch to an older release line
#    published after a newer line would otherwise be offered as an upgrade.
# 2. gpuworks-dist assemble signs a fresh tree from the release archives and
#    the catalogs embedded in this checkout.
# 3. rclone copies the immutable files to every source first, refusing to
#    replace an object that holds other bytes (--immutable).
# 4. gpuworks-dist verify checks every public URL against the new index
#    before any source points at it, so a CDN copy that differs stops the
#    publish while the channel still names the previous build.
# 5. Each source's <channel>/index.json, <channel>/latest and
#    <channel>/manifest.json are switched, and the channel is read back from
#    every public URL; when that fails, every source is put back on its
#    previous pointers.
# 6. scripts/get.sh, the one-line installer, is copied to the root of every
#    source as get.sh.
# 7. Version-tag directories (vYY.M.PATCH) are kept forever. Of the nightly
#    directories, those beyond the newest $GPUWORKS_DIST_KEEP (default 30)
#    are removed, except the one either channel's latest names.
#
# Credentials come from the environment (protected CI variables) or from
# secrets/dist-publish.env; a worktree without it falls back to the main
# checkout's ../gpuworks/secrets/dist-publish.env:
#   GPUWORKS_DIST_SIGNING_KEY, GPUWORKS_DIST_SIGNING_KEY_ID
#   GPUWORKS_DIST_OSS_ACCESS_KEY_ID, GPUWORKS_DIST_OSS_SECRET_ACCESS_KEY
#   GPUWORKS_DIST_OBS_ACCESS_KEY_ID, GPUWORKS_DIST_OBS_SECRET_ACCESS_KEY
#   GPUWORKS_DIST_R2_ACCESS_KEY_ID, GPUWORKS_DIST_R2_SECRET_ACCESS_KEY, GPUWORKS_DIST_R2_ENDPOINT
set -eu

channel=${1:?usage: publish_dist.sh <channel> <dist-dir>}
dist=${2:?usage: publish_dist.sh <channel> <dist-dir>}

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

# The signing key comes from the same environment file as the storage keys.
for env_file in secrets/dist-publish.env ../gpuworks/secrets/dist-publish.env; do
  if [ -z "${GPUWORKS_DIST_SIGNING_KEY:-}" ] && [ -f "$env_file" ]; then
    # shellcheck disable=SC1090
    . "$env_file"
  fi
done
: "${GPUWORKS_DIST_SIGNING_KEY:?set GPUWORKS_DIST_SIGNING_KEY (base64 ed25519 seed)}"
# shellcheck source=scripts/dist_remotes.sh
. ./scripts/dist_remotes.sh
export GPUWORKS_DIST_SIGNING_KEY GPUWORKS_DIST_SIGNING_KEY_ID

rclone=${RCLONE:-rclone}
keep=${GPUWORKS_DIST_KEEP:-30}

built_at=${GPUWORKS_BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
commit=${GPUWORKS_BUILD_COMMIT:-$(git rev-parse HEAD)}

work=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-dist.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
# Remotes live in the environment; an empty config file keeps rclone from
# reading (or complaining about) the operator's own.
: >"$work/rclone.conf"
export RCLONE_CONFIG="$work/rclone.conf"
go build -o "$work/gpuworks-dist" ./cmd/gpuworks-dist

release_id=$("$work/gpuworks-dist" release-id -dist "$dist")
published="$work/published"
adopted_release=false
for remote in $remotes; do
  rm -rf "$published"
  # A release not published yet is an empty listing (or "directory not
  # found"); either way nothing is copied.
  "$rclone" copy "$remote/releases/$release_id" "$published" 2>/dev/null || true
  if [ -z "$(ls -A "$published" 2>/dev/null)" ]; then
    continue
  fi
  if adopted=$("$work/gpuworks-dist" adopt -dir "$published" -release-id "$release_id"); then
    echo "release $release_id is already on $remote (built $adopted); republishing those archives, not this build's"
    dist=$published
    built_at=$adopted
    adopted_release=true
    break
  fi
  # The upload below refuses to replace it, so the publish stops there.
  echo "$remote holds a release $release_id that does not check out against its SHA256SUMS"
done

# Only a new formal release needs handbooks. Preserve an adopted release
# byte-for-byte, including legacy releases that never carried documentation.
if [ "$channel" = release ] && [ "$adopted_release" = false ] && [ ! -f "$dist/docs/SHA256SUMS" ]; then
  echo "formal releases require the versioned Chinese and English PDF handbooks" >&2
  exit 1
fi

set --
for url in $publics; do
  set -- "$@" -source "$url"
done
if newer=$("$work/gpuworks-dist" newer -channel "$channel" -built-at "$built_at" "$@"); then
  echo "not publishing: $newer, after this build ($built_at)"
  exit 0
fi

if [ "$channel" = release ]; then
  for remote in $remotes; do
    current=$("$rclone" cat "$remote/release/latest" 2>/dev/null || true)
    if [ -n "$current" ] && [ "$current" != "$release_id" ] &&
      [ "$(printf '%s\n%s\n' "$current" "$release_id" | sort -V | tail -n 1)" != "$release_id" ]; then
      echo "not publishing: $remote's release channel names $current, a later version than $release_id" >&2
      exit 1
    fi
  done
fi

pointers="$channel/index.json $channel/latest $channel/manifest.json"

tree="$work/tree"
"$work/gpuworks-dist" assemble -channel "$channel" -dist "$dist" -out "$tree" -release-id "$release_id" \
  -commit "$commit" -built-at "$built_at"

immutable='Cache-Control: public, max-age=31536000, immutable'
pointer='Cache-Control: no-cache'
for remote in $remotes; do
  echo "uploading files to $remote"
  "$rclone" copy "$tree" "$remote" --checksum --immutable --header-upload "$immutable" \
    --exclude "/*/index.json" --exclude "/*/latest" --exclude "/*/manifest.json"
  if [ -d "$tree/releases/$release_id/docs" ]; then
    # PDFs are release sidecars, outside the server update index. Read every
    # byte back before switching channel pointers, rather than just HEAD size.
    "$rclone" check "$tree/releases/$release_id/docs" "$remote/releases/$release_id/docs" --download --one-way
  fi
done

echo "checking every source serves the files of the new index"
"$work/gpuworks-dist" verify -channel "$channel" -index "$tree/$channel/index.json" "$@"

# What each source points at now, to put back if the switch does not read
# back. A source the channel is new on has no previous pointers.
previous="$work/previous"
n=0
for remote in $remotes; do
  n=$((n + 1))
  mkdir -p "$previous/$n/$channel"
  for rel in $pointers; do
    status=0
    "$rclone" copyto "$remote/$rel" "$previous/$n/$rel" 2>/dev/null || status=$?
    case $status in
      0) ;;
      3 | 4) rm -f "$previous/$n/$rel" ;; # not there: put back as absent
      *)
        echo "cannot read $remote/$rel (rclone exit $status); not switching anything" >&2
        exit 1
        ;;
    esac
  done
done
restore() {
  n=0
  for remote in $remotes; do
    n=$((n + 1))
    for rel in $pointers; do
      if [ -f "$previous/$n/$rel" ]; then
        "$rclone" copyto "$previous/$n/$rel" "$remote/$rel" --header-upload "$pointer" || true
      else
        "$rclone" deletefile "$remote/$rel" 2>/dev/null || true
      fi
    done
    echo "put $remote back on its previous pointers"
  done
}

switched=true
for remote in $remotes; do
  echo "switching $remote to $release_id"
  for rel in $pointers; do
    if ! "$rclone" copyto "$tree/$rel" "$remote/$rel" --header-upload "$pointer"; then
      switched=false
      break
    fi
  done
  $switched || break
done

if ! $switched || ! "$work/gpuworks-dist" verify -channel "$channel" -release-id "$release_id" "$@"; then
  restore
  echo "the channel did not switch to $release_id cleanly; every source is back on its previous pointers" >&2
  exit 1
fi

for remote in $remotes; do
  "$rclone" copyto scripts/get.sh "$remote/get.sh" --header-upload "$pointer"
done

for remote in $remotes; do
  set --
  for name in nightly release; do
    id=$("$rclone" cat "$remote/$name/latest" 2>/dev/null || true)
    if [ -n "$id" ]; then
      set -- "$@" -protect "$id"
    fi
  done
  if ! "$rclone" lsjson -R --files-only "$remote/releases" >"$work/listing.json"; then
    echo "[]" >"$work/listing.json"
  fi
  for old in $("$work/gpuworks-dist" prune -keep "$keep" "$@" <"$work/listing.json"); do
    echo "pruning $remote/releases/$old"
    "$rclone" purge "$remote/releases/$old"
  done
done
echo "published $channel $release_id to: $publics"

#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

release_id=${GPUWORKS_RELEASE_ID:-$(git describe --tags --always 2>/dev/null || echo dev)}
# The channel the binaries follow for upgrades (ADR 0040): nightly for
# default-branch builds, release for version tags.
channel=${GPUWORKS_RELEASE_CHANNEL:-nightly}
# CI names the commit and one build time shared by every job of the run
# (GPUWORKS_BUILD_COMMIT, GPUWORKS_BUILT_AT); a local build uses its own.
commit=${GPUWORKS_BUILD_COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
build_at=${GPUWORKS_BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
dist_dir=${GPUWORKS_DIST_DIR:-$repo_dir/dist}
release_oses=${GPUWORKS_RELEASE_OSES:-"linux darwin"}
release_arches=${GPUWORKS_RELEASE_ARCHES:-"amd64 arm64"}
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-release.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT INT TERM

case "$release_id" in
  *[!A-Za-z0-9._+-]*|'')
    echo "invalid release id: $release_id" >&2
    exit 2
    ;;
esac

# A release build is named by its version tag, vYY.M.PATCH
# (docs/dev/release.md#版本号); publish_dist.sh orders the release channel by it.
case "$channel" in
  nightly) ;;
  release)
    if ! printf '%s\n' "$release_id" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
      echo "release channel build needs a vYY.M.PATCH release id, got: $release_id" >&2
      exit 2
    fi
    # Every release ships with its notes (docs/dev/release.md#版本号): the
    # version's page in both languages and its row in the version list.
    for notes in "docs/releases/$release_id.md" "docs-en/releases/$release_id.md"; do
      if [ ! -f "$notes" ]; then
        echo "release $release_id has no release notes: add $notes before tagging" >&2
        exit 2
      fi
    done
    if ! grep -Fq "]($release_id.md)" docs/releases/README.md; then
      echo "release $release_id is not listed in docs/releases/README.md" >&2
      exit 2
    fi
    ;;
  *)
    echo "unknown release channel: $channel" >&2
    exit 2
    ;;
esac

for target_arch in $release_arches; do
  case "$target_arch" in
    amd64|arm64) ;;
    *)
      echo "unsupported release arch: $target_arch" >&2
      exit 2
      ;;
  esac
done
if [ -z "$(echo $release_arches)" ]; then
  echo "GPUWORKS_RELEASE_ARCHES must select at least one arch" >&2
  exit 2
fi

mkdir -p "$dist_dir"
target_count=0
seen_oses=" "
for target_os in $release_oses; do
  case "$target_os" in
    linux|darwin) ;;
    *)
      echo "unsupported release OS: $target_os" >&2
      exit 2
      ;;
  esac
  case "$seen_oses" in
    *" $target_os "*)
      echo "duplicate release OS: $target_os" >&2
      exit 2
      ;;
  esac
  seen_oses="$seen_oses$target_os "
  target_count=$((target_count + 1))
  for target_arch in $release_arches; do
    make VERSION="$release_id" COMMIT="$commit" DATE="$build_at" CHANNEL="$channel" "server-${target_os}-${target_arch}-prebuilt-ui"
  done
done

if [ "$target_count" -eq 0 ]; then
  echo "GPUWORKS_RELEASE_OSES must select at least one OS" >&2
  exit 2
fi

for os in $release_oses; do
  for arch in $release_arches; do
    package="gpuworks-${release_id}-${os}-${arch}"
    package_dir="$work_dir/$package"
    mkdir -p "$package_dir"
    cp "bin/gpuworks-${os}-${arch}" "$package_dir/gpuworks"
    chmod 0755 "$package_dir/gpuworks"
    # The binary embeds third-party programs (NOTICE); their licenses require
    # the license texts to travel with every binary distribution.
    cp LICENSE NOTICE "$package_dir/"
    cp -R LICENSES "$package_dir/LICENSES"
    # The install defaults differ per OS (docs/reference/cli.md#gpuworks-install).
    case "$os" in
      linux)
        platform="Linux"
        default_dir="/opt/gpuworks/server"
        install_step="2. Extract it and install the control plane as a systemd service:

   ./gpuworks install

   It prints the URL, the admin credentials and the directories it created
   (default /opt/gpuworks/server). Useful flags: --addr IP:PORT, --dir DIR,
   --quiet. Where systemd is not running, the server runs as a background
   process instead. Without root or sudo, install to a writable directory:

   ./gpuworks install --dir ~/gpuworks/server --no-systemd"
        ;;
      darwin)
        platform="macOS"
        default_dir="~/gpuworks/server"
        install_step="2. Extract it and install the control plane:

   ./gpuworks install

   It prints the URL, the admin credentials and the directories it created
   (default ~/gpuworks/server). Useful flags: --addr IP:PORT, --dir DIR,
   --quiet. The server runs as a background process and does not start at
   boot. To install it as a launch daemon instead, run this from an account
   that can use sudo:

   ./gpuworks install --autostart"
        ;;
      *)
        echo "unsupported release OS: $os" >&2
        exit 2
        ;;
    esac
    cat > "$package_dir/INSTALL.txt" <<EOF
GPUWorks ${release_id} for ${platform} ${arch}

1. Verify the archive with SHA256SUMS.
${install_step}

3. Open the printed URL, create a node, and run the node-specific installation
   command shown by the UI. The server embeds the agents for Linux amd64 and
   arm64 and for macOS on Apple silicon.

Later upgrades run the installed binary, which is not on PATH:

   ${default_dir}/gpuworks upgrade

It backs up first and rolls back if the new version does not come up. A
systemd or launchd install asks for sudo to stop and restart the service and
to restore file ownership; a background install upgrades without sudo. A host
that cannot reach the distribution sources upgrades from a copied archive:

   ${default_dir}/gpuworks upgrade --archive gpuworks-<release>-${os}-${arch}.tar.gz

With a different --dir, use the gpuworks binary in that directory.

Build commit: ${commit}
Build time:   ${build_at}
Documentation: https://gpuworks.readthedocs.io/stable/en/how-to/install-server/
EOF
    tar -C "$work_dir" -czf "$dist_dir/$package.tar.gz" "$package"
  done
done

(
  cd "$dist_dir"
  set --
  for os in $release_oses; do
    for arch in $release_arches; do
      set -- "$@" "gpuworks-${release_id}-${os}-${arch}.tar.gz"
    done
  done
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@" > SHA256SUMS
  else
    shasum -a 256 "$@" > SHA256SUMS
  fi
)

# `arch` is also the release site's unique card key. Keep the legacy Linux
# values and give macOS distinct keys; `goarch` retains the exact Go target.
cat > "$dist_dir/release.json" <<EOF
{
  "release_id": "${release_id}",
  "commit": "${commit}",
  "build_at": "${build_at}",
  "channel": "preview",
  "files": [
EOF

first_file=1
for os in $release_oses; do
  for arch in $release_arches; do
    filename="gpuworks-${release_id}-${os}-${arch}.tar.gz"
    file_sha=$(awk -v file="$filename" '$2 == file {print $1}' "$dist_dir/SHA256SUMS")
    file_size=$(wc -c < "$dist_dir/$filename" | tr -d ' ')
    case "$os" in
      linux) site_arch="$arch" ;;
      darwin) site_arch="macos-$arch" ;;
    esac
    if [ "$first_file" -eq 0 ]; then
      printf ',\n' >> "$dist_dir/release.json"
    fi
    first_file=0
    printf '    {"os": "%s", "arch": "%s", "goarch": "%s", "name": "%s", "size": %s, "sha256": "%s"}' \
      "$os" "$site_arch" "$arch" "$filename" "$file_size" "$file_sha" >> "$dist_dir/release.json"
  done
done

cat >> "$dist_dir/release.json" <<EOF

  ],
  "checksums": "SHA256SUMS"
}
EOF

echo "release packages written to $dist_dir"

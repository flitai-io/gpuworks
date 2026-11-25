#!/usr/bin/env bash
# Build a macOS (darwin/arm64) environment package for the agent's host
# runtime driver: an OCI image whose single layer is a relocatable root with a
# python-build-standalone interpreter at /python, the engine installed into
# that interpreter's own site-packages, and launchers in /bin. It is built on
# Linux - uv resolves and installs the macOS wheels by target platform - so CI
# needs no Mac. See README.md for the contract the agent relies on.
#
# usage: build.sh <engine> <version> <revision> <output.tar>
#   engine    vllm-metal | mlx-vlm | mlx-audio | llama.cpp
#   version   the engine release (vllm-metal: its GitHub tag without "v";
#             llama.cpp: its bNNNNN build; others: the PyPI version)
#   revision  the build revision the runner matrix records for this package
#             (r1, r2, ...): given every time, so a rebuild never reuses a tag
#   output    docker-save archive, loadable by the agent and pushable with
#             `crane push <output.tar> <ref>`
#
# Environment:
#   GPUWORKS_HOSTENV_CACHE  download cache (default ~/.cache/gpuworks-hostenv)
#   GPUWORKS_HOSTENV_TAG    image tag written into the archive (default: the
#                           gpuworks-runner name, metal<macOS min>-<engine><version>-<revision>;
#                           CLAUDE.md "内置镜像命名")
#   UV_INDEX_URL            optional PyPI mirror for dependency resolution
#   GPUWORKS_HOSTENV_COMMIT mlx-vlm only: build from this upstream commit (a
#                           full 40-hex sha) instead of the PyPI release, for
#                           model support merged after the latest release.
#                           version is then the PEP 440 development version
#                           the matrix names the package by (0.8.1.dev20261121)
set -euo pipefail

engine=${1:?engine}
version=${2:?version}
revision=${3:?revision, as the runner matrix records it (r1, r2, ...)}
[[ $revision =~ ^r[0-9]+$ ]] || { echo "revision must look like r1, got $revision" >&2; exit 2; }
output=$(realpath -m "${4:?output tarball}")
commit=${GPUWORKS_HOSTENV_COMMIT:-}
if [[ -n $commit ]]; then
  [[ $engine == mlx-vlm ]] || { echo "GPUWORKS_HOSTENV_COMMIT applies to mlx-vlm only" >&2; exit 2; }
  [[ $commit =~ ^[0-9a-f]{40}$ ]] || { echo "GPUWORKS_HOSTENV_COMMIT must be a full 40-hex sha, got $commit" >&2; exit 2; }
fi

# Pinned inputs. Bump deliberately: the package is what a deployment runs.
PBS_RELEASE=20261009
PBS_PYTHON=3.12.15
UV_VERSION=0.13.0
MACOS_TARGET=15.0 # vllm-metal's floor; also MLX's. Equals accelerator.MinimumMacOSMajor (a test checks)
# Wheel tags derived from the pins above.
cp_tag=cp${PBS_PYTHON%.*} cp_tag=${cp_tag//./}  # cp312
macos_tag=macosx_${MACOS_TARGET//./_}           # macosx_15_0

cache=${GPUWORKS_HOSTENV_CACHE:-$HOME/.cache/gpuworks-hostenv}
# The runner tag names the engine as the image catalog does (vllm for
# vllm-metal) and the framework version as the minimum macOS.
# llama.cpp's builds are bNNNNN, so a "-" separates name and build
# (cmd/gpuworks-image-manifest's idGrammar; the pushed tag is releaseTagPattern).
sep=""
case $engine in vllm-metal) runner_engine=vllm ;; llama.cpp) runner_engine=llama.cpp sep=- ;; *) runner_engine=$engine ;; esac
tag=${GPUWORKS_HOSTENV_TAG:-gpuworks/gpuworks-runner:metal${MACOS_TARGET}-${runner_engine}${sep}${version}-${revision}}
mkdir -p "$cache"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root=$work/root

fetch() { # url dest
  [[ -s $2 ]] || { curl -fsSL --retry 3 -o "$2.part" "$1" && mv "$2.part" "$2"; }
}

# package writes the root as a one-layer darwin/arm64 image: owned by root
# with a fixed mtime, so equal inputs give an equal digest.
package() { # image env entries...
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2026-01-01 00:00:00Z' \
    -C "$root" -cf - . | gzip -n >"$work/layer.tar.gz"
  local repo env_args=()
  repo=$(cd "$(dirname "$0")/../.." && pwd)
  for e in "$@"; do env_args+=(-env "$e"); done
  (cd "$repo" && CGO_ENABLED=0 go run ./scripts/hostenvimage -layer "$work/layer.tar.gz" -tag "$tag" -o "$output" \
    -os darwin -arch arm64 "${env_args[@]}" -workdir "${workdir:-}" \
    -label "ai.gpuworks.host-env.engine=$engine" -label "ai.gpuworks.host-env.version=$version")
  echo "built $tag -> $output ($(du -h "$output" | cut -f1))"
}

# llama.cpp is native code: upstream's macOS release carries llama-server and
# the dylibs it loads from its own directory (the Metal shaders are embedded
# in libggml-metal). No Python and no adapter: llama-server takes --model and
# serves /health and the OpenAI routes itself.
if [[ $engine == llama.cpp ]]; then
  archive=$cache/llama-$version-bin-macos-arm64.tar.gz
  fetch "https://github.com/ggml-org/llama.cpp/releases/download/$version/llama-$version-bin-macos-arm64.tar.gz" "$archive"
  mkdir -p "$work/llama" "$root/bin"
  tar -xzf "$archive" -C "$work/llama"
  server=$(find "$work/llama" -name llama-server -type f | head -1)
  [[ -n $server ]] || { echo "no llama-server in $archive" >&2; exit 1; }
  cp -a "$(dirname "$server")"/. "$root/bin/"
  printf '{"engine": "%s", "version": "%s", "macos_min": "%s"}\n' "$engine" "$version" "$MACOS_TARGET" >"$root/gpuworks-env.json"
  package "PATH=/bin:/usr/bin:/bin"
  exit 0
fi

uv=$cache/uv-$UV_VERSION/uv
if [[ ! -x $uv ]]; then
  mkdir -p "$(dirname "$uv")"
  fetch "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/uv-$(uname -m)-unknown-linux-gnu.tar.gz" "$cache/uv-$UV_VERSION.tgz"
  tar -xzf "$cache/uv-$UV_VERSION.tgz" -C "$(dirname "$uv")" --strip-components 1
fi

pbs=$cache/cpython-$PBS_PYTHON+$PBS_RELEASE-aarch64-apple-darwin-install_only.tar.gz
fetch "https://github.com/astral-sh/python-build-standalone/releases/download/$PBS_RELEASE/cpython-$PBS_PYTHON%2B$PBS_RELEASE-aarch64-apple-darwin-install_only.tar.gz" "$pbs"
mkdir -p "$root"
tar -xzf "$pbs" -C "$root" # -> $root/python
site=$root/python/lib/python${PBS_PYTHON%.*}/site-packages

# What to install, and the launcher each engine's templates call.
requirements=()
overrides=() # uv overrides: replace a dependency that has no macOS arm64 wheel
workdir=""   # image WorkingDir, as the Linux runner of the same engine has it
launchers=()
case $engine in
vllm-metal)
  # vllm-metal pins the vLLM core it was built against; read the pin from the
  # release, as its own installer does.
  vllm_tag=$(curl -fsSL "https://raw.githubusercontent.com/vllm-project/vllm-metal/v$version/.github/vllm-release-tag.commit" | tr -d '[:space:]')
  vllm_version=${vllm_tag#v}
  # vLLM's CPU wheel is tagged with its own floor (macOS 11), not ours.
  core=$cache/vllm-$vllm_version+cpu-$cp_tag-$cp_tag-macosx_11_0_arm64.whl
  plugin=$cache/vllm_metal-$version-$cp_tag-$cp_tag-${macos_tag}_arm64.whl
  fetch "https://github.com/vllm-project/vllm/releases/download/$vllm_tag/vllm-$vllm_version%2Bcpu-$cp_tag-$cp_tag-macosx_11_0_arm64.whl" "$core"
  fetch "https://github.com/vllm-project/vllm-metal/releases/download/v$version/vllm_metal-$version-$cp_tag-$cp_tag-${macos_tag}_arm64.whl" "$plugin"
  # gguf: vllm-metal loads GGUF weights through it but does not declare it.
  requirements=("$core" "$plugin" gguf)
  # Templates name vLLM's bundled chat templates relative to the working
  # directory, as examples/<file>.jinja, which the Linux runners run in
  # (/vllm-workspace). The wheel does not ship them; take them from the
  # release's source.
  src=$cache/vllm-$vllm_version-src.tar.gz
  fetch "https://github.com/vllm-project/vllm/archive/refs/tags/$vllm_tag.tar.gz" "$src"
  mkdir -p "$root/vllm-workspace/examples"
  # Only examples/ itself: a "*" that matched "/" would flatten nested files.
  tar -xzf "$src" -C "$root/vllm-workspace/examples" --strip-components 2 --wildcards --no-wildcards-match-slash "vllm-$vllm_version/examples/*.jinja"
  workdir=/vllm-workspace
  launchers=("vllm=vllm.entrypoints.cli.main")
  ;;
mlx-vlm)
  requirements=("mlx-vlm==$version")
  if [[ -n $commit ]]; then
    # mlx-vlm is pure Python: build its wheel from the commit's source and
    # install that (--no-build below refuses a git requirement).
    src=$cache/mlx-vlm-$commit.tar.gz
    fetch "https://github.com/Blaizzy/mlx-vlm/archive/$commit.tar.gz" "$src"
    mkdir -p "$work/mlx-vlm-src" "$work/wheels"
    tar -xzf "$src" -C "$work/mlx-vlm-src" --strip-components 1
    UV_CACHE_DIR=$cache/uv-cache "$uv" build --quiet --wheel -o "$work/wheels" "$work/mlx-vlm-src"
    requirements=("$(ls "$work"/wheels/mlx_vlm-*.whl)")
  fi
  launchers=("mlx_vlm.server=mlx_vlm.server" "gpuworks-mlx-vlm=gpuworks_adapters.mlx_vlm")
  ;;
mlx-audio)
  # misaki[en] and its spaCy model are optional dependencies some TTS models
  # import at first use, downloading the model with pip when missing; a
  # package must never need the network at run time.
  # webrtcvad publishes only an sdist, which a cross build cannot compile;
  # webrtcvad-wheels ships the same module prebuilt for macOS arm64.
  requirements=("mlx-audio[server]==$version" "misaki[en]" "webrtcvad-wheels==2.0.14.post1"
    "en_core_web_sm @ https://github.com/explosion/spacy-models/releases/download/en_core_web_sm-3.8.0/en_core_web_sm-3.8.0-py3-none-any.whl")
  overrides=("webrtcvad; sys_platform == 'never'")
  launchers=("mlx_audio.server=mlx_audio.server" "gpuworks-mlx-audio=gpuworks_adapters.mlx_audio")
  ;;
*)
  echo "unknown engine $engine" >&2
  exit 2
  ;;
esac

override_args=()
if [[ ${#overrides[@]} -gt 0 ]]; then
  printf '%s\n' "${overrides[@]}" >"$work/overrides.txt"
  override_args=(--override "$work/overrides.txt")
fi
MACOSX_DEPLOYMENT_TARGET=$MACOS_TARGET UV_CACHE_DIR=$cache/uv-cache "$uv" pip install \
  --quiet --target "$site" "${override_args[@]}" --no-build \
  --python-platform aarch64-apple-darwin --python-version "${PBS_PYTHON%.*}" \
  "${requirements[@]}"
# --target writes console scripts whose shebang is this Linux host's python.
rm -rf "$site/bin"
# The serve adapters inference templates start (adapters/gpuworks_adapters;
# their tests: adapters/test_*.py).
cp -R "$(dirname "$0")/adapters/gpuworks_adapters" "$site/"
find "$site/gpuworks_adapters" -name __pycache__ -prune -exec rm -rf {} +

mkdir -p "$root/bin"
for launcher in "${launchers[@]}"; do
  name=${launcher%%=*}
  module=${launcher#*=}
  printf '#!/bin/sh\n# gpuworks host environment launcher\nexec python3 -m %s "$@"\n' "$module" >"$root/bin/$name"
  chmod 0755 "$root/bin/$name"
done
cat >"$root/gpuworks-env.json" <<EOF
{"engine": "$engine", "version": "$version", "commit": "$commit", "python": "$PBS_PYTHON", "python_build": "$PBS_RELEASE", "macos_min": "$MACOS_TARGET"}
EOF

package "PATH=/bin:/python/bin:/usr/bin:/bin" "PYTHONNOUSERSITE=1"

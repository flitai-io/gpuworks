#!/usr/bin/env bash
# Builds the agent image a Kubernetes cluster's DaemonSet runs
# (build/agent-image/Dockerfile), locally or for a release.
#
#   agent_image.sh build      one architecture into the local docker (make agent-image)
#   agent_image.sh publish    amd64 + arm64, pushed to every registry (make agent-image-publish)
#
# The Makefile passes GO, LDFLAGS and VERSION, so the agent in the image is
# built exactly like the one the server embeds. The image is tagged with its
# runtime revision (build/agent-image/REVISION), which the server's manifest
# names, and with <revision>-<version> to tell the builds apart.
#
# nerdctl comes from its GitHub release, verified against the checksums below and kept in a cache directory:
# CI runners reach GitHub poorly, and a cached tarball is never fetched twice.
# NERDCTL_URL_BASE may list several download bases, tried in order; the
# checksum makes any mirror of the release as good as GitHub.
#
# Environment:
#   VERSION                    the server's version (required)
#   ARCH                       build: the architecture (amd64 | arm64)
#   AGENT_IMAGE                build: the image name (default gpuworks/gpuworks-agent:<revision>)
#   GPUWORKS_AGENT_IMAGE_REPOS publish: space-separated repositories to push to
#   GPUWORKS_AGENT_IMAGE_DOCKER_CONFIG
#                              publish: path to a docker config.json holding the
#                              registries' credentials (a CI file variable)
#   GPUWORKS_DOCKER_BUILD_NETWORK  build: docker build --network (default "default")
#   NERDCTL_VERSION, NERDCTL_URL_BASE, GPUWORKS_TOOL_CACHE, DEBIAN_MIRROR,
#   GPUWORKS_DOCKER_CLI_IMAGE, GPUWORKS_BUILDKIT_IMAGE  overrides
set -euo pipefail

mode="${1:-}"
: "${VERSION:?VERSION is required}"
: "${GO:=go}"
: "${LDFLAGS:=}"
NERDCTL_VERSION="${NERDCTL_VERSION:-2.1.6}"
NERDCTL_URL_BASE="${NERDCTL_URL_BASE:-https://github.com/containerd/nerdctl/releases/download}"
cache="${GPUWORKS_TOOL_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/gpuworks/tools}"
DEBIAN_MIRROR="${DEBIAN_MIRROR:-mirrors.aliyun.com}"
docker_cli_image="${GPUWORKS_DOCKER_CLI_IMAGE:-docker:29-cli}"
buildkit_image="${GPUWORKS_BUILDKIT_IMAGE:-moby/buildkit:v0.25.2}"
revision="$(tr -d '[:space:]' < build/agent-image/REVISION)"

# nerdctl_sha256 is the published SHA256SUMS entry of the pinned release.
nerdctl_sha256() {
  case "$NERDCTL_VERSION-$1" in
    2.1.6-amd64) echo 22857b373edea479a4534ba62cae1c77f2af38f0aac4c91c1c68cf09e29d6f9e ;;
    2.1.6-arm64) echo 5c30be3ec118eb222bf635f8049c2f96b4c46d9343ec445058e9bc2ee9531c28 ;;
    *)
      local var="NERDCTL_SHA256_$1"
      [ -n "${!var:-}" ] || { echo "no checksum for nerdctl $NERDCTL_VERSION $1: set $var" >&2; exit 2; }
      echo "${!var}"
      ;;
  esac
}

# fetch_nerdctl puts a verified nerdctl for $1 into $2.
fetch_nerdctl() {
  local arch="$1" dest="$2" name want got
  name="nerdctl-$NERDCTL_VERSION-linux-$arch.tar.gz"
  want="$(nerdctl_sha256 "$arch")"
  mkdir -p "$cache"
  if [ ! -f "$cache/$name" ] || [ "$(sha256sum "$cache/$name" | cut -d' ' -f1)" != "$want" ]; then
    local tmp base ok=""
    tmp="$(mktemp "$cache/.$name.XXXXXX")"
    for base in $NERDCTL_URL_BASE; do
      if ! curl -fsSL --retry 2 --connect-timeout 20 -o "$tmp" "$base/v$NERDCTL_VERSION/$name"; then
        echo "$name: download from $base failed" >&2
        continue
      fi
      got="$(sha256sum "$tmp" | cut -d' ' -f1)"
      if [ "$got" = "$want" ]; then
        ok=1
        break
      fi
      echo "$name from $base: sha256 $got, want $want" >&2
    done
    if [ -z "$ok" ]; then
      rm -f "$tmp"
      echo "$name: no download base gave the release ($NERDCTL_URL_BASE)" >&2
      exit 1
    fi
    mv -f "$tmp" "$cache/$name"
  fi
  tar -xzf "$cache/$name" -C "$dest" nerdctl
}

# context builds <arch>/gpuworks-agent and <arch>/nerdctl into $1 for each
# architecture that follows.
context() {
  local ctx="$1" arch
  shift
  cp build/agent-image/Dockerfile "$ctx/"
  for arch in "$@"; do
    mkdir -p "$ctx/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$GO" build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
      -o "$ctx/$arch/gpuworks-agent" ./cmd/gpuworks-agent
    fetch_nerdctl "$arch" "$ctx/$arch"
  done
}

# tags_for turns the repositories into -t flags.
tags_for() {
  local repo
  for repo in $GPUWORKS_AGENT_IMAGE_REPOS; do
    printf -- '-t %s:%s -t %s:%s-%s ' "$repo" "$revision" "$repo" "$revision" "$VERSION"
  done
}

ctx="$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-agent-image.XXXXXX")"
trap 'rm -rf "$ctx"' EXIT

case "$mode" in
  build)
    arch="${ARCH:-amd64}"
    context "$ctx" "$arch"
    docker build --platform "linux/$arch" --network "${GPUWORKS_DOCKER_BUILD_NETWORK:-default}" --build-arg DEBIAN_MIRROR="$DEBIAN_MIRROR" \
      --build-arg AGENT_IMAGE_REVISION="$revision" \
      -t "${AGENT_IMAGE:-gpuworks/gpuworks-agent:$revision}" "$ctx"
    ;;
  publish)
    : "${GPUWORKS_AGENT_IMAGE_REPOS:?GPUWORKS_AGENT_IMAGE_REPOS names no repository to push to}"
    auth="${GPUWORKS_AGENT_IMAGE_DOCKER_CONFIG:-}"
    if [ -z "$auth" ] || [ ! -s "$auth" ]; then
      echo "GPUWORKS_AGENT_IMAGE_DOCKER_CONFIG is not set: no registry credentials to push the agent image with" >&2
      exit 1
    fi
    context "$ctx" amd64 arm64
    builder="gpuworks-agent-image-$$"
    # buildx runs from the docker CLI image against the host daemon, so the
    # host needs no buildx plugin. --provenance/--sbom off: SWR refuses an
    # index that carries attestations. Credentials reach buildkit through the
    # client's session, never the image.
    # shellcheck disable=SC2046
    docker run --rm --network host \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v "$ctx:$ctx:ro" -w "$ctx" -v "$(realpath "$auth"):/run/gpuworks-registry-auth.json:ro" \
      -e BUILDKIT_IMAGE="$buildkit_image" -e DEBIAN_MIRROR="$DEBIAN_MIRROR" -e REVISION="$revision" \
      "$docker_cli_image" sh -euc '
        builder="$1"; shift
        # buildx keeps its state in DOCKER_CONFIG: a directory of the
        # container, so nothing is left on the runner.
        mkdir -p /root/.docker && cp /run/gpuworks-registry-auth.json /root/.docker/config.json
        # buildkit runs in a container of its own and does not inherit the
        # daemon'"'"'s proxy; hand it the same one, so pulling the base and pushing
        # reach the registries the way the daemon does. The values are quoted
        # because buildx reads each --driver-opt as CSV and the no-proxy list
        # has commas; none of them holds a space.
        hp="$(docker info --format "{{.HTTPProxy}}")"
        sp="$(docker info --format "{{.HTTPSProxy}}")"
        np="$(docker info --format "{{.NoProxy}}")"
        # shellcheck disable=SC2086
        docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" --driver-opt network=host \
          ${hp:+--driver-opt "env.http_proxy=$hp"} ${sp:+--driver-opt "env.https_proxy=$sp"} ${np:+--driver-opt "\"env.no_proxy=$np\""} >/dev/null
        trap "docker buildx rm -f \"$builder\" >/dev/null 2>&1 || true" EXIT
        # The registries'"'"' tokens are fetched by this client for buildkit, so it
        # needs the proxy too.
        export HTTP_PROXY="$hp" HTTPS_PROXY="$sp" NO_PROXY="$np"
        docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
          --provenance=false --sbom=false --build-arg DEBIAN_MIRROR="$DEBIAN_MIRROR" --build-arg AGENT_IMAGE_REVISION="$REVISION" \
          "$@" --push .
      ' sh "$builder" $(tags_for)
    echo "pushed: $(tags_for | sed 's/-t //g')"
    ;;
  *)
    echo "usage: $0 build|publish" >&2
    exit 2
    ;;
esac

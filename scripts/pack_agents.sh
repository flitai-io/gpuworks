#!/bin/sh
# The first platform in the requested matrix supplies the raw zstd dictionary.
# Keep bin/dl as the independent, uncompressed recovery artifacts.
set -eu
dest=$1
shift
base=$1
stage=$(mktemp -d "${TMPDIR:-/tmp}/gpuworks-agent-pack.XXXXXX")
trap 'rm -rf "$stage"' EXIT
mkdir "$stage/pack"
basename "$base" > "$stage/pack/base"
pack() {
    agent=$1
    name=$(basename "$agent")
    out="$stage/pack/$name.zst"
    reference=$base
    if [ "$agent" = "$base" ]; then
        reference=
        set --
    else
        set -- "--patch-from=$base"
    fi
    if command -v zstd >/dev/null 2>&1 && zstd -19 -q "$@" "$agent" -o "$out"; then
        return
    fi
    "${GO:-go}" run ./scripts/zstdpack -base "$reference" -o "$out" "$agent"
}
for agent do
    pack "$agent"
done
"${GO:-go}" run ./scripts/agentmanifest -version "$VERSION" -o "$stage/agent-manifest.json" "$@"
rm -rf "$dest/pack"
rm -f "$dest"/gpuworks-agent-* "$dest/agent-manifest.json"
mv "$stage/pack" "$stage/agent-manifest.json" "$dest/"

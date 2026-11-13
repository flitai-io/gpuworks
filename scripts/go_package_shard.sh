#!/bin/sh
# Print one shard of a set of Go packages, one import path per line, so CI can
# split `go test ./...` across parallel jobs:
#
#   scripts/go_package_shard.sh <index>/<total> <go list args...>
#   go test $(scripts/go_package_shard.sh 2/3 ./...)
#
# Packages are sorted by import path and dealt round-robin, the same rule
# go_test_shard.sh applies to the tests of one package; neighbouring heavy
# packages (the integrationtest suites, server/api) land in different shards.
# Every package lands in exactly one shard. A shard with no packages fails, so
# an empty list never turns into a `go test` of the current directory.
set -eu

shard=${1:?usage: go_package_shard.sh <index>/<total> <go list args...>}
shift
index=${shard%/*}
total=${shard#*/}
case "$index$total" in
*[!0-9]* | "") echo "go_package_shard.sh: shard must be <index>/<total>, got $shard" >&2; exit 2 ;;
esac
if [ "$total" -lt 1 ] || [ "$index" -lt 1 ] || [ "$index" -gt "$total" ]; then
	echo "go_package_shard.sh: shard $shard is out of range" >&2
	exit 2
fi

list=$(${GO:-go} list "$@") || exit 1
packages=$(printf '%s\n' "$list" | LC_ALL=C sort -u | awk -v i="$index" -v n="$total" 'NF && (NR - 1) % n == i - 1')
if [ -z "$packages" ]; then
	echo "go_package_shard.sh: shard $shard has no packages" >&2
	exit 1
fi
printf '%s\n' "$packages"

#!/bin/sh
# Print a `go test -run` pattern selecting one shard of a suite's top-level
# tests, so CI can split a long single-package suite across parallel jobs.
#
#   scripts/go_test_shard.sh <index>/<total> <go test -list args...>
#   scripts/go_test_shard.sh 2/3 -tags=multiproc ./test/l2/...
#
# Every test lands in exactly one shard, and a shard with no tests prints a
# pattern that matches nothing rather than an empty one, which would match
# everything.
#
# With GPUWORKS_SHARD_WEIGHTS naming a file of "<TestName> <seconds>" lines the
# shards are balanced by measured duration: tests are taken heaviest first and
# each lands in the shard whose total is smallest, so one slow case no longer
# decides how long its shard runs. Tests the file does not know weigh nothing
# and are spread last, by name, over the shards that ended lightest. The file
# is a starting point, not a contract: test/l2/shard-weights.txt carries the
# durations of one CI run and is regenerated the same way.
#
# Without that file tests are sorted by name and dealt round-robin (the 1st,
# 4th, 7th... to shard 1 of 3), which spreads a family of slow cases sharing a
# name prefix across shards.
set -eu

shard=${1:?usage: go_test_shard.sh <index>/<total> <go test -list args...>}
shift
index=${shard%/*}
total=${shard#*/}
case "$index$total" in
*[!0-9]* | "") echo "go_test_shard.sh: shard must be <index>/<total>, got $shard" >&2; exit 2 ;;
esac
if [ "$total" -lt 1 ] || [ "$index" -lt 1 ] || [ "$index" -gt "$total" ]; then
	echo "go_test_shard.sh: shard $shard is out of range" >&2
	exit 2
fi

list=$(${GO:-go} test -list '.*' "$@") || {
	printf '%s\n' "$list" >&2
	exit 1
}
tests=$(printf '%s\n' "$list" | grep -E '^(Test|Example|Fuzz)[A-Za-z0-9_]*$' | LC_ALL=C sort -u)

weights=${GPUWORKS_SHARD_WEIGHTS:-}
if [ -n "$weights" ] && [ -f "$weights" ]; then
	if [ -s "$weights" ] && ! awk 'BEGIN { ok = 1 }
		{ sub(/#.*/, ""); if ($0 == "") next
		  if (NF != 2 || $2 !~ /^[0-9]+(\.[0-9]+)?$/) ok = 0 }
		END { exit ok ? 0 : 1 }' "$weights"; then
		echo "go_test_shard.sh: $weights must hold '<TestName> <seconds>' lines" >&2
		exit 2
	fi
	selected=$(printf '%s\n' "$tests" | awk -v wf="$weights" '
		BEGIN {
			while ((getline line < wf) > 0) {
				sub(/#.*/, "", line)
				if (split(line, f, /[ \t]+/) >= 2) w[f[1]] = f[2] + 0
			}
		}
		{ printf "%s %s\n", $0, (($0 in w) ? w[$0] : 0) }' |
		LC_ALL=C sort -k2,2nr -k1,1 |
		awk -v i="$index" -v n="$total" '
			{
				best = 1
				for (j = 2; j <= n; j++) if (load[j] < load[best]) best = j
				load[best] += $2
				if (best == i) print $1
			}')
else
	selected=$(printf '%s\n' "$tests" | awk -v i="$index" -v n="$total" '(NR - 1) % n == i - 1')
fi
names=$(printf '%s\n' "$selected" | grep -v '^$' | paste -sd '|' -)
if [ -z "$names" ]; then
	echo '^$'
else
	echo "^($names)\$"
fi

#!/bin/sh
# ci_disk_check.sh reports the free space a CI job is about to write into and
# fails the job up front when a filesystem is too full to run it.
#
# A runner whose disk filled up otherwise fails jobs at random places (a Go
# build, a docker pull, a test's temp dir), which reads as flaky tests. This
# check names the runner, the path and the free space instead, so the failure
# is classified as the infrastructure problem it is.
#
# The paths are the checkout, the temp dir, $HOME (the shell runners keep
# their caches under it), the bind-mounted Go cache of the docker runners and,
# where the job can see it, the Docker data root. A path that does not exist
# is skipped; one that cannot be inspected is checked at its nearest existing
# parent.
#
# GPUWORKS_CI_DISK_FAIL_GB (default 5) fails the job below that many free
# GiB; GPUWORKS_CI_DISK_WARN_GB (default 20) only warns. GPUWORKS_CI_DISK_PATHS
# replaces the path list (space separated).
set -u

fail_gb=${GPUWORKS_CI_DISK_FAIL_GB:-5}
warn_gb=${GPUWORKS_CI_DISK_WARN_GB:-20}
runner=${CI_RUNNER_DESCRIPTION:-$(hostname 2>/dev/null || echo unknown)}

if [ -n "${GPUWORKS_CI_DISK_PATHS:-}" ]; then
	paths=$GPUWORKS_CI_DISK_PATHS
else
	paths="${CI_PROJECT_DIR:-$(pwd)} ${TMPDIR:-/tmp} ${HOME:-} /host-go-cache"
	if command -v docker >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
		# A wedged daemon must not hang the job before it starts.
		root=$(timeout 10 docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)
		paths="$paths $root"
	fi
fi

status=0
seen=""
for path in $paths; do
	[ -n "$path" ] && [ -e "$path" ] || continue
	probe=$path
	while ! df -Pk "$probe" >/dev/null 2>&1; do
		[ "$probe" = / ] && break
		probe=$(dirname "$probe")
	done
	line=$(df -Pk "$probe" 2>/dev/null | awk 'NR == 2')
	[ -n "$line" ] || continue
	mount=$(echo "$line" | awk '{print $6}')
	case " $seen " in *" $mount "*) continue ;; esac
	seen="$seen $mount"
	free_gb=$(echo "$line" | awk '{printf "%d", $4 / 1048576}')
	used=$(echo "$line" | awk '{print $5}')
	if [ "$free_gb" -lt "$fail_gb" ]; then
		echo "ci-disk-check: runner $runner: $mount (for $path) has ${free_gb} GiB free, $used used; below the ${fail_gb} GiB this job needs. Free space on the runner and retry." >&2
		status=1
	elif [ "$free_gb" -lt "$warn_gb" ]; then
		echo "ci-disk-check: warning: runner $runner: $mount (for $path) has only ${free_gb} GiB free, $used used." >&2
	else
		echo "ci-disk-check: $mount (for $path): ${free_gb} GiB free"
	fi
done
exit "$status"

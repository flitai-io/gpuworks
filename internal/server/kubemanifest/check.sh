#!/usr/bin/env bash
# GPUWorks check for a Kubernetes cluster, run where kubectl reaches it.
#
#   precheck  Before connecting: starts a short-lived DaemonSet with the agent
#             image on every node the agents would run on, and says for each
#             node whether it pulled the image and reached the server. It
#             removes what it created when it ends, also when interrupted.
#   verify    After applying the manifest: says for each node how its agent
#             pod stands. It changes nothing in the cluster.
#
# Each line is "<node> <code> <detail>"; the GPUWorks console explains every
# code and what to do about it. The script exits 0 only when every node is OK.
# GPUWORKS_CHECK_WAIT overrides how many seconds it waits for the pods.
set -u

# @@PARAMS@@

WAIT=${GPUWORKS_CHECK_WAIT:-$WAIT}
SEP=$'\x1f'
fail=0

say() { printf '%-32s %-20s %s\n' "$1" "$2" "${3:-}"; }

# One line per pod that is not going away, fields split by SEP: name, node (the one the DaemonSet
# meant it for while unscheduled), ready, waiting reason, exit code of the
# last run, why it is not scheduled, waiting message.
POD_TEMPLATE='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\x1f"}}{{with .spec.nodeName}}{{.}}{{else}}{{with .spec.affinity}}{{with .nodeAffinity}}{{with .requiredDuringSchedulingIgnoredDuringExecution}}{{range .nodeSelectorTerms}}{{range .matchFields}}{{range .values}}{{.}}{{end}}{{end}}{{end}}{{end}}{{end}}{{end}}{{end}}{{"\x1f"}}{{range .status.containerStatuses}}{{if .ready}}ready{{end}}{{end}}{{"\x1f"}}{{range .status.containerStatuses}}{{with .state}}{{with .waiting}}{{with .reason}}{{.}}{{end}}{{end}}{{end}}{{end}}{{"\x1f"}}{{range .status.containerStatuses}}{{with .state.terminated}}{{.exitCode}}{{else}}{{with .lastState}}{{with .terminated}}{{.exitCode}}{{end}}{{end}}{{end}}{{end}}{{"\x1f"}}{{range .status.conditions}}{{if eq .type "PodScheduled"}}{{if eq .status "False"}}{{with .message}}{{.}}{{end}}{{end}}{{end}}{{end}}{{"\x1f"}}{{range .status.containerStatuses}}{{with .state}}{{with .waiting}}{{with .message}}{{.}}{{end}}{{end}}{{end}}{{end}}{{"\n"}}{{end}}{{end}}'
DESIRED_TEMPLATE='{{with .status}}{{if .observedGeneration}}{{.desiredNumberScheduled}}{{end}}{{end}}'
MESSAGES_TEMPLATE='{{range .items}}{{.message}}{{"\n"}}{{end}}'

k() { kubectl "$@"; }
pods() { k -n "$NS" get pods -l "app.kubernetes.io/name=$DS" --allow-missing-template-keys=true -o go-template="$POD_TEMPLATE" 2>/dev/null; }
desired() { k -n "$NS" get daemonset "$DS" --allow-missing-template-keys=true -o go-template="$DESIRED_TEMPLATE" 2>/dev/null; }
# events prints the messages of the events of kind/name with reason, oldest
# first as the API lists them.
events() {
	k -n "$NS" get events --field-selector "involvedObject.kind=$1,involvedObject.name=$2,reason=$3" \
		--allow-missing-template-keys=true -o go-template="$MESSAGES_TEMPLATE" 2>/dev/null
}

# longest prints the longest line of its input, the last of equal ones.
longest() {
	local line best=
	while IFS= read -r line; do
		[ "${#line}" -ge "${#best}" ] && best=$line
	done
	printf '%s' "$best"
}

pull_failed() {
	case "$1" in ErrImagePull | ImagePullBackOff | InvalidImageName | ErrImageNeverPull) return 0 ;; esac
	return 1
}

# decided says whether a pod's state is final enough to report: running, or
# failed in a way waiting longer does not change.
decided() { # ready reason code sched
	[ -n "$1" ] || pull_failed "$2" || [ -n "$3" ] || [ -n "$4" ]
}

require_kubectl() {
	if ! command -v kubectl >/dev/null 2>&1; then
		say - KUBECTL_MISSING "kubectl is not on PATH"
		exit 2
	fi
	local out
	if ! out=$(k get --raw /version 2>&1 >/dev/null); then
		say - KUBECTL_FAILED "$(printf '%s' "$out" | tail -n 1)"
		exit 2
	fi
}

# wait_pods polls until every pod the DaemonSet wants exists and is decided,
# or until WAIT seconds have passed.
wait_pods() {
	local deadline=$((SECONDS + WAIT)) want have open name node ready reason code sched msg
	while :; do
		want=$(desired)
		[ "$want" = 0 ] && return
		have=0 open=0
		while IFS=$SEP read -r name node ready reason code sched msg; do
			[ -n "$name" ] || continue
			have=$((have + 1))
			decided "$ready" "$reason" "$code" "$sched" || open=$((open + 1))
		done <<<"$(pods)"
		if [ -n "$want" ] && [ "$have" -ge "$want" ] && [ "$open" = 0 ]; then return; fi
		[ "$SECONDS" -ge "$deadline" ] && return
		sleep 5
	done
}

report() {
	local want have=0 ok=0 name node ready reason code sched msg detail
	want=$(desired)
	if [ "$want" = 0 ]; then
		say - NO_NODE_SELECTED "the DaemonSet's node selection matches no node"
		fail=1
		return
	fi
	while IFS=$SEP read -r name node ready reason code sched msg; do
		[ -n "$name" ] || continue
		have=$((have + 1))
		node=${node:-$name}
		if [ -n "$ready" ]; then
			say "$node" OK
			ok=$((ok + 1))
			continue
		fi
		fail=1
		if [ -n "$UNREACHABLE_EXIT" ] && [ "$code" = "$UNREACHABLE_EXIT" ]; then
			say "$node" SERVER_UNREACHABLE "$SERVER"
		elif pull_failed "$reason"; then
			# The waiting message only says it backs off; the kubelet's event
			# says why, and is the longest of them ("Error: ErrImagePull" only
			# repeats the reason).
			detail=$(events Pod "$name" Failed | longest)
			say "$node" IMAGE_PULL_FAILED "${detail:-$reason: $msg}"
		elif [ -n "$sched" ]; then
			say "$node" NOT_SCHEDULED "$sched"
		elif [ -n "$code" ] || [ "$reason" = CrashLoopBackOff ]; then
			say "$node" CONTAINER_FAILED "exit code ${code:-?}; kubectl -n $NS logs $name --previous"
		elif [ -n "$reason" ] && [ "$reason" != ContainerCreating ] && [ "$reason" != PodInitializing ]; then
			say "$node" CONTAINER_FAILED "$reason: $msg"
		else
			say "$node" STILL_STARTING "still pulling the image or starting after ${WAIT}s; run the check again"
		fi
	done <<<"$(pods)"
	if [ -z "$want" ]; then
		fail=1
		say - STILL_STARTING "the DaemonSet controller has not counted the nodes yet; run the check again"
	elif [ "$have" -lt "$want" ]; then
		fail=1
		detail=$(events DaemonSet "$DS" FailedCreate | tail -n 1)
		say - POD_CREATE_FAILED "$((want - have)) of $want pods were not created: ${detail:-see kubectl -n $NS describe daemonset $DS}"
	fi
	echo "$ok of ${want:-?} nodes OK"
}

precheck() {
	local out ns_created=0
	if [ -n "$PULL_SECRET" ] && ! k -n "$NS" get secret "$PULL_SECRET" >/dev/null 2>&1; then
		say - PULL_SECRET_MISSING "Secret $PULL_SECRET is not in namespace $NS"
		exit 1
	fi
	# A namespace an earlier check created may still be going away.
	local i phase
	for i in $(seq 1 30); do
		phase=$(k get namespace "$NS" -o go-template='{{.status.phase}}' 2>/dev/null) || break
		[ "$phase" = Terminating ] || break
		[ "$i" = 1 ] && echo "Waiting for namespace $NS to finish terminating..."
		sleep 2
	done
	if ! k get namespace "$NS" >/dev/null 2>&1; then
		ns_created=1
	fi
	# shellcheck disable=SC2064 # ns_created is fixed by now
	trap "cleanup $ns_created" EXIT
	trap 'exit 130' INT TERM
	if [ "$ns_created" = 1 ] && ! out=$(printf '%s\n' "$NAMESPACE_YAML" | k apply -f - 2>&1); then
		say - APPLY_FAILED "$out"
		exit 1
	fi
	if ! out=$(printf '%s\n' "$DAEMONSET_YAML" | k apply -f - 2>&1); then
		say - APPLY_FAILED "$out"
		exit 1
	fi
	echo "Checking image $IMAGE and server $SERVER on the selected nodes (up to ${WAIT}s)..."
	wait_pods
	report
}

cleanup() {
	k -n "$NS" delete daemonset "$DS" --ignore-not-found --wait=false >/dev/null 2>&1
	if [ "$1" = 1 ]; then
		k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
	fi
}

verify() {
	if ! k -n "$NS" get daemonset "$DS" >/dev/null 2>&1; then
		say - DAEMONSET_MISSING "no DaemonSet $DS in namespace $NS; apply the manifest first"
		exit 1
	fi
	echo "Checking the agents of DaemonSet $DS in namespace $NS (up to ${WAIT}s)..."
	wait_pods
	report
}

require_kubectl
"$MODE"
exit "$fail"

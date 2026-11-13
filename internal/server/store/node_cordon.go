package store

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Who took a node out of scheduling.
//
// An operator's decision outranks the platform's: a node an operator
// cordoned stays cordoned until they say otherwise, and the platform lifts
// only the cordons it set itself. Without recording who set it, a platform
// cordon that cleared itself also lifted an operator's cordon that happened
// to be on top of it.
//
// The platform's own reason is not recorded: it is read from the node's
// current facts (PlatformCordonReason), so what an operator is shown is
// why the node is cordoned now, not why it once was.
type SchedulingSource string

const (
	// SchedulingBySelf is the zero value: nobody holds the node out of
	// scheduling (it is schedulable, or was cordoned before this was
	// recorded).
	SchedulingBySelf SchedulingSource = ""
	// SchedulingByOperator: a person set it. The platform never changes it.
	SchedulingByOperator SchedulingSource = "operator"
	// SchedulingByPlatform: the platform set it for a reason of its own,
	// and lifts it once every such reason is gone.
	SchedulingByPlatform SchedulingSource = "platform"
)

// Platform cordon reasons, as a client renders them in its own language.
const (
	// CordonRuntimeGPUBlocked: the container runtime cannot give a
	// container this node's accelerators, so every deployment placed here
	// would fail at container start while the cards look perfectly fine.
	CordonRuntimeGPUBlocked = "runtime_gpu_blocked"
	// CordonCardNeedsHostReboot: a card reports a fault whose recovery, as
	// its vendor states it, is a host reboot or a power cycle. Nothing the
	// platform can do on the node fixes it, and the other cards sit behind
	// the same driver.
	CordonCardNeedsHostReboot = "card_needs_host_reboot"
	// CordonKubeUnschedulable: Kubernetes stopped scheduling onto the node
	// (kubectl cordon or drain, a node being replaced), so the platform's
	// own replicas do not land on a node about to go. Lifted once
	// Kubernetes lifts it.
	CordonKubeUnschedulable = "kube_unschedulable"
)

// PlatformCordonReason is why the platform would hold this node out of
// scheduling right now, or "" when it would not. cards are the node's cards
// as the report describes them; kubeUnschedulable is a Kubernetes node's own
// cordon.
func PlatformCordonReason(runtimeGPUBlocked string, kubeUnschedulable bool, cards []proto.GPUInfo) string {
	if kubeUnschedulable {
		return CordonKubeUnschedulable
	}
	if strings.TrimSpace(runtimeGPUBlocked) != "" {
		return CordonRuntimeGPUBlocked
	}
	for _, card := range cards {
		if !accelerator.CardUsable(card.Ownership) {
			continue
		}
		switch card.HealthCode {
		case accelerator.HealthRebootRequired, accelerator.HealthPowerCycleRequired:
			return CordonCardNeedsHostReboot
		}
	}
	return ""
}

// cordonDecision is what a status report does to a node's scheduling.
type cordonDecision struct {
	scheduling NodeScheduling
	source     SchedulingSource
	reason     string // the platform's reason, for the log
	changed    bool
}

// decideCordon applies the platform's half of NodeScheduling to a report.
//
// It cordons a schedulable node while it has a reason, and lifts its own
// cordon once it has none. It never touches a node an operator cordoned or
// drained, and never re-cordons within the same report: an operator who
// turns scheduling back on gets until the next report (about a minute) to
// try a deployment by hand, which is the point of letting them.
func decideCordon(was NodeScheduling, by SchedulingSource, reason string) cordonDecision {
	switch {
	case reason != "" && was == NodeSchedulable:
		return cordonDecision{scheduling: NodeCordoned, source: SchedulingByPlatform, reason: reason, changed: true}
	case reason == "" && was == NodeCordoned && by == SchedulingByPlatform:
		return cordonDecision{scheduling: NodeSchedulable, source: SchedulingBySelf, changed: true}
	}
	return cordonDecision{scheduling: was, source: by}
}

// CordonReason is the platform's reason for holding the node out of
// scheduling, read from the node's current facts and loaded cards; "" when
// the platform does not hold it (an operator did, or nobody does).
func (n *Node) CordonReason() string {
	if n.SchedulingBy != SchedulingByPlatform || n.Scheduling == NodeSchedulable {
		return ""
	}
	cards := make([]proto.GPUInfo, 0, len(n.GPUs))
	for _, g := range n.GPUs {
		cards = append(cards, proto.GPUInfo{Ownership: g.Ownership, HealthCode: g.HealthCode})
	}
	return PlatformCordonReason(n.RuntimeGPUBlocked, n.KubeUnschedulable, cards)
}

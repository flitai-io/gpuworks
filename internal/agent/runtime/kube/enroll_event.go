package kube

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// EnrollRefusedReason is the reason of the Warning event an agent writes on
// its own pod when the server refuses its enrollment, so
// `kubectl describe pod` shows why the node does not appear.
const EnrollRefusedReason = "GpuworksEnrollRefused"

// EnrollRefusalEventInterval is how long a refusal that persists goes
// unreported again: the retry loop asks every minute, an event a minute
// would be noise, but one that scrolled out of `kubectl describe` (events
// expire after an hour) should come back.
const EnrollRefusalEventInterval = 10 * time.Minute

const (
	eventComponent    = "gpuworks-agent"
	eventWriteTimeout = 10 * time.Second
)

// RefusalReporter writes an enrollment refusal as a Warning Event on the
// agent's own pod. It reports a refusal once, again when the refusal changes
// (another reason, or another entry holding the node) and again once
// Interval has passed with the same refusal. A failed write (no events rule
// in the Role of a manifest from before this existed, or the API server
// being away) is only logged: the event is a hint, never part of enrolling.
type RefusalReporter struct {
	API       *kubeapi.Client
	Namespace string
	PodName   string
	NodeName  string
	Log       *slog.Logger
	// Interval defaults to EnrollRefusalEventInterval, Now to time.Now.
	Interval time.Duration
	Now      func() time.Time

	lastKey string
	lastAt  time.Time
	podUID  string
}

func (r *RefusalReporter) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Report writes the event for refusal unless the same refusal was reported
// less than Interval ago. The attempt counts as the report even when the
// write fails, so a forbidden or unreachable API is tried once per Interval,
// not once per enrollment retry.
func (r *RefusalReporter) Report(ctx context.Context, refusal *EnrollRefusal) {
	if refusal == nil || r.API == nil {
		return
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	if r.Namespace == "" || r.PodName == "" {
		return // not an agent pod of a DaemonSet manifest: nothing to hang an event on
	}
	interval := r.Interval
	if interval <= 0 {
		interval = EnrollRefusalEventInterval
	}
	key := refusal.Reason + "\x00" + refusal.Entry
	now := r.now()
	if key == r.lastKey && now.Sub(r.lastAt) < interval {
		return
	}
	r.lastKey, r.lastAt = key, now

	ctx, cancel := context.WithTimeout(ctx, eventWriteTimeout)
	defer cancel()
	if r.podUID == "" {
		var pod kubeapi.Pod
		if err := r.API.Get(ctx, kubeapi.PodPath(r.Namespace, r.PodName), &pod); err != nil {
			// The event still names the pod; only its uid, which makes
			// kubectl match it to this very pod and not an older one of the
			// same name, is missing.
			log.Warn("could not read the agent's own pod for the enrollment-refusal event; writing it without a uid", "pod", r.PodName, "err", err)
		} else {
			r.podUID = pod.Metadata.UID
		}
	}
	stamp := now.Format(time.RFC3339)
	ev := kubeapi.Event{
		Metadata: kubeapi.ObjectMeta{Name: fmt.Sprintf("%s.%x", r.PodName, now.UnixNano()), Namespace: r.Namespace},
		Involved: kubeapi.ObjectReference{
			APIVersion: "v1", Kind: "Pod", Namespace: r.Namespace, Name: r.PodName, UID: r.podUID,
		},
		Reason: EnrollRefusedReason, Type: "Warning", Message: RefusalEventMessage(refusal),
		FirstTimestamp: stamp, LastTimestamp: stamp, Count: 1,
		Source:             kubeapi.EventSource{Component: eventComponent, Host: r.NodeName},
		ReportingComponent: eventComponent, ReportingInstance: r.PodName,
	}
	if _, err := r.API.CreateEvent(ctx, ev); err != nil {
		log.Warn("could not write the enrollment-refusal event on the agent's pod (the manifest's Role needs events create; re-apply the manifest)", "pod", r.PodName, "err", err)
		return
	}
	log.Info("wrote the enrollment-refusal event on the agent's pod", "pod", r.PodName, "reason", refusal.Reason)
}

// RefusalEventMessage is the text of the event for a refusal: why, and what
// to do. English only: a Kubernetes event has no locale.
func RefusalEventMessage(refusal *EnrollRefusal) string {
	switch refusal.Reason {
	case "node_taken":
		holder := "another entry of this cluster"
		if refusal.Entry != "" {
			holder = fmt.Sprintf("the entry %q", refusal.Entry)
		}
		return fmt.Sprintf("GPUWorks refused to enroll this node: it is already held by %s, whose agent on it is online or was seen in the last 15 minutes. "+
			"Remove this node from one of the two entries' node selectors, or wait until that agent has been offline for 15 minutes; "+
			"this agent keeps retrying and enrolls as soon as the node is free.", holder)
	case "other_cluster":
		return "GPUWorks refused to enroll this node: the connect token belongs to another Kubernetes cluster (a token binds to the first cluster that uses it). " +
			"Apply the manifest of a cluster entry created for this cluster in the GPUWorks console instead."
	case "", "unknown_token":
		return "GPUWorks refused to enroll this node: the connect token is unknown or has been revoked. " +
			"Create a new cluster entry for this cluster in the GPUWorks console (or use one whose token is not revoked) and apply its manifest here."
	default:
		msg := "GPUWorks refused to enroll this node (reason " + refusal.Reason + ")"
		if refusal.Message != "" {
			msg += ": " + refusal.Message
		}
		return msg
	}
}

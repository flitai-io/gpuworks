package kube

import (
	"context"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// The parts of borrowing that HAMi and ACK GPU sharing have in common: hold
// pods go through a scheduler instead of being bound to the node, so each
// either runs, or is left Pending with the scheduler's reason.

const defaultScheduleWait = 60 * time.Second

// baseHoldPod is a hold pod before it asks for any card: what every mode
// shares. The caller sets how it reaches the node and what it requests.
func (p *cardPool) baseHoldPod(name string) kubeapi.Pod {
	grace := int64(0)
	pod := kubeapi.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: kubeapi.ObjectMeta{Name: name, Namespace: p.ns,
			Labels: map[string]string{LabelManagedBy: "gpuworks", LabelRole: RoleHold, LabelNode: p.node}},
		Spec: kubeapi.PodSpec{
			RestartPolicy: "Always", TerminationGracePeriodSeconds: &grace, ImagePullSecrets: p.pullSecrets,
			PriorityClassName: p.priorityClass, Tolerations: []kubeapi.Toleration{{Operator: "Exists"}},
			Containers: []kubeapi.Container{{
				Name: "hold", Image: p.holdImage, ImagePullPolicy: "IfNotPresent", Command: p.holdCmd,
				Resources: &kubeapi.Resources{Requests: map[string]string{"cpu": "10m", "memory": "16Mi"}},
			}},
		},
	}
	if p.owner != nil {
		pod.Metadata.OwnerReferences = []kubeapi.OwnerReference{*p.owner}
	}
	return pod
}

// isHoldPod says pod is one of the platform's hold pods.
func (p *cardPool) isHoldPod(pod kubeapi.Pod) bool {
	return pod.Metadata.Namespace == p.ns && strings.HasPrefix(pod.Metadata.Name, holdPrefix)
}

// schedOutcome is what became of a hold pod sent through a scheduler.
type schedOutcome struct {
	running bool
	// why is the scheduler's or kubelet's reason when it is not running.
	why string
	// idle: nothing scheduled or refused the pod in time, which is what a
	// scheduler name nobody serves looks like.
	idle bool
}

// waitScheduled waits until each hold pod runs, is refused by the kubelet, or
// is found unschedulable: a pod left Pending with PodScheduled False and
// reason Unschedulable will not run unless something else changes, and its
// message is the scheduler's own account (for HAMi, for example,
// CardInsufficientMemory when another pod holds part of the card).
func (p *cardPool) waitScheduled(ctx context.Context, names []string) map[string]schedOutcome {
	out := map[string]schedOutcome{}
	pending := map[string]bool{}
	for _, n := range names {
		pending[n] = true
	}
	limit := p.scheduleWait
	if limit <= 0 {
		limit = defaultScheduleWait
	}
	deadline := time.Now().Add(limit)
	for len(pending) > 0 && ctx.Err() == nil {
		for n := range pending {
			var pod kubeapi.Pod
			err := p.api.Get(ctx, kubeapi.PodPath(p.ns, n), &pod)
			switch {
			case err != nil && kubeapi.IsNotFound(err):
				out[n] = schedOutcome{why: "the hold pod was deleted"}
				delete(pending, n)
			case err != nil:
			case pod.Status.Phase == "Running":
				out[n] = schedOutcome{running: true}
				delete(pending, n)
			case pod.Status.Phase == "Failed":
				out[n] = schedOutcome{why: "the kubelet refused the hold pod: " + strings.TrimSpace(pod.Status.Reason+": "+pod.Status.Message)}
				delete(pending, n)
			default:
				if c, ok := pod.Status.Condition("PodScheduled"); ok && c.Status == "False" && c.Reason == "Unschedulable" {
					out[n] = schedOutcome{why: "the scheduler could not place the hold pod: " + c.Message}
					delete(pending, n)
				}
			}
		}
		if len(pending) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			for n := range pending {
				out[n] = schedOutcome{why: "the hold pod was neither scheduled nor refused within " + limit.String(), idle: true}
			}
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(p.poll):
		}
	}
	for n := range pending {
		if _, ok := out[n]; !ok {
			out[n] = schedOutcome{why: "waiting for the hold pod was interrupted"}
		}
	}
	return out
}

// waitGone waits, for a bounded time, until the API server no longer has the
// deleted hold pods: a scheduler counts a pod's share of a card until the
// pod is gone, and a borrow that began before then would not find the card
// free.
func (p *cardPool) waitGone(ctx context.Context, pods []string) {
	limit := p.releaseWait
	if limit <= 0 {
		limit = defaultReleaseWait
	}
	deadline := time.Now().Add(limit)
	for _, n := range pods {
		for {
			var pod kubeapi.Pod
			err := p.api.Get(ctx, kubeapi.PodPath(p.ns, n), &pod)
			if kubeapi.IsNotFound(err) {
				break
			}
			if !time.Now().Before(deadline) || ctx.Err() != nil {
				p.log.Warn("the returned hold pod is still there", "pod", n, "err", err)
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(p.poll):
			}
		}
	}
}

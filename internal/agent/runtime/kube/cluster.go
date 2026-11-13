package kube

import (
	"context"
	"net/url"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// What the agent answers about its node and cluster, rather than about an
// instance: cordon state, whether a node of the cluster still exists and
// whether one of its entry's agents runs there.

// Unschedulable reports whether this node is cordoned in Kubernetes.
func (d *Driver) Unschedulable(ctx context.Context) (bool, error) {
	var n kubeapi.Node
	if err := d.o.API.Get(ctx, kubeapi.NodePath(d.o.NodeName)+"?resourceVersion=0", &n); err != nil {
		return false, err
	}
	return n.Spec.Unschedulable, nil
}

// NodeExists reports whether a node of this cluster still exists.
func (d *Driver) NodeExists(ctx context.Context, name string) (bool, error) {
	var n kubeapi.Node
	err := d.o.API.Get(ctx, kubeapi.NodePath(name), &n)
	if kubeapi.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// AgentOn reports whether an agent pod of this agent's DaemonSet is on the
// named node, in any phase and bound or not yet: a node the DaemonSet no
// longer selects has none, while one whose agent is crashing, pulling or
// waiting for the scheduler still has its pod.
func (d *Driver) AgentOn(ctx context.Context, name string) (bool, error) {
	var list kubeapi.PodList
	q := "?labelSelector=" + url.QueryEscape(agentPodLabel+"="+DaemonSetName)
	if err := d.o.API.Get(ctx, kubeapi.PodsPath(d.o.Namespace)+q, &list); err != nil {
		return false, err
	}
	for _, p := range list.Items {
		if p.TargetNode() == name {
			return true, nil
		}
	}
	return false, nil
}

// agentPodLabel is the label the manifest gives the agent pods.
const agentPodLabel = "app.kubernetes.io/name"

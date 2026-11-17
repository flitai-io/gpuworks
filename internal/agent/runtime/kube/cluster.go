package kube

import (
	"context"
	"net/url"
	"slices"

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

// NodesSelected reports, for each named node, whether its labels match a node
// selection: every selector pair, and when poolLabel is set its value of
// that label is one of poolIDs. It reads each node, as the manifest's
// nodeSelector and nodeAffinity are matched against the node's labels; a node
// that no longer exists is not selected, and an API failure on any other read
// is an error rather than a guess.
func (d *Driver) NodesSelected(ctx context.Context, names []string, selector map[string]string, poolLabel string, poolIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		var n kubeapi.Node
		err := d.o.API.Get(ctx, kubeapi.NodePath(name), &n)
		switch {
		case kubeapi.IsNotFound(err):
			out[name] = false
			continue
		case err != nil:
			return nil, err
		}
		out[name] = nodeMatches(n.Metadata.Labels, selector, poolLabel, poolIDs)
	}
	return out, nil
}

func nodeMatches(labels, selector map[string]string, poolLabel string, poolIDs []string) bool {
	for k, v := range selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	if poolLabel == "" {
		return true
	}
	got, ok := labels[poolLabel]
	return ok && slices.Contains(poolIDs, got)
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

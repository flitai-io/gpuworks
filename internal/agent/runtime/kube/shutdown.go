package kube

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// What happens when this agent leaves a node: an ordinary restart, rolling
// update or in-place upgrade leaves everything as it is (another agent of
// the same DaemonSet will run here again), but once the node's entry
// narrows its node selection and the manifest is reapplied, the DaemonSet
// removes the agent pod from every node it no longer selects. Nothing is
// left there to return the cards this agent borrowed, so it must do that
// itself before it goes.

// ReleaseCardsIfNodeUnselected runs as the agent leaves this node (SIGTERM,
// or its connection loop returning for any other reason): once this
// agent's own DaemonSet no longer selects this node, the agent returns
// every card it holds here — deleting this node's hold pods — because
// nothing else is left to do it. When the DaemonSet still selects the
// node, or the check itself fails, nothing is touched: an ordinary
// restart, rolling update or in-place upgrade must not lose its cards, and
// neither must a node this agent cannot currently confirm has dropped out
// of selection. ctx should carry a short deadline (the pod's own
// terminationGracePeriodSeconds); an API error is logged, never fatal to
// shutdown.
func (d *Driver) ReleaseCardsIfNodeUnselected(ctx context.Context) {
	selected, err := d.nodeStillSelected(ctx)
	if err != nil {
		d.o.Log.Warn("cannot tell whether the agent DaemonSet still selects this Kubernetes node; leaving its hold pods as they are", "err", err)
		return
	}
	if selected {
		return
	}
	d.o.Log.Info("this node is no longer selected by the agent DaemonSet; returning its borrowed cards before the agent leaves")
	if err := d.cards.releaseAllHeld(ctx); err != nil {
		d.o.Log.Warn("could not list this node's hold pods while leaving it; some may stay behind", "err", err)
	}
}

// nodeStillSelected reads this agent's own DaemonSet and node, and reports
// whether the DaemonSet's pod template — the nodeSelector and the pool
// nodeAffinity term kubemanifest.Render writes — still matches this
// node's labels. It is the same match NodesSelected answers for a remote
// node on the server's behalf (cluster.go's nodeMatches, asked over the
// proto.RPCKubeNodesSelected RPC), read here against this node directly.
func (d *Driver) nodeStillSelected(ctx context.Context) (bool, error) {
	var ds kubeapi.DaemonSet
	if err := d.o.API.Get(ctx, kubeapi.DaemonSetPath(d.o.Namespace, DaemonSetName), &ds); err != nil {
		return false, fmt.Errorf("read the agent DaemonSet: %w", err)
	}
	var node kubeapi.Node
	if err := d.o.API.Get(ctx, kubeapi.NodePath(d.o.NodeName), &node); err != nil {
		return false, fmt.Errorf("read this node: %w", err)
	}
	poolLabel, poolIDs := daemonSetPoolTerm(ds.Spec.Template.Spec.Affinity)
	return nodeMatches(node.Metadata.Labels, ds.Spec.Template.Spec.NodeSelector, poolLabel, poolIDs), nil
}

// daemonSetPoolTerm reads the pool nodeAffinity term kubemanifest.Render
// writes: one requiredDuringSchedulingIgnoredDuringExecution term with one
// matchExpressions entry, operator In, naming PoolLabel and PoolIDs. Any
// other shape — no affinity at all, or one hand-edited into something
// Render never writes — reads as "no pool restriction" rather than
// guessed at; nodeMatches then relies on NodeSelector alone.
func daemonSetPoolTerm(a *kubeapi.Affinity) (label string, ids []string) {
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.Required == nil {
		return "", nil
	}
	for _, term := range a.NodeAffinity.Required.Terms {
		for _, expr := range term.MatchExpressions {
			if expr.Operator == "In" && expr.Key != "" {
				return expr.Key, expr.Values
			}
		}
	}
	return "", nil
}

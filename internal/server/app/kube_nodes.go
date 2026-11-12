package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Kubernetes nodes come and go with their cluster: a node scaled away or
// replaced, or one its entry no longer selects (a changed node selector or
// set of node pools), leaves its row offline here for good. kubeNodeLoop
// asks another online agent of the same entry whether such a node still
// exists there and runs one of the entry's agents, and removes the ones that
// do not and run nothing. The server holds no cluster credentials, so the
// question goes through an agent.

const (
	kubeNodeSweepInterval = 5 * time.Minute
	// kubeNodeGoneAfter is how long a node is offline before it is asked
	// about: long enough for an agent pod restart or a node reboot.
	kubeNodeGoneAfter = 15 * time.Minute
)

func (a *App) kubeNodeLoop(ctx context.Context) {
	t := time.NewTicker(kubeNodeSweepInterval)
	defer t.Stop()
	strikes := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepGoneKubeNodes(ctx, a.log, a.store, a.hub.CallNode, a.hub.Kick, time.Now().Add(-kubeNodeGoneAfter), strikes)
		}
	}
}

// nodeCaller is the hub's RPC to a node's agent.
type nodeCaller func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)

// sweepGoneKubeNodes removes a node only when two sweeps in a row find it
// gone: one answer can catch an agent pod between the deletion and the
// re-creation of a rolling update. strikes holds the nodes the last sweep
// found gone.
func sweepGoneKubeNodes(ctx context.Context, log *slog.Logger, st *store.Store, call nodeCaller, kick func(string), cutoff time.Time, strikes map[string]bool) {
	nodes, err := st.OfflineKubeNodes(cutoff)
	if err != nil {
		log.Warn("kubernetes node sweep failed", "err", err)
		return
	}
	found := map[string]bool{}
	defer func() {
		for uuid := range strikes {
			if !found[uuid] {
				delete(strikes, uuid)
			}
		}
	}()
	for _, node := range nodes {
		if instances, err := st.InstancesForNode(node.ID); err != nil || len(instances) > 0 {
			continue // its instances are the controller's to settle first
		}
		peers, err := st.OnlineKubePeers(node.KubeClusterID, node.ID)
		if err != nil || len(peers) == 0 {
			continue
		}
		var answer proto.KubeNodeExistsResult
		asked := false
		for _, peer := range peers {
			callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			raw, err := call(callCtx, peer.UUID, proto.RPCKubeNodeExists, proto.KubeNodeExistsParams{NodeName: node.KubeNodeName})
			cancel()
			if err == nil && json.Unmarshal(raw, &answer) == nil {
				asked = true
				break
			}
		}
		left := !answer.Exists || (answer.Agent != nil && !*answer.Agent)
		if !asked || !left {
			continue
		}
		found[node.UUID] = true
		if !strikes[node.UUID] {
			strikes[node.UUID] = true
			continue
		}
		delete(strikes, node.UUID)
		if err := st.DeleteNode(node.UUID); err != nil {
			log.Warn("removing a node that left its Kubernetes cluster failed", "node", node.UUID, "err", err)
			continue
		}
		kick(node.UUID)
		log.Info("removed a node that left its Kubernetes cluster", "node", node.UUID, "kube_node", node.KubeNodeName)
	}
}

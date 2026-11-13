package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubeSelectionStopMessage is what an instance asked to stop because its node
// leaves an entry's selection says on its timeline.
const kubeSelectionStopMessage = "its node leaves the Kubernetes cluster entry's node selection"

// kubeStopLeaving asks the instances on the nodes a confirmed selection leaves
// out to stop, through the platform's own removal path (store.StopInstancesOnNodes:
// the instance is moved to removing, which the controller and the node's agent
// act on), and returns them. It does not wait for the agents, and it touches
// only the instances on those nodes: a deployment that also runs elsewhere
// keeps those replicas.
//
// Nodes the server knows leave (verified) and that still take replicas are
// cordoned first, as the administrator would have: without it the controller
// could rebuild a replica it just lost on the same node, the freed cards being
// the best fit, and the instance would run on a node whose agent is about to go.
// A node already cordoned or draining is left as it is. When the question to
// the agents was not answered the nodes may still be selected, so they are
// not cordoned and only their instances are stopped, as the confirmation said.
func (a *apiHandlers) kubeStopLeaving(entry store.KubeCluster, drops []kubeDrop, verified bool) ([]dto.KubeStoppedInstance, error) {
	ids := make([]uint, 0, len(drops))
	for _, d := range drops {
		ids = append(ids, d.nodeID)
		if verified && d.schedulable {
			if _, err := a.d.Store.SetNodeScheduling(d.NodeUUID, store.NodeCordoned); err != nil {
				return nil, err
			}
		}
	}
	stopped, err := a.d.Store.StopInstancesOnNodes(ids, kubeSelectionStopMessage)
	if err != nil {
		return nil, err
	}
	out := make([]dto.KubeStoppedInstance, 0, len(stopped))
	for _, s := range stopped {
		out = append(out, dto.KubeStoppedInstance{InstanceUUID: s.InstanceUUID, Deployment: s.Deployment, NodeUUID: s.NodeUUID})
	}
	if a.d.Log != nil {
		a.d.Log.Info("instances asked to stop: their nodes leave a Kubernetes entry's selection", "cluster", entry.Name, "nodes", len(drops), "instances", len(out), "verified", verified)
	}
	return out, nil
}

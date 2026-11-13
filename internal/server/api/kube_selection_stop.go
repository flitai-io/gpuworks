package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubeSelectionStopMessage is what an instance asked to stop because its node
// leaves an entry's selection says on its timeline.
const kubeSelectionStopMessage = "its node leaves the Kubernetes cluster entry's node selection"

// kubeLeaveOf is what a confirmed selection change does to the nodes it leaves
// out: their instances are asked to stop through the platform's own removal
// path (store.StopInstancesOnNodes: the instance is moved to removing, which
// the controller and the node's agent act on), without waiting for the agents
// and touching only the instances on those nodes, so a deployment that also
// runs elsewhere keeps those replicas.
//
// Nodes the server knows leave (verified) and that still take replicas are
// cordoned as well, as the administrator would have: without it the controller
// could rebuild a replica it just lost on the same node, the freed cards being
// the best fit, and the instance would run on a node whose agent is about to go.
// A node already cordoned or draining is left as it is. When the question to
// the agents was not answered the nodes may still be selected, so they are
// not cordoned and only their instances are stopped, as the confirmation said.
//
// It writes nothing. The store methods that save the change (UpdateKubeClusterLeaving,
// SetKubeClusterPoolsLeaving) write it in the same transaction, after every
// validation the request has, so a change that fails stops nothing.
func kubeLeaveOf(drops []kubeDrop, verified bool) store.KubeLeave {
	leave := store.KubeLeave{Message: kubeSelectionStopMessage}
	for _, d := range drops {
		leave.StopNodeIDs = append(leave.StopNodeIDs, d.nodeID)
		if verified && d.schedulable {
			leave.CordonNodeUUIDs = append(leave.CordonNodeUUIDs, d.NodeUUID)
		}
	}
	return leave
}

// kubeStoppedDTO is the answer's list of the instances the save asked to stop;
// always a list, empty when none.
func kubeStoppedDTO(stopped []store.StoppedInstance) []dto.KubeStoppedInstance {
	out := make([]dto.KubeStoppedInstance, 0, len(stopped))
	for _, s := range stopped {
		out = append(out, dto.KubeStoppedInstance{InstanceUUID: s.InstanceUUID, Deployment: s.Deployment, NodeUUID: s.NodeUUID})
	}
	return out
}

// logKubeStopped notes the stops of a saved change.
func (a *apiHandlers) logKubeStopped(entry string, stopped []dto.KubeStoppedInstance, leave store.KubeLeave) {
	if a.d.Log != nil && len(stopped) > 0 {
		a.d.Log.Info("instances asked to stop: their nodes leave a Kubernetes entry's selection", "cluster", entry, "nodes", len(leave.StopNodeIDs), "instances", len(stopped), "cordoned", len(leave.CordonNodeUUIDs))
	}
}

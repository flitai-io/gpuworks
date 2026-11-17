package controller

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// cacheReferencesChanged retries cleanup only where a changed deployment's
// readers lived; an instance's readers are noted by onEvent, which has already
// looked the instance up. Delete notifications contain only an ID and arrive
// after the row is gone.
// Their durable waiting operations are retried by the next node status report
// (or reconnect/startup), rather than waking every cache in the fleet.
func (c *Controller) cacheReferencesChanged(ev bus.Event) {
	if ev.Topic != "deployment" {
		return
	}
	id, err := strconv.ParseUint(ev.Key, 10, 64)
	if err != nil {
		return
	}
	dep, err := c.store.GetDeployment(uint(id))
	if err != nil {
		return
	}
	// Include both configured placement and actual instances: a pool or a
	// placement edit can leave readers on nodes absent from the new plan.
	nodeIDs, err := dep.StoredNodeIDs()
	if err != nil {
		return
	}
	nodeIDs = append(nodeIDs, dep.NodeID)
	insts, err := c.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return
	}
	for _, inst := range insts {
		nodeIDs = append(nodeIDs, inst.NodeID)
	}
	// The waiting operations are looked up once per tick for every node
	// noted since the last one (flushEventRoutes).
	c.routes.noteReaders(nodeIDs...)
}

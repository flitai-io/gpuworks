package controller

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeStatusSettle is how often routine node status reports are acted on.
// Every agent reports once a minute, so a thousand nodes report about
// seventeen times a second; reconciling every deployment on each of them
// kept the controller re-planning the whole cluster without pause. Reports
// arriving within this window are acted on once, for the deployments that
// could gain from them.
const nodeStatusSettle = 2 * time.Second

// nodeStatusReported notes a routine status report. The next settle tick
// enqueues the deployments a changed node could unblock.
func (c *Controller) nodeStatusReported() {
	c.nodeStatusDirty.Store(true)
}

// settleNodeStatus enqueues, once per batch of status reports, every
// deployment that is not settled (deploymentSettled). A settled deployment
// has nothing a node's free cards, disk, images or health could change; one
// whose last pass found no node waits out poolSettleBackoff first.
func (c *Controller) settleNodeStatus() {
	if !c.nodeStatusDirty.Swap(false) {
		return
	}
	facts, err := c.store.DeploymentSettleFacts()
	if err != nil {
		// Unknown is unsettled: fall back to reconciling everything.
		c.log.Warn("could not read deployment settle facts", "err", err)
		c.enqueueAllDeployments()
		return
	}
	for _, fact := range facts {
		if !deploymentSettled(fact) && !c.pool.waitingRecently(fact.ID, c.now()) {
			c.q.add(depKey(fact.ID))
		}
	}
}

// deploymentSettled reports whether a deployment is known to need nothing: an
// active single-node deployment with exactly its replicas running and none on
// the way anywhere, or a stopped one with nothing left running. Anything else
// - a replica waiting for a node, one starting, one on a draining node, a
// multi-node or PD deployment, a deletion - is unsettled, so a doubt costs a reconcile, never
// a missed one.
func deploymentSettled(fact store.DeploymentSettleFact) bool {
	if fact.Moving > 0 || fact.OnDrainingNode > 0 {
		return false
	}
	switch fact.State {
	case store.DeployStopped:
		return fact.Running == 0
	case store.DeployActive:
		if fact.NNodes > 1 {
			return false
		}
		if pd, err := planning.PDOf(fact.Placement); err != nil || pd != nil {
			return false
		}
		return fact.Running == max(fact.Replicas, 1)
	}
	return false
}

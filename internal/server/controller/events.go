// Bus-event routing: which change notifications make which deployments need
// another reconcile pass. It is split out of controller.go because it is the
// one place that decides what counts as a change worth re-planning for, and
// that decision is what keeps a fleet's progress traffic from turning into a
// cluster-wide re-plan.
package controller

import (
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// onEvent maps a change event to the deployment(s) that need reconciling.
//
// A progress event is deliberately inert here. Reconciling is driven by state:
// a transfer that is merely further along cannot place, start, or unblock
// anything, and treating each of its samples as a change is what turned one
// downloading node into a full re-plan of every deployment in the cluster.
func (c *Controller) onEvent(ev bus.Event) {
	c.cacheEvent(ev)
	switch ev.Topic {
	case store.InferenceTemplateTopic:
		c.observation.request(ev.Key)
	case "network_test":
		c.onNetworkTestEvent(ev.Key, ev.Reason)
	case "deployment":
		if id, err := strconv.ParseUint(ev.Key, 10, 64); err == nil {
			if ev.Reason == store.DeploymentChangeRequirements || ev.Reason == store.DeploymentChangeDGPUQuota {
				// A node refused under the old pool or the old slice size may
				// be exactly what the new one admits; try it again at once.
				c.forgetPool(uint(id))
			}
			if ev.Reason == store.DeploymentChangeDGPUQuota {
				c.markDGPULimitPush(uint(id))
			}
			c.q.add(depKey(uint(id)))
		}
	case "instance":
		if ev.Reason == bus.ReasonProgress {
			return
		}
		// Instance status changed → reconcile its deployment, and retry the
		// cache operations that were waiting for its node's readers. A
		// deleted instance is retried by its node's next report instead
		// (cacheReferencesChanged).
		if depID, nodeID, err := c.store.InstanceRefs(ev.Key); err == nil {
			c.q.add(depKey(depID))
			c.routes.noteReaders(nodeID)
		}
	case "node":
		switch ev.Reason {
		case bus.ReasonProgress:
		case store.NodeChangeStatus:
			c.nodeStatusReported() // acted on in batches (settleNodeStatus)
		default:
			// A node registering, reconnecting, relabelled or drained may
			// unblock a waiting deployment, and concerns the replicas on it.
			c.routes.noteNode(ev.Key) // acted on in batches (flushEventRoutes)
		}
	case "modelfile":
		if ev.Reason == bus.ReasonProgress {
			return
		}
		c.releaseTerminalModelSync(ev.Key)
		// A model becoming present may unblock a deployment; one that is
		// already running may still care that its files changed. The key is
		// resolved at the tick, once (flushEventRoutes).
		c.routes.noteModelFile(ev.Key)
	case "storage":
		switch ev.Reason {
		case bus.ReasonProgress:
		case "node_state":
			// A storage turning ready on a node is what a waiting deployment
			// may need; nodes report it on every reconnect.
			c.routes.noteUnsettled()
		default:
			// An operator's storage edit is rare; it gets every deployment.
			c.routes.noteAll()
		}
	case store.PullSettingsTopic:
		// A rule change may allow a download or a pull a deployment was
		// refused for, or forbid one; an operator's edit is rare, so every
		// deployment gets a pass rather than an index of who was refused.
		c.routes.noteAll()
	case "image":
		// Image-node progress/presence and peer-sync starts do not change catalog refs.
		// Pushing the same full list back to every agent here creates a feedback
		// loop: each agent refreshes node status, which publishes another image
		// node event. Catalog mutations still propagate the new ref set.
		if ev.Reason != "node" && ev.Reason != "syncing" && ev.Reason != bus.ReasonProgress {
			c.requestImageRefsPush()
		}
		if ev.Reason == bus.ReasonProgress {
			return
		}
		// A copy finishing or failing unblocks the deployments waiting for
		// it; only those are re-queued.
		if ev.Reason == "node" {
			if id, err := strconv.ParseUint(strings.TrimSpace(ev.Key), 10, 64); err == nil {
				for _, dep := range c.imageWaits.waiting(uint(id)) {
					c.q.add(depKey(dep))
				}
			}
		}
	}
}

func (c *Controller) releaseTerminalModelSync(key string) {
	// Keep the database observation and map ownership in the same critical
	// section: a delayed terminal observation must not remove a replacement.
	c.syncMu.Lock()
	run := c.modelSyncs[key]
	if run == nil {
		c.syncMu.Unlock()
		return
	}
	mf, err := c.store.ResolveModelFileKey(key)
	if err != nil || mf.State == proto.MFSyncing || mf.State == proto.MFPending || mf.State == proto.MFDownloading || mf.TransferID != run.transferID {
		// Admission registers its run before BeginModelSync changes pending to
		// syncing. Neither a preparation event nor another attempt's terminal
		// state is a reason to interrupt that new run.
		c.syncMu.Unlock()
		return
	}
	delete(c.modelSyncs, key)
	c.syncMu.Unlock()
	// A target can finish before the source-side RPC response reaches the
	// coordinator. Let that successful response finish; failed or withdrawn
	// terminal states must still interrupt their remaining transfer work.
	if !proto.ModelFileStateReady(mf.State) {
		run.cancel()
	}
}

package controller

// deploymentMemory is something the controller keeps in memory about each
// deployment and drops once the deployment stops (gone false) or is gone
// (gone true). Dropping it is memory only: no I/O, nothing that can fail, and
// dropping twice is harmless. Cleanup that talks to nodes (withdrawing
// downloads and cache copies) is not memory: it can fail, and the reconcile
// that calls it retries it.
type deploymentMemory interface{ forget(depID uint, gone bool) }

// forgetFunc is memory dropped once the deployment stops.
type forgetFunc func(depID uint)

func (f forgetFunc) forget(depID uint, _ bool) { f(depID) }

// forgetGoneFunc is memory a stopped deployment keeps, because it still holds
// when the deployment starts again, and drops once the deployment is gone.
type forgetGoneFunc func(depID uint)

func (f forgetGoneFunc) forget(depID uint, gone bool) {
	if gone {
		f(depID)
	}
}

// deploymentMemories is every deploymentMemory the controller keeps. State
// kept by deployment is added here; TestEveryControllerFieldIsClassified
// fails until a new Controller field is either listed here or said not to be
// kept by deployment.
func (c *Controller) deploymentMemories() []deploymentMemory {
	return []deploymentMemory{
		forgetFunc(c.forgetPool),
		forgetFunc(c.placements.forget),
		forgetFunc(c.sleepPushes.forget),
		forgetFunc(c.drainClocks.forget),
		forgetFunc(c.dgpuPushes.forget),
		forgetFunc(c.imageWaits.forget),
		forgetFunc(c.failover.forgetStopped),
		forgetGoneFunc(c.failover.forgetMoves),
		// An operator may name the nodes of a stopped deployment's next
		// replicas (the replicas API takes place_on while it is stopped); the
		// queue goes once the deployment is gone.
		forgetGoneFunc(c.orders.ForgetPlacementHint),
	}
}

// forgetDeployment drops what the controller keeps in memory about a
// deployment that stopped, or everything once it is gone.
func (c *Controller) forgetDeployment(depID uint, gone bool) {
	for _, m := range c.deploymentMemories() {
		m.forget(depID, gone)
	}
}

package controller

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// poolTrialBudget is how many candidates one pass plans in full
// (planning.PoolTrialBudget).
const poolTrialBudget = planning.PoolTrialBudget

// poolRefusalMemory is how long a candidate whose trial refused a replica is
// tried after every other candidate by later passes. It is what moves the search down the ranking
// instead of re-planning the same best candidates forever when their room
// was only estimated; it is short, so a node that frees up is tried again
// soon.
const poolRefusalMemory = 2 * time.Minute

// poolClosest is how many refused candidates a pass records: the nodes
// nearest to running a replica, which the deployment's status names.
const poolClosest = 3

// unitPool is a deployment's candidate pool for one reconcile pass: read once
// when a replica first needs a node, and kept current as the pass places
// replicas.
type unitPool struct {
	*planning.Pool
	nodes int
	need  int
}

// poolOf builds the deployment's candidate pool (planning.BuildPool) from
// bulk reads - nodes with their GPUs, which GPUs awake instances hold, the
// model's files, the networks when a requirement names one - never one query
// per node.
func (c *Controller) poolOf(deployment *unitDeployment, model *store.Model) (*unitPool, error) {
	if deployment.pool != nil {
		return deployment.pool, nil
	}
	dep := deployment.dep
	nodes, others, err := c.poolNodes(deployment.requirements)
	if err != nil {
		return nil, err
	}
	held, err := planning.HeldGPUs(c.store, dep.YieldPolicy)
	if err != nil {
		return nil, err
	}
	files, err := c.poolModelFiles(model.ID, nodes, others)
	if err != nil {
		return nil, err
	}
	// Unresolvable networks or unreadable storages leave their requirements
	// unmet.
	facts := planning.LoadNodeFacts(c.store, c.networks.Resolve, nodes, deployment.requirements)
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		return nil, err
	}
	need := planning.ReplicaGPUNeed(dep)
	admission := planning.TargetRefusalFor(dep, deployment.requirements, func() (*store.Model, error) { return model, nil }, c.hub.Online)
	pool := &unitPool{nodes: len(nodes) + others, need: need, Pool: planning.BuildPool(planning.PoolFacts{
		Nodes: nodes, Held: held, Files: files, Facts: facts, Instances: deployment.insts,
		Online: c.hub.Online, Requirements: deployment.requirements, Strategy: dep.PlacementStrategy, Need: need, Placement: placement,
		TargetRefusal: admission.Refusal,
	})}
	if err := admission.Err(); err != nil {
		return nil, err
	}
	if others > 0 {
		// The nodes left unread fail the node requirement, as BuildPool would
		// have counted them.
		pool.Counts[planning.PoolUnmet] += others
		pool.UnmetByKey[nodelabel.KeyNode] += others
	}
	deployment.pool = pool
	return pool, nil
}

// poolModelFiles reads the model's files the pool ranks by: those on its
// nodes. A pool narrowed to named nodes (others > 0) reads only theirs.
func (c *Controller) poolModelFiles(modelID uint, nodes []store.Node, others int) ([]store.ModelFile, error) {
	if others == 0 {
		return c.store.ModelFilesForModel(modelID)
	}
	ids := make([]uint, 0, len(nodes))
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
	}
	return c.store.ModelFilesForModelOnNodes(modelID, ids)
}

// poolNodes reads the nodes a pool can hold. A deployment pinned to named
// nodes reads only those: every other node fails its node requirement, so
// reading a whole fleet on each pass (a 1000-node fleet of pinned
// deployments reconciles thousands of times) would only count them. others
// is how many nodes were left unread.
func (c *Controller) poolNodes(reqs nodelabel.Requirements) (nodes []store.Node, others int, err error) {
	named := reqs[nodelabel.KeyNode]
	if len(named) == 0 {
		nodes, err = c.store.ListNodes()
		return nodes, 0, err
	}
	if nodes, err = c.store.NodesByUUID(named); err != nil {
		return nil, 0, err
	}
	total, err := c.store.CountNodes()
	if err != nil {
		return nil, 0, err
	}
	return nodes, max(total-len(nodes), 0), nil
}

// pickPoolNode picks the node a pool replica starts on: the best-ranked
// candidate whose trial plan - the same preview the deploy form's automatic
// placement runs - has room, trying at most poolTrialBudget candidates, those
// refused within poolRefusalMemory last. When none has room the
// pass records why, once per timeout window like any unschedulable reason,
// and the replica waits; nothing is written. A candidate that fits once the
// deployments sharing its cards give them up is waited on only when none
// fits outright. The pick is counted against the
// candidate at once, so the pass's next replica ranks the pool as it will be.
func (c *Controller) pickPoolNode(ctx context.Context, deployment *unitDeployment, model *store.Model) (*store.Node, bool) {
	dep := deployment.dep
	pool, err := c.poolOf(deployment, model)
	if err != nil {
		c.log.Warn("deployment candidate pool", "dep", dep.ID, "err", err)
		return nil, false
	}
	if len(pool.Ranked) == 0 {
		params := map[string]any{"nodes": pool.nodes, "unmet": pool.Counts[planning.PoolUnmet], "cordoned": pool.Counts[planning.PoolCordoned], "offline": pool.Counts[planning.PoolOffline]}
		if pool.UnmetDetail != "" {
			params["detail"] = pool.UnmetDetail
		}
		c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolEmpty, Params: params})
		c.pool.wait(dep.ID, c.now())
		return nil, false
	}
	// Candidates refused within poolRefusalMemory go to the back of the
	// line rather than out of it: they are what a long pool skips to reach
	// untried ones, and what a short pool still plans every pass, so a
	// pinned replica starts the moment its one node has room.
	order := make([]int, 0, len(pool.Ranked))
	var refusedRecently []int
	for index := range pool.Ranked {
		if c.pool.refusedRecently(dep.ID, pool.Ranked[index].Node.ID, c.now()) {
			refusedRecently = append(refusedRecently, index)
			continue
		}
		order = append(order, index)
	}
	order = append(order, refusedRecently...)
	// A replica an operator asked to start on a named node tries it first,
	// once (planning.HintPlacement).
	hinted, hasHint := planning.NextPlacementHint(dep.ID)
	if hasHint {
		for i, index := range order {
			if pool.Ranked[index].Node.UUID == hinted {
				order = append(append([]int{index}, order[:i]...), order[i+1:]...)
				break
			}
		}
		planning.ConsumePlacementHint(dep.ID, hinted)
	}
	// A replica waiting for the holders of cards on one node to yield
	// (placewait) is rechecked every placewait.Recheck: that node is tried
	// first, and while it still fits only once they yield the wait goes on
	// without re-planning the rest of the pool. Only when it no longer fits
	// even then does the search go on down the ranking.
	waitedFirst := false
	if !hasHint {
		if w, waiting := c.waits.Get(dep.ID, c.now()); waiting {
			for i, index := range order {
				if pool.Ranked[index].Node.ID == w.Cards.NodeID {
					order = append(append([]int{index}, order[:i]...), order[i+1:]...)
					waitedFirst = true
					break
				}
			}
		}
	}
	tried := 0
	// closest are the candidates refused this pass, best first: what the
	// deployment's status shows as the nodes nearest to running a replica.
	var closest []map[string]any
	// yieldWait is the best candidate that fits once deployments sharing its
	// cards give them up: taken only when none fits outright.
	var yieldWait *scheduler.Reason
	var waitsFor *planning.LaunchYield
	for _, index := range order {
		if tried == poolTrialBudget {
			break
		}
		candidate := &pool.Ranked[index]
		tried++
		refusal, yield, err := c.trialPoolNode(ctx, dep, candidate.Node, model)
		if err == nil && refusal == nil {
			candidate.Replicas++
			candidate.FreeGPUs -= pool.need
			picked := candidate.Node
			pool.Ranked = planning.RankPool(pool.Ranked, dep.PlacementStrategy, pool.need)
			c.pool.settled(dep.ID)
			return picked, true
		}
		if err != nil {
			c.log.Warn("candidate trial plan", "dep", dep.ID, "node", candidate.Node.UUID, "err", err)
			refusal = &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}}
		}
		if yield != nil {
			if yieldWait == nil {
				yieldWait, waitsFor = refusal, yield
			}
			if waitedFirst && tried == 1 {
				break
			}
			continue
		}
		c.pool.refuse(dep.ID, candidate.Node.ID, c.now())
		if len(closest) < poolClosest {
			closest = append(closest, map[string]any{"node": candidate.Node.UUID, "name": candidate.Node.DisplayName(), "reason": *refusal})
		}
	}
	if yieldWait != nil {
		c.recordRefusal(dep, *yieldWait, waitsFor, nil)
		c.pool.wait(dep.ID, c.now())
		return nil, false
	}
	params := map[string]any{
		"candidates": len(pool.Ranked), "tried": tried,
		"busy": pool.Counts[planning.PoolBusy], "unmet": pool.Counts[planning.PoolUnmet], "cordoned": pool.Counts[planning.PoolCordoned], "offline": pool.Counts[planning.PoolOffline],
	}
	if len(closest) > 0 {
		// The best candidate's own reason, whole, so its figures and detail
		// reach the reader and not just its code.
		first := closest[0]["reason"].(scheduler.Reason)
		params["node"], params["first"], params["first_reason"], params["closest"] = closest[0]["name"], first.Code, first, closest
	}
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: params})
	c.pool.wait(dep.ID, c.now())
	return nil, false
}

// trialPoolNode plans a replica on node as a preview: the estimate, the
// scheduler against the node's ledger and the template's hardware contract,
// without the model being there yet, a port reserved or anything written. A
// non-nil refusal is the scheduler's reason, or the model being stuck there;
// yield is what a refusal that waits for holders to yield waits for.
func (c *Controller) trialPoolNode(ctx context.Context, dep *store.Deployment, node *store.Node, model *store.Model) (refusal *scheduler.Reason, yield *planning.LaunchYield, err error) {
	// A node where the model is stuck would take the replica and then hold
	// it there; the pool passes it over until the model is sorted out.
	if refusal, err := c.modelUnavailable(dep, node, model); refusal != nil || err != nil {
		return refusal, nil, err
	}
	req, templateLayer, _, err := c.launchRequest(dep, node, model, nil)
	if err != nil {
		return nil, nil, err
	}
	topology, err := planning.RankTopology(node, nil, planning.GroupRank{Ranks: 1})
	if err != nil {
		return nil, nil, err
	}
	req.Preview, req.Topology = true, topology
	plan, refusal, err := c.planInstanceLaunch(ctx, dep, node, req, templateLayer)
	return refusal, plan.YieldWait(refusal), err
}

// forgetPool drops what the controller remembers about a deployment's pool,
// for a deployment that is gone or whose requirements changed.
func (c *Controller) forgetPool(depID uint) {
	c.pool.forget(depID)
	c.waits.Done(depID)
}

// poolSettleBackoff is how long a deployment whose last pass found no node
// is left out of routine node status batches (settleNodeStatus). A pass
// over a thousand-node pool plans up to poolTrialBudget candidates; retrying
// every waiting deployment on every batch would keep the controller's one
// worker busy repeating refusals. Changes that can unblock a deployment
// directly - a node coming online, labels, edited requirements, its own
// instances - still reconcile it at once.
const poolSettleBackoff = 20 * time.Second

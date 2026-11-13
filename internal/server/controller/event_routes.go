package controller

import (
	"sort"
	"sync"
	"time"
)

// eventRouteSettle is how often the changes noted by events are turned into
// reconciles. A server restart or a network blip reconnects every agent at
// once: thousands of node, model-file and storage events in a few seconds.
// Reading the deployments for each of them made the reconcile cost scale with
// events × deployments; noting them and acting once per tick makes it scale
// with the deployments they touch.
const eventRouteSettle = 250 * time.Millisecond

// eventRoutes accumulates, between ticks, what bus events said changed. It is
// written by the bus subscriber and taken by the settle tick.
type eventRoutes struct {
	mu sync.Mutex
	// all asks for every deployment: a change rare enough (an operator's
	// storage or pull-rule edit) that indexing who it concerns is not worth it.
	all bool
	// unsettled asks for every deployment not settled (deploymentSettled): a
	// node, model file or storage changed in a way that may unblock one.
	unsettled bool
	// nodes are node UUIDs whose own deployments need a pass: an offline,
	// relabelled or draining node concerns the replicas on it even when their
	// deployment is settled.
	nodes map[string]struct{}
	// modelFiles are model-file keys that changed. The tick resolves each
	// once, however many events named it, to refresh its storage cache and to
	// reconcile the active deployments of its model (downloadDependentIDs).
	modelFiles map[string]struct{}
	// readers are node ids whose cache readers changed; their waiting cache
	// operations are retried (cacheReferencesChanged).
	readers map[uint]struct{}
}

// eventRouteBatch is one tick's worth of eventRoutes.
type eventRouteBatch struct {
	all, unsettled bool
	nodes          []string
	modelFiles     []string
	readers        []uint
}

func (r *eventRoutes) noteAll() {
	r.mu.Lock()
	r.all = true
	r.mu.Unlock()
}

func (r *eventRoutes) noteUnsettled() {
	r.mu.Lock()
	r.unsettled = true
	r.mu.Unlock()
}

func (r *eventRoutes) noteNode(uuid string) {
	r.mu.Lock()
	r.unsettled = true
	if r.nodes == nil {
		r.nodes = map[string]struct{}{}
	}
	r.nodes[uuid] = struct{}{}
	r.mu.Unlock()
}

func (r *eventRoutes) noteModelFile(key string) {
	r.mu.Lock()
	r.unsettled = true
	if r.modelFiles == nil {
		r.modelFiles = map[string]struct{}{}
	}
	r.modelFiles[key] = struct{}{}
	r.mu.Unlock()
}

func (r *eventRoutes) noteReaders(nodeIDs ...uint) {
	if len(nodeIDs) == 0 {
		return
	}
	r.mu.Lock()
	if r.readers == nil {
		r.readers = map[uint]struct{}{}
	}
	for _, id := range nodeIDs {
		r.readers[id] = struct{}{}
	}
	r.mu.Unlock()
}

// take empties the routes and returns what they held.
func (r *eventRoutes) take() eventRouteBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := eventRouteBatch{all: r.all, unsettled: r.unsettled}
	for uuid := range r.nodes {
		b.nodes = append(b.nodes, uuid)
	}
	for key := range r.modelFiles {
		b.modelFiles = append(b.modelFiles, key)
	}
	for id := range r.readers {
		b.readers = append(b.readers, id)
	}
	sort.Strings(b.nodes)
	sort.Strings(b.modelFiles)
	sort.Slice(b.readers, func(i, j int) bool { return b.readers[i] < b.readers[j] })
	r.all, r.unsettled, r.nodes, r.modelFiles, r.readers = false, false, nil, nil, nil
	return b
}

// flushEventRoutes enqueues what the events since the last tick concern. Any
// read that fails, or a node that can no longer be found, widens the pass to
// every deployment, and a pass that cannot even list them is kept for the next
// tick: a doubt costs reconciles, never a missed one.
func (c *Controller) flushEventRoutes() {
	b := c.routes.take()
	if len(b.readers) > 0 {
		ops, err := c.store.WaitingCacheOperations(b.readers)
		if err != nil {
			c.log.Warn("find cache operations for changed references", "nodes", b.readers, "err", err)
			c.routes.noteReaders(b.readers...) // try again next tick
		}
		for _, op := range ops {
			c.cacheQ.add(cacheKey(op.StorageID, op.NodeID))
		}
	}
	if b.all {
		c.enqueueAllDeployments()
		return
	}
	if len(b.nodes) > 0 {
		ids, unknown, err := c.store.DeploymentIDsOnNodes(b.nodes)
		if err != nil || unknown {
			c.enqueueAllDeployments()
			return
		}
		for _, id := range ids {
			c.q.add(depKey(id))
		}
	}
	if len(b.modelFiles) > 0 {
		var models []uint
		lost := false
		for _, key := range b.modelFiles {
			row, err := c.store.ResolveModelFileKey(key)
			if err != nil {
				lost = true // a deleted row no longer says whose model it was
				continue
			}
			c.cacheModelFileChanged(row)
			models = append(models, row.ModelID)
		}
		if lost {
			c.enqueueAllDeployments()
			return
		}
		ids, err := c.downloadDependentIDs(models)
		if err != nil {
			c.enqueueAllDeployments()
			return
		}
		for _, id := range ids {
			c.q.add(depKey(id))
		}
	}
	if b.unsettled {
		facts, err := c.store.DeploymentSettleFacts()
		if err != nil {
			c.log.Warn("could not read deployment settle facts", "err", err)
			c.enqueueAllDeployments()
			return
		}
		// Unlike a routine status report (settleNodeStatus), these changes
		// are what a deployment that found no node was waiting for, so its
		// pool backoff does not hold it back.
		for _, fact := range facts {
			if !deploymentSettled(fact) {
				c.q.add(depKey(fact.ID))
			}
		}
	}
}

// enqueueAllDeployments queues every deployment by id.
func (c *Controller) enqueueAllDeployments() {
	ids, err := c.store.DeploymentIDs()
	if err != nil {
		c.log.Warn("could not list deployments to reconcile", "err", err)
		c.routes.noteAll() // try again next tick
		return
	}
	for _, id := range ids {
		c.q.add(depKey(id))
	}
}

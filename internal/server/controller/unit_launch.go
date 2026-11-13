package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// groupPollInterval is how often a generation waiting for its ranks' model
// and image is re-checked.
const groupPollInterval = 10 * time.Second

// imagePrePuller pulls an image onto a node ahead of any launch; the hub
// implements it (hub.PullImage) and records the result on the image's node
// row.
type imagePrePuller interface {
	PullImage(ctx context.Context, imageID uint, targetNodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error)
}

// imagePrePulled reports whether the deployment's image is on the node,
// asking the node to pull it when it is not, from the same candidates a
// launch would try (the template's sources included). An image the catalog
// does not know, or a hub that cannot pre-pull, leaves the pull to the launch
// as for any other instance. A pull that failed is said on the deployment and
// tried again.
func (c *Controller) imagePrePulled(ctx context.Context, dep *store.Deployment, node *store.Node) bool {
	puller, ok := c.hub.(imagePrePuller)
	if !ok {
		return true
	}
	image, err := c.store.ResolveImageRef(dep.Image)
	if err != nil {
		return true
	}
	if row, err := c.store.ImageNode(image.ID, node.ID); err == nil {
		if row.State == store.ImageStatePresent {
			return true
		}
		if row.State == store.ImageStateFailed && row.Source != "peer" {
			c.recordUnschedulable(dep, scheduler.ImageDeliveryFailed(node.UUID, dep.Image, row.Source, row.Message))
		}
	}
	fetch := imagecatalog.FetchRequest{Copies: c.deploymentImageCopies(dep)}
	// PullImage returns the running attempt instead of starting another.
	if _, err := puller.PullImage(ctx, image.ID, node.UUID, fetch); err != nil {
		c.log.Warn("generation image pre-pull", "dep", dep.ID, "node", node.UUID, "err", err)
	}
	return false
}

// deploymentImageCopies are the references that name a deployment's image:
// its template's sources, the catalog's, the image itself.
func (c *Controller) deploymentImageCopies(dep *store.Deployment) []string {
	selection, _ := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	return c.store.ImageCopies(dep.Image, selection.ImageSourcesFor(dep.Image))
}

// rankLaunch is one rank of a generation, planned and not yet written: the
// node it runs on, its assembled launch, and what its instance row records.
type rankLaunch struct {
	node          *store.Node
	plan          planning.LaunchPlan
	port          int
	modelHostPath string
	// reads are the model files the instance mounts.
	reads  []store.InstanceModelFile
	extras instanceExtras
}

// launchGeneration launches one generation of a unit on the picked nodes:
// the leader, then every follower with the leader's group identity. A
// generation is placed as a whole (a gang): every rank is planned first, and
// only when all of them fit are their instances written, in one transaction.
// A rank that does not fit leaves nothing behind - no card held by a leader
// whose follower has nowhere to go, no failed attempt against the retry budget
// - and records why, so the replica waits for room. It reports whether the
// generation was written.
func (c *Controller) launchGeneration(ctx context.Context, deployment *unitDeployment, model *store.Model, slot planning.UnitSlot, nodes []*store.Node, attempt int) (bool, error) {
	if refused := c.refuseSharedNode(deployment.dep, nodes); refused {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	nodeIDs := make([]uint, len(nodes))
	for i, node := range nodes {
		nodeIDs[i] = node.ID
	}
	release, pinned := c.store.TryPinNodeCaches(nodeIDs)
	if !pinned {
		c.q.addAfter(depKey(deployment.dep.ID), groupPollInterval)
		return false, nil
	}
	// Keep every ready model component alive until persistGeneration commits
	// the instance references used by eviction's final eligibility check.
	// PD ranks use the same barrier before their readiness lookup below.
	defer release()
	if deployment.pd != nil {
		return c.launchPDGeneration(ctx, deployment, model, slot, nodes, attempt)
	}
	dep := deployment.dep
	var group netplan.Group
	rank := planning.GroupRank{Ranks: len(nodes)}
	if slot.Unit.MultiNode() {
		// Every rank uses the network its replica's nodes share, decided
		// afresh at each launch so network changes reach newly started
		// instances. Replicas exchange nothing, so each resolves its own.
		members := make([]store.Node, 0, len(nodes))
		for _, node := range nodes {
			members = append(members, *node)
		}
		resolved, err := c.networks.ComputeGroup(members)
		if err != nil {
			return false, fmt.Errorf("deployment %d compute network: %w", dep.ID, err)
		}
		if err := planning.CheckGroupNetwork(members, resolved); err != nil {
			return false, fmt.Errorf("deployment %d compute network: %w", dep.ID, err)
		}
		group = resolved
		// Each generation is its own group: two replicas launched at the
		// same attempt must not read as one generation.
		prefix := dep.DistGroup
		if prefix == "" {
			prefix = fmt.Sprintf("deployment-%d", dep.ID)
		}
		rank.Group, rank.Attempt = prefix+"-"+auth.NewUUID()[:8], attempt
		master, err := replicaMaster(dep, nodes[0], group)
		if err != nil {
			return false, err
		}
		rank.Master = master
	}
	launches := make([]rankLaunch, 0, len(nodes))
	for index, node := range nodes {
		rank.Rank = index
		var unit *planning.UnitRecord
		if index == 0 {
			unit = &planning.UnitRecord{Key: slot.Key, Followers: slot.Unit.FollowersKey()}
		}
		launch, refusal, yield, err := c.planRank(ctx, dep, node, model, group, rank, unit)
		if err != nil {
			return false, err
		}
		if launch == nil {
			c.recordRankWait(dep, refusal, yield, rank, node)
			return false, nil
		}
		launches = append(launches, *launch)
	}
	return true, c.persistGeneration(ctx, dep, launches)
}

// refuseSharedNode refuses a generation that would put two of its ranks on one
// node. Ranks are planned before any is written, so each is checked against
// the node as it stands; two on the same node would each count the same free
// cards. Replica lists are validated to name distinct nodes, so this only
// guards against a placement that slipped past that.
func (c *Controller) refuseSharedNode(dep *store.Deployment, nodes []*store.Node) bool {
	seen := make(map[uint]int, len(nodes))
	for index, node := range nodes {
		if first, dup := seen[node.ID]; dup {
			c.recordUnschedulableRank(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{
				"detail": fmt.Sprintf("ranks %d and %d of one replica are on the same node", first, index)}}, &rankPlace{Rank: index, Node: node})
			return true
		}
		seen[node.ID] = index
	}
	return false
}

// recordRankRefusal records why a rank held its generation back. A nil
// refusal is a rank whose model is still arriving, which says so elsewhere.
func (c *Controller) recordRankRefusal(dep *store.Deployment, refusal *scheduler.Reason, rank planning.GroupRank, node *store.Node) {
	c.recordRankWait(dep, refusal, nil, rank, node)
}

// recordRankWait is recordRankRefusal with what a refusal that waits for
// holders to yield waits for (LaunchPlan.YieldWait).
func (c *Controller) recordRankWait(dep *store.Deployment, refusal *scheduler.Reason, yield *planning.LaunchYield, rank planning.GroupRank, node *store.Node) {
	if refusal == nil {
		return
	}
	var at *rankPlace
	if rank.Ranks > 1 {
		at = &rankPlace{Rank: rank.Rank, Node: node}
	}
	c.recordRefusal(dep, *refusal, yield, at)
}

// persistGeneration writes a planned generation's instances in one
// transaction, then starts each on its node and re-queues the deployment.
func (c *Controller) persistGeneration(ctx context.Context, dep *store.Deployment, launches []rankLaunch) (err error) {
	insts := make([]*store.Instance, 0, len(launches))
	reads := make([][]store.InstanceModelFile, 0, len(launches))
	observation := c.observationFor(dep)
	for _, launch := range launches {
		reads = append(reads, launch.reads)
		startTimeout := launch.plan.Resolved.StartTimeoutSeconds
		if startTimeout <= 0 {
			startTimeout = c.instanceStartTimeout(dep)
		}
		if c.startTimeout > 0 {
			startTimeout = c.startTimeout
		}
		inst, err := scheduledInstance(dep, launch.plan, launch.port, launch.modelHostPath, startTimeout, launch.node, launch.extras, observation)
		if err != nil {
			return err
		}
		insts = append(insts, inst)
	}
	reserved := 0
	persisted := false
	defer func() {
		if persisted {
			return
		}
		for index := range reserved {
			launch := launches[index]
			if releaseErr := c.inferenceNodeHandlerCreateFailed(ctx, launch.node, launch.plan.Output.Result.Plan.GPUIndexes, insts[index].UUID); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("external resource compensation: %w", releaseErr))
			}
		}
	}()
	for index, inst := range insts {
		launch := launches[index]
		if reserveErr := c.inferenceNodeHandlerBeforeCreate(ctx, launch.node, launch.plan.Output.Result.Plan.GPUIndexes, inst.UUID); reserveErr != nil {
			return fmt.Errorf("deployment %d external resource reservation: %w", dep.ID, reserveErr)
		}
		reserved++
	}
	if err := c.store.CreateInstances(insts, reads); err != nil {
		return err
	}
	persisted = true
	c.waits.Done(dep.ID)
	// Every rank is pushed even when one push fails: the rows are written,
	// and a rank whose push is skipped would wait for a push nothing else
	// makes. A node that missed its push gets the desired state when it
	// reconnects.
	var pushErrs []error
	for index, inst := range insts {
		if err := c.startScheduledInstance(dep, launches[index].node, inst); err != nil {
			pushErrs = append(pushErrs, err)
		}
	}
	// CreateInstances emits instance events, but they can race the
	// controller's own reconciliation window (for example when an in-process
	// hub reports the desired state before the subscription observes the DB
	// commit). Explicitly queue the deployment after a successful push so a
	// replicas>1 deployment cannot stall forever after its first replica.
	c.q.add(depKey(dep.ID))
	return errors.Join(pushErrs...)
}

// planRank assembles and checks one rank of an ordinary or multi-node
// deployment without writing it. A nil launch means the rank cannot start
// now: refusal says why, or is nil while the rank's model is still arriving;
// yield is what a refusal that waits for holders to yield waits for.
func (c *Controller) planRank(ctx context.Context, dep *store.Deployment, node *store.Node, model *store.Model, group netplan.Group, rank planning.GroupRank, unit *planning.UnitRecord) (*rankLaunch, *scheduler.Reason, *planning.LaunchYield, error) {
	files, ready, err := c.ensureDeploymentModelsPresent(ctx, node, dep, model)
	if err != nil {
		return nil, nil, nil, err
	}
	if !ready {
		refusal, err := c.modelUnavailable(dep, node, model)
		return nil, refusal, nil, err
	}
	req, templateLayer, modelHostPath, err := c.launchRequest(dep, node, model, &files)
	if err != nil {
		return nil, nil, nil, err
	}
	if rank.Ranks > 1 {
		// A multi-node generation rendezvous on its own replica's leader,
		// never on the address stored for another replica's.
		req.MasterAddr = rank.Master
	}
	var compute *netplan.Resolved
	if resolved, ok := group.Ranks[node.ID]; ok {
		compute = &resolved
	}
	topology, err := planning.RankTopology(node, compute, rank)
	if err != nil {
		return nil, nil, nil, err
	}
	port := 0
	if !topology.Headless {
		var rejected *scheduler.Reason
		if port, rejected = c.pickPort(node); rejected != nil {
			return nil, rejected, nil, nil
		}
	}
	req.Topology, req.APIPort = topology, port
	plan, refusal, err := c.planInstanceLaunch(ctx, dep, node, req, templateLayer)
	if err != nil || refusal != nil {
		return nil, refusal, plan.YieldWait(refusal), err
	}
	ports := []int{}
	if !topology.Headless {
		ports = []int{port}
	}
	return &rankLaunch{node: node, plan: plan, port: port, modelHostPath: modelHostPath, reads: files.reads(),
		extras: instanceExtras{Ports: ports, ExplainNetwork: topology.NNodes > 1, Unit: unit}}, nil, nil, nil
}

// replicaMaster is where a multi-node generation's ranks rendezvous: the
// deployment's stored master address when the replica is led from the node it
// was stored for (the first replica's leader; the store clears it when the
// list leads the first replica from another node), otherwise the leader's
// address on the network its replica shares. Leaders of different replicas
// are on different nodes, so they never share a master port.
func replicaMaster(dep *store.Deployment, leader *store.Node, group netplan.Group) (string, error) {
	if dep.MasterAddr != "" && leader.ID == dep.NodeID {
		return dep.MasterAddr, nil
	}
	if resolved, ok := group.Ranks[leader.ID]; ok && resolved.IP != "" {
		return resolved.IP, nil
	}
	return "", fmt.Errorf("deployment %d: replica leader %s has no address on its compute network", dep.ID, leader.DisplayName())
}

// instanceStartTimeout is the fallback for an older allocation with no resolved
// budget. Current launches take ResolveLaunch's decision; the harness override
// still wins so short hermetic failure paths remain testable.
func (c *Controller) instanceStartTimeout(dep *store.Deployment) int {
	if c.startTimeout > 0 {
		return c.startTimeout
	}
	if dep != nil && dep.StartTimeoutSec > 0 {
		return dep.StartTimeoutSec
	}
	return c.store.DeploymentPolicy().StartTimeoutSec
}

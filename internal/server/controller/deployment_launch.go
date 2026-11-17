package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentModels is every model a deployment's instances read: its own,
// then each distinct component model its template binds.
func (c *Controller) deploymentModels(dep *store.Deployment, primary *store.Model) ([]*store.Model, error) {
	ids, err := planning.DeploymentModelIDs(dep)
	if err != nil {
		return nil, err
	}
	models := []*store.Model{primary}
	for _, id := range ids[1:] {
		model, err := c.store.GetModel(id)
		if err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, nil
}

// modelUnavailable is why a replica cannot start on node because of where
// its models come from (planning.SourceRefusal): no storage can serve the
// node, or an acquisition or cache fill there failed or waits for an operator
// to pick a source. A model still arriving is not stuck and gives nil.
// Without it a replica waiting on such a node shows no reason at all, and a
// pool replica would keep picking that node.
func (c *Controller) modelUnavailable(dep *store.Deployment, node *store.Node, primary *store.Model) (*scheduler.Reason, error) {
	models, err := c.deploymentModels(dep, primary)
	if err != nil {
		return nil, err
	}
	rules, err := c.sourceRules(dep)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		src, err := c.store.ModelSourceOn(model.ID, node, rules)
		if err != nil {
			return nil, err
		}
		if refusal := planning.SourceRefusal(src, node, model); refusal != nil {
			return refusal, nil
		}
	}
	return nil, nil
}

// recordUnschedulable makes a placement failure visible, and arranges for the
// deployment to be re-checked on its own.
//
// The scheduler already worked out why: Result.Message is a Reason, and that
// type is documented as "the shared explanation unit (feeds instance_events and
// the topo graph)". The old code wrote it to a single log line and dropped the
// rest, which left a deployment that could never be placed permanently stuck:
// no instance existed, so nothing was written to instances.schedule_expl; no
// event was published, and the work queue is purely event-driven, so nothing
// would reconcile the deployment again; and deriveRuntimeState reports
// `pending` for any deployment with no instance and present weights, which is
// indistinguishable from one that is merely queued.
//
// The deferred re-enqueue is the other half. Because the queue keeps the
// EARLIEST deadline per key, this never postpones a due retry - it only ever
// guarantees one happens, so a deployment that becomes placeable later recovers
// without an operator touching it.
//
// Events are written at most once per timeout window. Reconciling is cheap and
// happens on every node/model/deployment change; recording each attempt would
// bury the timeline under duplicate rows and turn "still unschedulable" into
// noise instead of a signal.
func (c *Controller) recordUnschedulable(dep *store.Deployment, reason scheduler.Reason) {
	c.recordUnschedulableRank(dep, reason, nil)
}

// reconnectRecheck is how soon a deployment held back only by nodes whose
// agents have not reconnected since the server started is looked at again.
const reconnectRecheck = 5 * time.Second

// agentsReconnecting says a refusal names offline nodes while the server is
// still within its offline grace of starting to watch them and some node has
// not reported since: its agent is reconnecting, and the refusal is not
// recorded. A node that reported since and then went offline is a real
// refusal, recorded as ever.
func (c *Controller) agentsReconnecting(reason scheduler.Reason) bool {
	if c.watchingSince.IsZero() || c.now().Sub(c.watchingSince) >= c.offlineGrace {
		return false
	}
	offline := 0
	switch v := reason.Params["offline"].(type) {
	case int:
		offline = v
	case float64:
		offline = int(v)
	}
	if offline <= 0 {
		return false
	}
	unseen, err := c.store.CountNodesUnseenSince(c.watchingSince)
	return err == nil && unseen > 0
}

// refusalChangeFloor is the least time between two recorded refusals of one
// deployment that differ.
const refusalChangeFloor = 10 * time.Second

// refusalIdentity are the params that make a refusal a different one: the
// best candidate's own refusal, and the counts of where the deployment may
// run - nodes, those its requirements exclude, those cordoned, those
// offline. An edit to the requirements or a node going away is news. How
// busy the candidates are is not: it moves with every other deployment.
var refusalIdentity = []string{"first", "nodes", "unmet", "cordoned", "offline"}

// sameRefusal reports whether a recorded schedule event is the refusal about
// to be recorded: the same code and refusalIdentity params, for the same rank
// on the same node.
func sameRefusal(data string, reason scheduler.Reason, at *rankPlace) bool {
	var last struct {
		Scheduler struct {
			Code   string                     `json:"code"`
			Params map[string]json.RawMessage `json:"params"`
		} `json:"scheduler"`
		Rank *int   `json:"rank"`
		Node string `json:"node"`
	}
	if json.Unmarshal([]byte(data), &last) != nil || last.Scheduler.Code != reason.Code {
		return false
	}
	for _, key := range refusalIdentity {
		value, stated := reason.Params[key]
		recorded, wasStated := last.Scheduler.Params[key]
		if stated != wasStated {
			return false
		}
		if !stated {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != string(recorded) {
			return false
		}
	}
	if at == nil {
		return last.Rank == nil
	}
	return last.Rank != nil && *last.Rank == at.Rank && last.Node == at.Node.UUID
}

// rankPlace names the rank of a multi-node generation that could not be
// placed, so the refusal says which one held the whole replica back.
type rankPlace struct {
	Rank int
	Node *store.Node
}

// recordUnschedulableRank is recordUnschedulable for one rank of a
// generation; at is nil for a single-node replica.
func (c *Controller) recordUnschedulableRank(dep *store.Deployment, reason scheduler.Reason, at *rankPlace) {
	c.recordRefusal(dep, reason, nil, at)
}

// recordRefusal records why a replica cannot be placed now; yield is what a
// refusal that waits for holders to yield waits for (LaunchPlan.YieldWait),
// nil for any other.
func (c *Controller) recordRefusal(dep *store.Deployment, reason scheduler.Reason, yield *planning.LaunchYield, at *rankPlace) {
	c.noteYieldWait(dep, reason, yield)
	if c.agentsReconnecting(reason) {
		// Just after the server starts, a node counts offline until its
		// agent reconnects: that is not a refusal worth showing. Look again
		// soon (a node coming online also queues the deployment).
		c.q.addAfter(depKey(dep.ID), reconnectRecheck)
		return
	}
	payload := map[string]any{"scheduler": reason}
	if at != nil {
		payload["rank"], payload["node"] = at.Rank, at.Node.UUID
	}

	policy := c.store.DeploymentPolicy()
	window := policy.PendingTimeout()
	if last, found, err := c.store.LatestScheduleEvent(dep.ID); err == nil && found {
		// A different refusal is news and is written at once: the latest
		// event is what the deployment's status shows, and one recorded
		// while the agents were still reconnecting must not stand for a
		// whole window. The same refusal again is the noise this throttles.
		// refusalChangeFloor still spaces two different refusals, so a
		// deployment flapping between two never writes a row per pass.
		since := c.now().Sub(last.CreatedAt)
		if since < window && sameRefusal(last.Data, reason, at) {
			c.q.addAfter(depKey(dep.ID), window-since)
			return
		}
		if since < refusalChangeFloor && refusalChangeFloor < window {
			c.q.addAfter(depKey(dep.ID), refusalChangeFloor-since)
			return
		}
	}
	// The Reason is stored whole: it is already the structured, locale-neutral
	// form (stable Code + Params), so the human-readable text stays the
	// client's job.
	// Logged with the event, not on every pass: a waiting deployment is
	// re-planned whenever a node reports, and the same refusal each time is
	// what the throttle above keeps out of the event log - and the server log.
	if at != nil {
		c.log.Info("deployment not schedulable yet", "dep", dep.ID,
			"code", reason.Code, "reason", reason.Message, "params", reason.Params, "rank", at.Rank, "node", at.Node.UUID)
	} else {
		c.log.Info("deployment not schedulable yet", "dep", dep.ID,
			"code", reason.Code, "reason", reason.Message, "params", reason.Params)
	}
	c.store.AddDeploymentEventCode(dep.ID, store.EventKindSchedule,
		store.EventDeploymentUnschedulable, payload)
	c.q.addAfter(depKey(dep.ID), window)
}

// launchRequest is the part of a launch every instance of a deployment shares
// (planning.DeploymentLaunch). Nil files ask for a preview of the node before
// the model is there (trialPoolNode): nothing is mounted.
func (c *Controller) launchRequest(dep *store.Deployment, node *store.Node, model *store.Model, files *deploymentModels) (planning.LaunchRequest, deploy.CatalogSelection, string, error) {
	// Every deployment's requirements say which storages its model may come
	// from; a multi-node one's say nothing else DeploymentLaunch reads.
	reqs, err := c.store.DeploymentRequirements(dep.ID)
	if err != nil {
		return planning.LaunchRequest{}, deploy.CatalogSelection{}, "", fmt.Errorf("deployment %d node requirements: %w", dep.ID, err)
	}
	req, templateLayer, modelHostPath, err := planning.DeploymentLaunch(dep, reqs, node, model, files.paths())
	req.ImplicitServedName = c.translatesServedName(node)
	return req, templateLayer, modelHostPath, err
}

// planInstanceLaunch assembles the launch and checks it against the node and
// the template's hardware contract. A non-nil refusal says why the launch
// cannot be placed now; recording it is the caller's, which knows the rank.
func (c *Controller) planInstanceLaunch(ctx context.Context, dep *store.Deployment, node *store.Node, req planning.LaunchRequest, templateLayer deploy.CatalogSelection) (planning.LaunchPlan, *scheduler.Reason, error) {
	plan, refusal, err := planning.PlanTemplateLaunch(ctx, c.store, c.networks, node, req, templateLayer)
	if reason, ok := planning.ImageSourceRefusal(err, node, dep.Image); ok {
		// The operator's rules forbid every source on this node: a refusal
		// the deployment shows, not an error to retry.
		return plan, &reason, nil
	}
	if err != nil {
		return plan, nil, fmt.Errorf("deployment %d: %w", dep.ID, err)
	}
	return plan, refusal, nil
}

// instanceExtras is what differs between the instances persistScheduledInstance
// writes: every host port the instance holds (so later allocations avoid
// them), its PD role, and whether its compute network is recorded.
type instanceExtras struct {
	Ports          []int
	Role           string
	ExplainNetwork bool
	// PD is the instance's launch record, read back from the schedule
	// explanation by the gateway and the controller (ADR 0028).
	PD *planning.PDRecord
	// Unit is set on a multi-node unit's leader: the unit it was launched
	// for (ADR 0029).
	Unit *planning.UnitRecord
}

// scheduledInstance builds the instance row and desired state for a planned
// launch without writing anything; persistGeneration writes a generation's
// rows together.
func scheduledInstance(dep *store.Deployment, plan planning.LaunchPlan, port int, modelHostPath string, startTimeout int, node *store.Node, extras instanceExtras, observation deploy.Observation) (*store.Instance, error) {
	gpus := plan.Output.Result.Plan.GPUIndexes
	// The cards are bound by hardware identity; the indexes are how the agent
	// and the ledger address them until the next probe renumbers them.
	gpuIdents := store.CardIdentsFor(node, gpus)
	topology := plan.Topology
	ports := extras.Ports
	if ports == nil {
		ports = []int{}
	}
	sleepAPI := ""
	if !topology.Headless && plan.Profile.Sleep.Supported {
		sleepAPI = "/sleep"
	}
	spec := proto.InstanceDesiredState{
		ResolvedLaunch:      plan.Resolved,
		UUID:                auth.NewUUID(),
		DeploymentID:        dep.ID,
		DesiredState:        "running",
		Image:               plan.Source.Image,
		ImagePullRefs:       plan.ImagePull.Refs(),
		ImageSources:        plan.ImagePull.Sources,
		ImagePullCandidates: plan.ImagePull.Candidates,
		Engine:              dep.Engine,
		SleepAPI:            sleepAPI,
		LogDialect:          dep.Engine,
		GPUIndexes:          gpus,
		GPUIdents:           gpuIdents,
		ServedModelName:     planning.ExpectedServedName(plan.Resolved, plan.Params.ImplicitServedName),
		HostNetwork:         true,
		Ports:               ports,
		ModelPath:           modelHostPath,
		APIPort:             port,
		StartTimeout:        startTimeout,
		DistGroup:           topology.Group,
		DistRole:            topology.Role,
		NNodes:              topology.NNodes,
		NodeRank:            topology.NodeRank,
		Headless:            topology.Headless,
		DistAttempt:         topology.Attempt,
		Sleepable:           !topology.Headless && dep.Sleepable && plan.Profile.Sleep.Supported,
		DesiredSleep:        dep.DesiredSleep,
		VRAMLimitBytes:      dep.VRAMLimitBytes,
		MemoryClaimBytes:    plan.Output.Result.Plan.ClaimBytes,
		PartitionBackend:    dep.PartitionBackend,
		SMLimitPct:          dep.SMLimitPct,
	}
	// A headless rank serves no API, so there is nothing of it to observe.
	if !topology.Headless {
		observation.Apply(&spec)
	}
	specJSON, _ := json.Marshal(spec)
	// schedule_explain carries the chosen plan + topology highlight (for the
	// topology graph), the estimate breakdown (for the VRAM decomposition) and
	// how the instance was launched (planning.ScheduleRecord).
	expl := planning.NewScheduleRecord(plan, extras.ExplainNetwork, extras.PD, extras.Unit).Encode()
	inst := &store.Instance{
		UUID:             spec.UUID,
		DeploymentID:     dep.ID,
		NodeID:           node.ID,
		State:            proto.InstPending,
		GPUIndexes:       jsonInts(gpus),
		GPUIdents:        jsonStrings(gpuIdents),
		Ports:            jsonInts(ports),
		APIPort:          port,
		Image:            spec.Image,
		ImageDigestID:    plan.ImagePull.DigestID,
		DesiredStateJSON: string(specJSON),
		ScheduleExpl:     expl,
		DistGroup:        topology.Group,
		DistRole:         topology.Role,
		NNodes:           topology.NNodes,
		NodeRank:         topology.NodeRank,
		Headless:         topology.Headless,
		DistAttempt:      topology.Attempt,
		Role:             extras.Role,
	}
	return inst, nil
}

// startScheduledInstance pushes a persisted instance's desired state to its
// node, or fails it when the node's serving path cannot reach it.
func (c *Controller) startScheduledInstance(dep *store.Deployment, node *store.Node, inst *store.Instance) error {
	// The store starts the instance under the deployment's standing sleep
	// order, read as it inserts the row; push what it stored.
	spec, err := inst.DesiredState()
	if err != nil {
		return err
	}
	c.log.Info("scheduled instance", "dep", dep.ID, "instance", spec.UUID, "gpus", spec.GPUIndexes, "port", spec.APIPort)
	if err := c.checkServingPath(context.Background(), node); err != nil {
		// The instance could never be reached: fail it with the reason instead
		// of starting a container nobody can call.
		c.log.Warn("instance not started: serving path unavailable", "dep", dep.ID, "instance", spec.UUID, "node", node.UUID, "err", err)
		return c.store.SetInstanceFailed(spec.UUID, proto.RuntimeFailure{Code: proto.ErrorServingPathUnavailable, Phase: proto.PhaseContainerPrepare, Node: node.DisplayName(), Detail: err.Error()}, err.Error())
	}

	if err := c.hub.PushToNode(node.UUID, proto.StateUpdate{Instances: []proto.InstanceDesiredState{spec}}); err != nil {
		return err
	}
	c.recordSleepPushed(dep.ID, spec)
	return nil
}

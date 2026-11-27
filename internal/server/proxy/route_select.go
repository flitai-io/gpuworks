package proxy

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// selectDeploymentInstance keeps API-contract authorization and data-plane
// routing on the same rollout. Multiple active deployments with one served
// name are valid during replacement; a pending rollout must not shadow an
// older deployment that is already serving.
//
// The final bool return is asleep: it is true only when the returned 503 is
// specifically a "model is asleep" rejection, so accounting can classify it as
// asleep_503 by this explicit flag rather than by matching rejection text.
func (p *Proxy) selectDeploymentInstance(modelName string) (*store.Deployment, target, *refusal, bool) {
	return p.selectDeploymentInstanceWithKey(modelName, "")
}

func (p *Proxy) selectDeploymentInstanceWithKey(modelName, affinity string) (*store.Deployment, target, *refusal, bool) {
	return p.selectScopedDeploymentInstance(nil, modelName, affinity, admission{})
}

// selectScopedDeploymentInstance is selectDeploymentInstanceWithKey over the
// deployments the caller's API key may call (nil: all). A name served only by
// deployments outside the scope is 403; deployments outside it are never
// touched, so a scoped caller cannot wake or load them. The replica is leased
// for adm. A request that asked to be refused when busy is, with
// MODEL_BUSY, when a deployment that would have served it has no free run
// slot for it and no other deployment of the name takes it.
func (p *Proxy) selectScopedDeploymentInstance(scope *store.APIKeyIdentity, modelName, affinity string, adm admission) (*store.Deployment, target, *refusal, bool) {
	allowed := func(id uint) bool { return scope == nil || scope.Allows(id) }
	if p.routes != nil {
		deployments, ok := p.routes.snapshot(modelName)
		if ok {
			var full *store.Deployment
			for _, rd := range deployments {
				if !allowed(rd.dep.ID) {
					continue
				}
				tgt, ok, busy := p.pickRouted(rd, modelName, affinity, p.hold.drainingOf(rd.dep.ID), adm)
				if ok {
					return rd.dep, tgt, nil, false
				}
				if busy && full == nil {
					full = rd.dep
				}
			}
			// The busy answer is the snapshot's: answering it reads nothing,
			// which is what a saturated gateway needs.
			if full != nil {
				return full, target{}, busyRefusal(modelName), false
			}
			// A dirty snapshot is refreshed asynchronously. The name-scoped store
			// lookup below is the bounded correctness fallback; never rebuild the
			// complete route table on the request path. It also settles a
			// name the snapshot shows only outside the caller's scope.
		}
		p.routes.requestRebuild()
	}
	served, err := p.store.DeploymentsByServedName(modelName)
	if err != nil || len(served) == 0 {
		return nil, target{}, refuse(ErrorModelNotFound, map[string]any{"model": modelName}), false
	}
	deployments := served[:0:0]
	for _, dep := range served {
		if allowed(dep.ID) {
			deployments = append(deployments, dep)
		}
	}
	if len(deployments) == 0 {
		return nil, target{}, refuse(ErrorModelAccessDenied, map[string]any{"model": modelName}), false
	}
	rejection := refuse(ErrorModelNotReady, map[string]any{"model": modelName})
	// attempted tracks which deployment the surfaced refusal (and asleep
	// flag) belongs to, for accounting even when every candidate failed: it
	// starts at the first candidate and is overridden whenever a later
	// candidate is specifically the asleep case, mirroring the message
	// selection below exactly.
	attempted := &deployments[0]
	asleep := false
	var full *store.Deployment
	for index := range deployments {
		dep := &deployments[index]
		tgt, candidate, candidateAsleep := p.selectDeploymentInstances(dep, modelName, affinity, adm)
		if candidate == nil {
			return dep, tgt, nil, false
		}
		// A deployment that would have served the request but for its
		// declaration answers for the name: busy is the truth to tell.
		if candidate.code == ErrorModelBusy && full == nil {
			full = dep
		}
		if index == 0 || candidateAsleep {
			rejection = candidate
			asleep = candidateAsleep
			attempted = dep
		}
	}
	if full != nil {
		return full, target{}, busyRefusal(modelName), false
	}
	return attempted, target{}, rejection, asleep
}

// pickRouted leases a replica of rd from the route snapshot; busy says none
// was leased because none had a free run slot for adm.
func (p *Proxy) pickRouted(rd *routeDeployment, modelName, affinity string, draining map[string]bool, adm admission) (tgt target, ok, busy bool) {
	if draining != nil && rd.nnodes > 1 {
		return target{}, false, false
	}
	if rd.nnodes > 1 {
		// Distributed readiness is read through once per request because
		// all ranks must belong to the same current attempt. The route
		// snapshot still supplies the node UUID map, avoiding one NodeByID
		// query for every rank.
		insts, _ := p.store.InstancesForDeployment(rd.id)
		tgt, rejection, _ := p.selectDistributedInstanceWithNodes(rd.dep, insts, modelName, affinity, rd.nodes, rd.drivers, adm)
		return tgt, rejection == nil, false
	}
	candidates := withoutDraining(rd.candidates, draining)
	if len(candidates) == 0 {
		return target{}, false, false
	}
	if !rd.rejectsBusy() {
		// Reported busy_reject:false by /v1/key, and so never refused for
		// being busy, whichever of its replicas are left to pick from.
		adm = adm.waiting()
	}
	lease, busy := p.pickAwake(candidates, rd.policy, affinity, false, adm)
	if lease == nil {
		// Busy is judged before a lease's awake check, so it is the
		// snapshot's word only while no instance change has passed it: a
		// full replica since ordered to sleep is asleep, not busy, and the
		// rows read for the request tell which.
		return target{}, false, busy && instanceRowsUnchanged(p.store, candidates[0].routedAt)
	}
	attempt := &routeAttempt{candidates: candidates, current: lease, tried: map[string]bool{lease.c.instanceUUID: true}, attempts: 1, started: time.Now(), modelName: lease.c.modelName, policy: rd.policy, affinity: affinity, deploymentID: rd.id, accountant: p.acct, admission: adm, prioritized: lease.c.prioritized}
	return target{nodeUUID: lease.c.nodeUUID, port: lease.c.port, modelName: lease.c.modelName, instanceUUID: lease.c.instanceUUID, lease: lease, candidates: candidates, attempt: attempt, pd: rd.pd, affinity: affinity}, true, false
}

func (p *Proxy) selectDeploymentInstances(dep *store.Deployment, modelName, affinity string, adm admission) (target, *refusal, bool) {
	// Draining replicas take no new request; with none other left, the
	// request is held as if the deployment slept (drain.go).
	draining := p.hold.drainingOf(dep.ID)
	if draining != nil && dep.NNodes > 1 {
		return p.refuseAsleep(dep, dep.Sleepable)
	}
	insts, _ := p.store.InstancesForDeployment(dep.ID)
	if dep.NNodes > 1 {
		return p.selectDistributedInstance(dep, insts, modelName, affinity, adm)
	}
	candidates := make([]candidate, 0, len(insts))
	anyAsleep := false
	// nodeOffline: an engine reports running and awake, and the only thing
	// between it and the request is its node's agent connection.
	nodeOffline := false
	pd, err := pdRouteFor(dep)
	if err != nil {
		p.log.Warn("PD deployment not routed: placement cannot be read", "dep", dep.ID, "err", err)
		return target{}, refuse(ErrorPDPlacementUnreadable, nil), false
	}
	// replicas and slotted count the running replicas and those whose launch
	// states run slots, as the route snapshot does (rejectsBusy).
	replicas, slotted := 0, 0
	for _, in := range insts {
		if in.State == proto.InstRunning && !in.Headless {
			replicas++
			if instanceRunSlots(dep, &in) > 0 {
				slotted++
			}
		}
		// An instance ordered to sleep, or still asleep, is "running" but cannot
		// serve: the offloaded engine would hang the request. Only when no awake
		// instance is left is the caller told to wake the model.
		if in.State == proto.InstRunning && planning.InstanceAsleep(&in) {
			anyAsleep = true
			if pd != nil {
				pd.noteAsleep(&in)
			}
			continue
		}
		if in.State == proto.InstRunning && !in.Headless && in.APIPort > 0 {
			node, err := p.store.NodeByID(in.NodeID)
			if err != nil {
				continue
			}
			if !p.hub.Online(node.UUID) {
				nodeOffline = true
				continue
			}
			name, slots := instanceLaunchFacts(dep, &in, planning.HostTranslatesModelPaths(node.RuntimeDriver))
			c := candidate{instanceID: in.ID, instanceUUID: in.UUID, nodeUUID: node.UUID, port: in.APIPort, modelName: name, uuidHash: fnv64([]byte(in.UUID)), slots: slots, prioritized: runsPriority(dep, &in)}
			if pd != nil {
				var ok bool
				if c, ok = pd.admit(&in, c); !ok {
					continue
				}
			}
			candidates = append(candidates, c)
		}
	}
	if pd != nil || slotted < replicas {
		// Never refused for being busy (rejectsBusy), whichever replicas
		// are left to pick from.
		adm = adm.waiting()
	}
	if pd != nil {
		nodes := map[uint]string{}
		for _, in := range insts {
			if node, err := p.store.NodeByID(in.NodeID); err == nil {
				nodes[in.NodeID] = node.UUID
			}
		}
		candidates = pd.withoutUnroutable(candidates, insts, nodes)
	}
	if routable := withoutDraining(candidates, draining); len(routable) < len(candidates) {
		if len(routable) == 0 {
			return p.refuseAsleep(dep, dep.Sleepable)
		}
		candidates = routable
	}
	if len(candidates) > 0 {
		policy := deploy.NormalizeRoutingPolicy(dep.RoutingPolicy)
		lease, busy := p.pickAwake(candidates, policy, affinity, true, adm)
		if lease != nil {
			attempt := &routeAttempt{candidates: candidates, current: lease, tried: map[string]bool{lease.c.instanceUUID: true}, attempts: 1, started: time.Now(), modelName: lease.c.modelName, policy: policy, affinity: affinity, deploymentID: dep.ID, accountant: p.acct, admission: adm, prioritized: lease.c.prioritized}
			return target{nodeUUID: lease.c.nodeUUID, port: lease.c.port, modelName: lease.c.modelName, instanceUUID: lease.c.instanceUUID, lease: lease, candidates: candidates, attempt: attempt, pd: pd, affinity: affinity}, nil, false
		}
		if busy {
			return target{}, busyRefusal(modelName), false
		}
	}
	// Distinguish "sleeping" from "not ready" so the caller is told to wake
	// the model rather than to retry.
	if anyAsleep {
		return p.refuseAsleep(dep, planning.RequestWakes(dep, insts))
	}
	if instancesGaveUp(insts) {
		return target{}, refuse(ErrorDeploymentStartFailed, map[string]any{"model": modelName}), false
	}
	// The engine is up: telling the caller it may still be starting would
	// send them to the deployment, which shows nothing wrong.
	if nodeOffline && len(candidates) == 0 {
		return target{}, refuse(ErrorModelNodeOffline, map[string]any{"model": modelName}), false
	}
	return target{}, refuse(ErrorModelNotReady, map[string]any{"model": modelName}), false
}

// instancesGaveUp reports that this deployment stopped retrying. A pending,
// pulling, or starting replacement is still live, so an old failed row does
// not by itself mean the launch is over. The same split is used when the API
// derives a deployment's runtime state.
func instancesGaveUp(insts []store.Instance) bool {
	if len(insts) == 0 {
		return false
	}
	hasFailed := false
	for _, in := range insts {
		switch in.State {
		case proto.InstFailed:
			hasFailed = true
		case proto.InstStopped, proto.InstPendingRemoval:
		default:
			return false
		}
	}
	return hasFailed
}

// selectDistributedInstance picks a multi-node replica to serve the request:
// a leader whose generation is complete (every follower of its group and
// attempt running, planning.GroupsIncomplete) and whose node is online, by
// the deployment's routing policy, as for single-node replicas. A deployment
// may list several multi-node replicas; each generation is its own group.
// The final bool return is asleep; see selectDeploymentInstance.
func (p *Proxy) selectDistributedInstance(dep *store.Deployment, insts []store.Instance, modelName, affinity string, adm admission) (target, *refusal, bool) {
	return p.selectDistributedInstanceWithNodes(dep, insts, modelName, affinity, nil, nil, adm)
}

// nodes and drivers are the route snapshot's node UUIDs and runtime drivers by
// node ID; nil reads each node from the store. A multi-node replica is never
// refused for being busy.
func (p *Proxy) selectDistributedInstanceWithNodes(dep *store.Deployment, insts []store.Instance, modelName, affinity string, nodes, drivers map[uint]string, adm admission) (target, *refusal, bool) {
	adm = adm.waiting()
	nodeUUID := func(id uint) string {
		if nodes != nil {
			return nodes[id]
		}
		if node, err := p.store.NodeByID(id); err == nil {
			return node.UUID
		}
		return ""
	}
	nodeDriver := func(id uint) string {
		if nodes != nil {
			return drivers[id]
		}
		if node, err := p.store.NodeByID(id); err == nil {
			return node.RuntimeDriver
		}
		return ""
	}
	online := func(id uint) bool {
		uuid := nodeUUID(id)
		return uuid != "" && p.hub.Online(uuid)
	}
	incomplete := planning.GroupsIncomplete(insts)
	var followers []store.Instance
	for _, in := range insts {
		if in.Headless && in.State == proto.InstRunning {
			followers = append(followers, in)
		}
	}
	candidates := make([]candidate, 0, len(insts))
	anySleeping := false
	for i := range insts {
		in := &insts[i]
		if in.State != proto.InstRunning || in.Headless {
			continue
		}
		// A generation serves as a whole: every follower of it must be on
		// an online node. It sleeps with its leader's engine, so the leader
		// alone says whether it is asleep. Rows from before rank metadata
		// was recorded carry no group; such a leader takes every running
		// follower of the deployment as its own.
		legacy := in.NNodes <= 1 && in.DistGroup == ""
		ranks, asleep, reachable := 0, planning.InstanceAsleep(in), online(in.NodeID)
		for j := range followers {
			if legacy || planning.SameGeneration(followers[j], *in) {
				ranks++
				reachable = reachable && online(followers[j].NodeID)
			}
		}
		if asleep {
			anySleeping = true
			continue
		}
		if in.APIPort <= 0 || incomplete[in.UUID] || (legacy && ranks < dep.NNodes-1) || !reachable {
			continue
		}
		name, slots := instanceLaunchFacts(dep, in, planning.HostTranslatesModelPaths(nodeDriver(in.NodeID)))
		candidates = append(candidates, candidate{instanceID: in.ID, instanceUUID: in.UUID, nodeUUID: nodeUUID(in.NodeID), port: in.APIPort, modelName: name, uuidHash: fnv64([]byte(in.UUID)), slots: slots, prioritized: runsPriority(dep, in)})
	}
	if len(candidates) == 0 {
		if anySleeping {
			return p.refuseAsleep(dep, planning.RequestWakes(dep, insts))
		}
		return target{}, refuse(ErrorDistributedNotReady, map[string]any{"model": modelName}), false
	}
	policy := deploy.NormalizeRoutingPolicy(dep.RoutingPolicy)
	lease, _ := p.pickAwake(candidates, policy, affinity, true, adm)
	if lease == nil {
		return target{}, refuse(ErrorDistributedLeaderOffline, map[string]any{"model": modelName}), false
	}
	attempt := &routeAttempt{candidates: candidates, current: lease, tried: map[string]bool{lease.c.instanceUUID: true}, attempts: 1, started: time.Now(), modelName: lease.c.modelName, policy: policy, affinity: affinity, deploymentID: dep.ID, accountant: p.acct, admission: adm, prioritized: lease.c.prioritized}
	return target{nodeUUID: lease.c.nodeUUID, port: lease.c.port, modelName: lease.c.modelName, instanceUUID: lease.c.instanceUUID, lease: lease, candidates: candidates, attempt: attempt, affinity: affinity}, nil, false
}

// refuseAsleep is the refusal of a request for dep because its replicas
// sleep (or drain to sleep), and records for holdForWake whether a request
// wakes it (wakes: a sleeper of it wakes on request, planning.RequestWakes,
// or its replicas drain for a yield). wakes is worked out from the instance
// rows the refusal was read from, so the request path reads nothing more.
func (p *Proxy) refuseAsleep(dep *store.Deployment, wakes bool) (target, *refusal, bool) {
	if p.hold != nil {
		p.hold.noteAsleep(dep.ID, wakes)
	}
	return target{}, asleepRefusal(dep), true
}

// asleepRefusal tells a client of a sleeping model what to do: wake it from
// the console or the API, or wait for its schedule or a waiting wake.
func asleepRefusal(dep *store.Deployment) *refusal {
	return refuse(ErrorModelAsleep, map[string]any{"deployment_id": dep.ID})
}

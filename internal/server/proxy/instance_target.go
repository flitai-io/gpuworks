package proxy

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// instanceHeader pins a request to one instance of the deployment it names.
// It is honoured only on the authenticated internal entry (the playground),
// never on /v1: a client holding an API key must not bypass load balancing or
// a PD deployment's two-stage handoff (plan D9). On a PD deployment a pinned
// request goes to that instance alone, which is not an end-to-end result.
const instanceHeader = "X-GPUWorks-Instance"

// pinnedTarget resolves the pinned instance of a deployment serving model.
// ok is false when the request pins nothing; rejection is non-nil when it pins
// an instance that cannot take it, and asleep reports that the refusal was
// the sleep the deployment is under, so the caller can account it the way the
// load balancing path accounts the same refusal rather than as a plain 5xx.
func (p *Proxy) pinnedTarget(r *http.Request, modelName string) (dep *store.Deployment, tgt target, ok bool, rejection *refusal, asleep bool) {
	uuid := strings.TrimSpace(r.Header.Get(instanceHeader))
	r.Header.Del(instanceHeader)
	if uuid == "" {
		return nil, target{}, false, nil, false
	}
	inst, err := p.store.InstanceByUUID(uuid)
	if err != nil {
		return nil, target{}, true, refuse(ErrorInstanceNotFound, map[string]any{"instance": uuid}), false
	}
	dep, err = p.store.GetDeployment(inst.DeploymentID)
	if err != nil || dep.ServedName != modelName {
		return nil, target{}, true, refuse(ErrorInstanceModelMismatch, map[string]any{"instance": uuid, "model": modelName}), false
	}
	// Sleep is judged by planning.InstanceAsleep, the predicate the load
	// balancing path and the deployment row use. Reading the reported level
	// alone let a pinned request through to an engine under a sleep order its
	// engine had not carried out yet — the window this feature exists for — so
	// the request was forwarded to an engine on its way to sleep, hung or
	// errored, and was accounted as awake while every other read of the same
	// deployment called it asleep.
	//
	// asleep repeats the load balancing path's rule exactly: only a running
	// instance refused for its sleep order is accounted asleep, so a replica
	// that is stopped or failed stays a plain 5xx even when it also carries a
	// stale sleep order. Getting that wrong would file stopped replicas under
	// the asleep counter and hide a real outage.
	asleep = inst.State == proto.InstRunning && planning.InstanceAsleep(inst)
	if asleep {
		return dep, target{}, true, refuse(ErrorInstanceAsleep, map[string]any{"instance": uuid}), true
	}
	if inst.State != proto.InstRunning || inst.Headless || inst.APIPort <= 0 {
		return dep, target{}, true, refuse(ErrorInstanceNotRunning, map[string]any{"instance": uuid}), false
	}
	node, err := p.store.NodeByID(inst.NodeID)
	if err != nil || !p.hub.Online(node.UUID) {
		return dep, target{}, true, refuse(ErrorInstanceOffline, map[string]any{"instance": uuid}), false
	}
	return dep, target{nodeUUID: node.UUID, port: inst.APIPort, modelName: instanceModelName(dep, inst), instanceUUID: inst.UUID}, true, nil, false
}

package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelsReady reports whether there is no download left to wait for.
//
// `present` and `registered` are the two ready states, exactly as the download
// reconciliation treats them (store.deploy: "current.State == MFPresent ||
// current.State == MFRegistered"). `registered` is what a locally registered
// path carries, and a deployment of one is ready the moment it is created.
//
// An empty list is ready too: with no model files recorded there is nothing
// outstanding, so a scheduling refusal is the whole story.
func modelsReady(modelStates []string) bool {
	for _, state := range modelStates {
		switch state {
		case proto.MFPresent, proto.MFRegistered:
		default:
			return false
		}
	}
	return true
}

// DeriveRuntimeState is the state a deployment reads as, from what it was
// ordered to be (desired), its instances, the states of the model files it
// waits for and whether the scheduler has refused it past the pending window.
// It reads nothing: the deployment list, the monitor and the batches derive
// the same state from the same facts.
func DeriveRuntimeState(desired store.DeploymentState, instances []store.Instance, modelStates []string, scheduleStalled bool) string {
	if desired == store.DeployStopped || desired == store.DeployDeleting {
		return "stopped"
	}
	if len(instances) == 0 {
		// No instance can exist yet until every required model file is
		// present on the target node. Failed downloads, insufficient disk,
		// and required downloads need operator action before they can proceed
		// (free disk space, start a public download or retry peer sync,
		// internal/server/controller/modelsync.go) — so without this check a
		// permanently-blocked deployment looks identical to one that is
		// merely queued and about to schedule normally.
		for _, state := range modelStates {
			if state == proto.MFFailed || state == proto.MFDownloadRequired || state == proto.MFInsufficientDisk {
				return "failed"
			}
		}
		// The scheduler has explicitly refused a placement and kept refusing for
		// longer than the configured window. That is not "queued and about to
		// schedule", and reporting it as pending is what left deployments
		// sitting forever with no visible reason.
		//
		// Only while the weights are actually present, though. Scheduling is
		// not even attempted until every model file is in place
		// (controller.ensureDeploymentModelsPresent returns early otherwise), so
		// a refusal recorded earlier says nothing about a deployment that is
		// downloading right now.
		if scheduleStalled && modelsReady(modelStates) {
			return "failed"
		}
		return "pending"
	}
	// Failed/stopped rows are attempt history once a replacement instance is
	// active. Retries deliberately use a fresh instance UUID and keep terminal
	// rows for diagnostics, so an old failure must not hide a currently pulling,
	// starting, or running replacement.
	//
	// A headless rank is neither, whatever its state: it exposes no endpoint,
	// and the gateway routes to a multi-node replica through its leader only,
	// so a follower that outlives its failed leader keeps nothing serving. A
	// running leader whose generation lost a follower to failure has failed
	// with it: the generation serves as a whole.
	incomplete := GroupsIncomplete(instances)
	hasLive, hasFailed := false, false
	for _, instance := range instances {
		switch {
		case instance.State == proto.InstFailed:
			hasFailed = true
		case instance.Headless:
		case instance.State == proto.InstStopped, instance.State == proto.InstPendingRemoval:
		case instance.State == proto.InstRunning && !InstanceAsleep(&instance) && incomplete[instance.UUID] && generationFailed(instances, instance):
			hasFailed = true
		default:
			hasLive = true
		}
	}
	if !hasLive && hasFailed {
		return "failed"
	}
	// A running replica keeps the runtime state running while a replacement
	// warms up. Sleeping leaders retain that runtime state too: the gateway
	// refuses them for sleep before checking their followers, and
	// deployment.EffectiveState presents that refusal as sleeping/asleep.
	if runningRoles(instances, incomplete) {
		return "running"
	}
	for _, instance := range instances {
		if instance.State == proto.InstPulling {
			return "pulling"
		}
	}
	for _, instance := range instances {
		if instance.State == proto.InstPending || instance.State == proto.InstStarting || instance.State == proto.InstOptimizing {
			return "starting"
		}
	}
	for _, instance := range instances {
		// A draining or removing instance is leaving while the rest serve; it
		// does not make a running deployment read as starting. A replica
		// moved off an offline node stays removing until the node returns.
		if instance.State != proto.InstRunning && instance.State != proto.InstDraining && instance.State != proto.InstRemoving && instance.State != proto.InstFailed && instance.State != proto.InstStopped && instance.State != proto.InstPendingRemoval {
			return "starting"
		}
	}
	for _, instance := range instances {
		// A leader missing a running follower is not serving: its
		// generation is coming up or going away.
		if instance.Headless || incomplete[instance.UUID] {
			continue
		}
		if instance.State == proto.InstRunning || instance.State == proto.InstDraining {
			return "running"
		}
	}
	return "starting"
}

// runningRoles reports whether every role has a running leader whose followers
// all run, or a sleeping leader. The gateway checks sleep before completeness:
// a sleeping leader keeps its running runtime state so EffectiveState can show
// sleeping/asleep, even while a follower is down. This does not count it ready.
// An ordinary deployment has one role, the empty one; a PD deployment with a
// role still warming does not serve yet.
func runningRoles(instances []store.Instance, incomplete map[string]bool) bool {
	roles := map[string]bool{}
	for _, instance := range instances {
		if instance.Headless || !InstanceActive(instance.State) {
			continue
		}
		running := instance.State == proto.InstRunning && (InstanceAsleep(&instance) || !incomplete[instance.UUID])
		roles[instance.Role] = roles[instance.Role] || running
	}
	for _, running := range roles {
		if !running {
			return false
		}
	}
	return len(roles) > 0
}

// generationFailed reports whether a follower of the leader's generation
// failed, which leaves the generation unable to serve.
func generationFailed(instances []store.Instance, leader store.Instance) bool {
	for _, instance := range instances {
		if instance.Headless && instance.State == proto.InstFailed && SameGeneration(instance, leader) {
			return true
		}
	}
	return false
}

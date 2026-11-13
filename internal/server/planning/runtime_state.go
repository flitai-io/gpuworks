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
	hasLive, hasFailed := false, false
	for _, instance := range instances {
		switch instance.State {
		case proto.InstFailed:
			hasFailed = true
		case proto.InstStopped, proto.InstPendingRemoval:
		default:
			hasLive = true
		}
	}
	if !hasLive && hasFailed {
		return "failed"
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
		if instance.State == proto.InstRunning || instance.State == proto.InstDraining {
			return "running"
		}
	}
	return "starting"
}

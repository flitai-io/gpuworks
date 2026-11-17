package engine

import (
	"strconv"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Container labels naming who launched a workload. Several agents with
// different state directories, each enrolled by a different server, may share
// one host's docker daemon or nerdctl namespace; these labels are how an
// operator tells their containers apart (`docker ps --filter
// label=gpuworks.server=https://...`, `docker inspect`). They are for
// people: nothing in the agent selects containers by them.
const (
	LabelServer     = "gpuworks.server"     // control-plane address the agent dials
	LabelNode       = "gpuworks.node"       // node UUID the server issued
	LabelStateDir   = "gpuworks.state-dir"  // the launching agent's state directory
	LabelInstance   = "gpuworks.instance"   // instance UUID (instance containers only)
	LabelDeployment = "gpuworks.deployment" // deployment ID (instance containers only)
)

// WorkloadOwner identifies the agent launching this engine's containers.
// Unknown fields stay empty and their labels are omitted: a server_dial agent
// never dials the server, so it has no server address to record.
type WorkloadOwner struct {
	ServerURL string
	NodeUUID  string
	StateDir  string
}

// SetWorkloadOwner records the owner labelled on every container launched
// from now on. The node UUID arrives at registration, after the engine is
// built, so it is set rather than passed at construction.
func (e *Engine) SetWorkloadOwner(owner WorkloadOwner) {
	e.owner.Store(&owner)
}

func (e *Engine) ownerLabels() map[string]string {
	labels := map[string]string{}
	if o := e.owner.Load(); o != nil {
		for k, v := range map[string]string{LabelServer: o.ServerURL, LabelNode: o.NodeUUID, LabelStateDir: o.StateDir} {
			if v != "" {
				labels[k] = v
			}
		}
	}
	return labels
}

// workloadSpec is toWorkloadSpec plus the owner labels; every container the
// engine launches is built through it.
func (e *Engine) workloadSpec(spec proto.InstanceDesiredState, name string) runtime.WorkloadSpec {
	ws := toWorkloadSpec(spec, name)
	ws.Labels = e.ownerLabels()
	return ws
}

// instanceWorkloadSpec is workloadSpec for an instance's own container, which
// also names the instance and its deployment.
func (e *Engine) instanceWorkloadSpec(spec proto.InstanceDesiredState, name string) runtime.WorkloadSpec {
	ws := e.workloadSpec(spec, name)
	ws.Labels[LabelInstance] = spec.UUID
	if spec.DeploymentID != 0 {
		ws.Labels[LabelDeployment] = strconv.FormatUint(uint64(spec.DeploymentID), 10)
	}
	return ws
}

// workloadOwnerSlot holds the owner SetWorkloadOwner last recorded.
type workloadOwnerSlot struct{ atomic.Pointer[WorkloadOwner] }

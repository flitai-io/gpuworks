package engine

import (
	"context"
	"time"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// BuildCardSmokeWorkload is the container the per-card runtime check runs in
// on a host whose runtime lives only in images: image (a runner image already
// on the node), the one card at gpuIndex injected exactly as a real instance
// gets it, and entrypoint as its command. Nothing else of an instance's is
// added — no model, no ports, no partition.
func (e *Engine) BuildCardSmokeWorkload(image string, gpuIndex int, name string, entrypoint []string) (agentruntime.WorkloadSpec, error) {
	ws := e.workloadSpec(proto.InstanceDesiredState{UUID: name, Image: image, GPUIndexes: []int{gpuIndex}}, name)
	if err := e.applyDeviceInjection(&ws, ws.GPUIndexes); err != nil {
		return agentruntime.WorkloadSpec{}, err
	}
	ws.Entrypoint = entrypoint
	ws.Cmd = nil
	return ws, nil
}

// DesiredImages are the images of the instances this agent is told to run.
func (e *Engine) DesiredImages() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	images := make([]string, 0, len(e.desired))
	for _, spec := range e.desired {
		if spec.Image != "" {
			images = append(images, spec.Image)
		}
	}
	return images
}

// cardGateLimit bounds how long a start waits for a runtime check on its
// cards. A check takes well under a second on a healthy card (a few on an
// Ascend die); one stuck in the driver is not waited out, the start goes on
// and the driver answers for itself.
const cardGateLimit = 10 * time.Second

// SetCardGate installs what a start waits on before it launches the container:
// the runtime check's WaitIdle. On a card only one process may open (an Ascend
// die without device-share) a check holding it would fail the engine's start.
func (e *Engine) SetCardGate(wait func(ctx context.Context, idents []string, limit time.Duration) bool) {
	e.mu.Lock()
	e.cardGate = wait
	e.mu.Unlock()
}

// waitForCardChecks waits for any runtime check on the instance's cards.
func (e *Engine) waitForCardChecks(ctx context.Context, uuid string, idents []string) {
	e.mu.Lock()
	wait := e.cardGate
	e.mu.Unlock()
	if wait == nil || len(idents) == 0 {
		return
	}
	start := time.Now()
	if !wait(ctx, idents, cardGateLimit) && ctx.Err() == nil {
		e.log.Warn("a runtime check is still on the instance's cards; starting anyway", "instance", uuid, "waited", time.Since(start).Round(time.Millisecond))
	} else if waited := time.Since(start); waited > 100*time.Millisecond {
		e.log.Info("waited for a runtime check on the instance's cards", "instance", uuid, "waited", waited.Round(time.Millisecond))
	}
}

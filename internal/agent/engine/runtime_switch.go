package engine

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// SwitchDriver stops GPUWorks-managed containers, swaps the runtime, and
// reconciles the existing desired state. Runtime changes are intentionally
// disruptive: keeping containers created by one runtime alive while handing
// control to another is neither portable nor safe.
func (e *Engine) SwitchDriver(ctx context.Context, driver runtime.Driver) error {
	if driver == nil {
		return fmt.Errorf("runtime driver is nil")
	}
	e.mu.Lock()
	old := e.driver
	if old != nil && strings.EqualFold(old.Name(), driver.Name()) {
		e.mu.Unlock()
		return nil
	}
	specs := make([]proto.InstanceDesiredState, 0, len(e.desired))
	for _, spec := range e.desired {
		specs = append(specs, spec)
	}
	if len(e.inFlight) > 0 {
		e.mu.Unlock()
		return fmt.Errorf("runtime switch is busy while instances are starting")
	}
	if len(e.cleanupRequired) > 0 {
		e.mu.Unlock()
		return fmt.Errorf("runtime switch is busy while instance cleanup is pending")
	}
	if old != nil {
		for _, spec := range specs {
			name := containerName(spec.UUID)
			status, err := old.Status(ctx, name)
			if err != nil {
				e.mu.Unlock()
				return fmt.Errorf("runtime switch: inspect old container %s: %w", name, err)
			}
			if !status.Exists {
				continue
			}
			e.log.Info("runtime switch: stopping instance container", "uuid", spec.UUID, "container", name)
			if status.Running {
				if err := old.Stop(ctx, name); err != nil {
					e.mu.Unlock()
					return fmt.Errorf("runtime switch: stop old container %s: %w", name, err)
				}
			}
			if err := old.Remove(ctx, name); err != nil {
				e.mu.Unlock()
				return fmt.Errorf("runtime switch: remove old container %s: %w", name, err)
			}
			status, err = old.Status(ctx, name)
			if err != nil {
				e.mu.Unlock()
				return fmt.Errorf("runtime switch: verify old container %s removal: %w", name, err)
			}
			if status.Exists {
				e.mu.Unlock()
				return fmt.Errorf("runtime switch: old container %s still exists after removal", name)
			}
		}
	}
	e.driver = driver
	e.runtimeMounts = newRuntimeMountManager(e.stateDir, driver)
	// Every engine process went with the old runtime's containers.
	e.processes = map[string]*processFacts{}
	// The old runtime's containers are gone and the new runtime has none yet:
	// nothing serves until reconcileAll adopts or starts each instance again.
	e.servingEpoch = map[string]uint64{}
	e.mu.Unlock()
	e.reconcileAll(ctx)
	return nil
}

// Idle reports whether the control plane has said what this node runs and
// the answer is nothing: no instance desired other than stopped, none
// starting or being torn down, no cleanup pending. A runtime switch then
// recreates nothing.
func (e *Engine) Idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.desiredKnown || len(e.inFlight) > 0 || len(e.cleanupRequired) > 0 {
		return false
	}
	for _, spec := range e.desired {
		if spec.DesiredState != "stopped" {
			return false
		}
	}
	return true
}

package engine

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// No container has been created when injection fails. Never manufacture a
// running state or direct address to report an accelerator requirement.
func (e *Engine) reportInjectionFailure(spec proto.InstanceDesiredState, err error) {
	status := proto.InstanceStatus{UUID: spec.UUID, State: proto.InstFailed, Message: err.Error()}
	var injection *gpu.InjectionError
	if errors.As(err, &injection) {
		failure := injection.Failure
		status.RuntimeFailure = &failure
	}
	e.report(status)
}

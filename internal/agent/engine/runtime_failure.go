package engine

import (
	"encoding/json"
	"errors"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// failureError is an error that carries the structured reason of the failure
// it causes (ADR 0067): the checks that know what went wrong say so in a
// code where they find it, and the report reads it back with errors.As
// instead of parsing the text. The text stays the English fallback.
type failureError struct {
	failure proto.RuntimeFailure
	err     error
}

func (e *failureError) Error() string { return e.err.Error() }
func (e *failureError) Unwrap() error { return e.err }

// withFailure attaches failure to err.
func withFailure(failure proto.RuntimeFailure, err error) error {
	return &failureError{failure: failure, err: err}
}

// failureOf is the reason err carries (a failureError or a refused
// accelerator injection), else code with err's text as its detail. phase
// fills a reason that does not name its own.
func failureOf(err error, code, phase string) proto.RuntimeFailure {
	var failure proto.RuntimeFailure
	var carried *failureError
	var injection *gpu.InjectionError
	switch {
	case errors.As(err, &carried):
		failure = carried.failure
	case errors.As(err, &injection):
		failure = injection.Failure
	default:
		failure = proto.RuntimeFailure{Code: code, Detail: err.Error()}
	}
	if failure.Phase == "" {
		failure.Phase = phase
	}
	return failure
}

// failedStatus is what a failed report carries besides its reason.
type failedStatus struct {
	ContainerID string
	// Phase is the report's own phase: PhaseSleep for a failed sleep order,
	// which the server reads to keep the generation's GPU claim.
	Phase    string
	Measured json.RawMessage
}

// reportFailed is the only place the agent reports an instance failed: a
// failed report cannot leave without its structured reason. message is the
// English fallback old servers and clients read.
func (e *Engine) reportFailed(uuid string, failure proto.RuntimeFailure, message string, extra failedStatus) {
	reason := failure.Bounded()
	e.report(proto.InstanceStatus{
		UUID: uuid, State: proto.InstFailed, Message: message, RuntimeFailure: &reason,
		ContainerID: extra.ContainerID, Phase: extra.Phase, Measured: extra.Measured,
	})
}

// reportCleanupPending reports a failure whose container or partition state
// could not be reclaimed yet: the instance is removing, not failed, so the
// controller does not rebuild it on top of what it still holds. The reason is
// the failure's own; the message also says what is pending.
func (e *Engine) reportCleanupPending(uuid string, failure proto.RuntimeFailure, message string, measured json.RawMessage) {
	reason := failure.Bounded()
	e.report(proto.InstanceStatus{UUID: uuid, State: proto.InstRemoving, Message: message, RuntimeFailure: &reason, Measured: measured})
}

// failLaunch reports a launch that failed before a container existed.
func (e *Engine) failLaunch(spec proto.InstanceDesiredState, err error, code, phase, message string) {
	e.reportFailed(spec.UUID, failureOf(err, code, phase), message, failedStatus{})
}

// reportInjectionFailure reports a refused accelerator admission or
// injection. No container has been created when it fails, so it never
// manufactures a running state or a direct address.
func (e *Engine) reportInjectionFailure(spec proto.InstanceDesiredState, err error) {
	e.failLaunch(spec, err, proto.ErrorAcceleratorInjectionUnavailable, proto.PhaseContainerPrepare, err.Error())
}

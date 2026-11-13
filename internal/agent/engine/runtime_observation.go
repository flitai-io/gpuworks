package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
)

// WithRuntimeObservationReporter binds runtime facts to the ctrl connection
// that was current before inspection began. capture returns zero while offline;
// report accepts only the same still-current epoch and must call the sender
// captured for that epoch rather than looking up a later connection.
func WithRuntimeObservationReporter(capture func() uint64, report func(uint64, proto.InstanceStatus) bool) EngineOption {
	return func(engine *Engine) {
		engine.captureObservationConnection = capture
		engine.reportRuntimeObservation = report
	}
}

func (e *Engine) collectAndPublishRuntimeObservation(ctx context.Context, spec proto.InstanceDesiredState, lookupID, expectedID string, epoch uint64) bool {
	driver := e.Driver()
	if _, ok := driver.(runtime.WorkloadInspector); !ok {
		return false
	}
	connectionEpoch := uint64(1)
	if e.captureObservationConnection != nil {
		connectionEpoch = e.captureObservationConnection()
		if connectionEpoch == 0 {
			return false
		}
	}
	observation := e.collectRuntimeObservation(ctx, driver, lookupID, expectedID)
	if ctx.Err() != nil {
		return false
	}
	e.mu.Lock()
	current, desired := e.desired[spec.UUID]
	valid := e.runtimeEpoch[spec.UUID] == epoch && desired && current.DesiredState != "stopped"
	e.mu.Unlock()
	if !valid {
		return false
	}
	// A reporter can reach a blocked ctrl connection, so it must not run under
	// Engine.mu. An inspect that finishes after removal invalidation is rejected
	// above; the observation-specific NodeRuntime path sends once on the current
	// connection and never retains it for a later session.
	status := proto.InstanceStatus{UUID: spec.UUID, RuntimeObservation: &observation}
	if e.reportRuntimeObservation != nil {
		return e.reportRuntimeObservation(connectionEpoch, status)
	}
	e.report(status)
	return true
}

func (e *Engine) collectRuntimeObservation(ctx context.Context, driver runtime.Driver, lookupID, expectedID string) proto.InstanceRuntimeObservation {
	observation := proto.InstanceRuntimeObservation{
		Runtime: driver.Name(),
		Container: proto.RuntimeContainerObservation{
			Status: proto.ObservationUnknown, Reason: proto.ObservationReasonUnsupported,
			Entrypoint: []string{}, Cmd: []string{}, Args: []string{}, Env: map[string]string{},
			Mounts: []proto.RuntimeMountObservation{}, Devices: []string{},
		},
		Image: proto.RuntimeImageObservation{
			Status: proto.ObservationUnknown, Reason: proto.ObservationReasonNotCollected,
			Entrypoint: []string{}, Cmd: []string{}, Env: map[string]string{}, Volumes: []string{},
		},
		Process: proto.RuntimeProcessObservation{Status: proto.ObservationUnknown, Reason: proto.ObservationReasonNotCollected},
	}
	inspector, ok := driver.(runtime.WorkloadInspector)
	if !ok {
		return observation
	}
	workload, err := inspector.InspectWorkload(ctx, lookupID)
	if err != nil {
		observation.Container.Reason = observationReason(err)
		return observation
	}
	if expectedID != "" && !handleIsName(driver) && !sameContainerIdentity(expectedID, workload.ContainerID) {
		observation.Container.Reason = proto.ObservationReasonInspectFailed
		return observation
	}
	observation.Container = proto.RuntimeContainerObservation{
		Status: proto.ObservationObserved, ObservedAtUnixMS: time.Now().UnixMilli(),
		ContainerID: workload.ContainerID, ImageID: workload.ImageID,
		Entrypoint: nonNilRedactedArgv(workload.Entrypoint), Cmd: nonNilRedactedArgv(workload.Cmd),
		Path: commandlog.Redact(workload.Path), Args: nonNilRedactedArgv(workload.Args), Env: nonNilRedactedEnv(workload.Env),
		Mounts:  make([]proto.RuntimeMountObservation, 0, len(workload.Mounts)),
		Devices: nonNilRedactedArgv(workload.Devices), RuntimeName: workload.RuntimeName, NetworkMode: workload.NetworkMode,
	}
	for _, mount := range workload.Mounts {
		observation.Container.Mounts = append(observation.Container.Mounts, proto.RuntimeMountObservation{
			Type: mount.Type, Source: commandlog.Redact(mount.Source), Destination: commandlog.Redact(mount.Destination), ReadOnly: mount.ReadOnly,
		})
	}
	imageInspector, ok := driver.(runtime.ImageInspector)
	if !ok {
		observation.Image.Reason = proto.ObservationReasonUnsupported
		return observation
	}
	imageRef := workload.ImageLookupRef
	if imageRef == "" {
		imageRef = workload.ImageID
	}
	if imageRef == "" {
		observation.Image.Reason = proto.ObservationReasonInspectFailed
		return observation
	}
	image, err := imageInspector.InspectImage(ctx, imageRef)
	if err != nil {
		observation.Image.Reason = observationReason(err)
		return observation
	}
	if image.ID == "" || (workload.ImageID != "" && !sameImageIdentity(workload.ImageID, image.ID)) {
		observation.Image.Reason = proto.ObservationReasonInspectFailed
		return observation
	}
	if workload.ImageID == "" {
		observation.Container.ImageID = image.ID
	}
	observation.Image = proto.RuntimeImageObservation{
		Status: proto.ObservationObserved, ObservedAtUnixMS: time.Now().UnixMilli(), ImageID: image.ID,
		Entrypoint: nonNilRedactedArgv(image.Entrypoint), Cmd: nonNilRedactedArgv(image.Cmd),
		Env: nonNilRedactedEnv(image.Env), Volumes: nonNilRedactedArgv(image.Volumes),
	}
	return observation
}

func observationReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return proto.ObservationReasonTimeout
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "permission denied") || strings.Contains(message, "operation not permitted"):
		return proto.ObservationReasonPermission
	case strings.Contains(message, "not found") || strings.Contains(message, "no such container"):
		return proto.ObservationReasonNotRunning
	default:
		return proto.ObservationReasonInspectFailed
	}
}

// handleIsName is true for a driver whose Run returns a name, not the
// container ID the inspection reports (the identity check cannot apply).
func handleIsName(driver runtime.Driver) bool {
	named, ok := driver.(runtime.NamedWorkloads)
	return ok && named.WorkloadHandleIsName()
}

func sameContainerIdentity(expected, actual string) bool {
	expected, actual = strings.TrimSpace(expected), strings.TrimSpace(actual)
	if expected == actual && expected != "" {
		return true
	}
	if !isHexContainerID(expected) || !isHexContainerID(actual) {
		return false
	}
	shorter, longer := expected, actual
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 12 && strings.HasPrefix(longer, shorter)
}

func isHexContainerID(value string) bool {
	if len(value) < 12 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func sameImageIdentity(expected, actual string) bool {
	return strings.TrimSpace(expected) != "" && strings.TrimSpace(expected) == strings.TrimSpace(actual)
}

func nonNilRedactedArgv(values []string) []string {
	redacted := commandlog.RedactArgv(values)
	if redacted == nil {
		return []string{}
	}
	return redacted
}

func nonNilRedactedEnv(values map[string]string) map[string]string {
	redacted := commandlog.RedactEnv(values)
	if redacted == nil {
		return map[string]string{}
	}
	return redacted
}

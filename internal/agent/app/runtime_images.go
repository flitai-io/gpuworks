package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/imagearchive"
)

// runtimeImageRPC shares the node's image slot and cancellation registry with
// archive loads. The handlers retain only the dependencies used by image work.
type runtimeImageRPC struct {
	driver   runtime.Driver
	lock     *sync.Mutex
	ops      *imageOperations
	stateDir string
	report   func(proto.ImageNodeStatus)
}

var runtimeImageHandlers = map[string]func(runtimeImageRPC, context.Context, string, json.RawMessage) (any, error){
	proto.RPCImageRuntimeList:    runtimeImageRPC.selection,
	proto.RPCImageRuntimeInspect: runtimeImageRPC.selection,
	proto.RPCImageRuntimeImport:  runtimeImageRPC.selection,
	proto.RPCImageRuntimePrepare: runtimeImageRPC.selection,
	proto.RPCImageRuntimeCopy:    runtimeImageRPC.startCopy,
}

func (s runtimeImageRPC) selection(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var selection proto.RuntimeImageSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}
	d, ok := s.driver.(runtime.LocalImageStore)
	if !ok {
		return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if method == proto.RPCImageRuntimeList {
		result, err := d.ListLocalImages(ctx, selection.Namespace)
		if err != nil {
			return nil, localImageError(method, selection, err)
		}
		return result, nil
	}
	if !runtime.ValidLocalImageRef(selection.Ref) || !runtime.ValidImageDigest(selection.Identity) {
		return nil, localImageError(method, selection, runtime.ErrLocalImageChanged)
	}
	if method == proto.RPCImageRuntimeImport {
		// This RPC confirms presence immediately. Only the tracked worker may copy layers.
		if selection.Namespace != selection.TargetNamespace {
			return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
		}
		if !s.lock.TryLock() {
			return nil, &proto.RPCError{Code: proto.ErrorLocalImageBusy, Message: "another image operation is in progress; retry when it finishes"}
		}
		defer s.lock.Unlock()
	}
	image, err := d.ResolveLocalImage(ctx, selection, nil)
	if err != nil {
		return nil, localImageError(method, selection, err)
	}
	meta := imagearchive.MetadataFromConfig(image.Architecture, image.Labels)
	copyRequired := false
	if method == proto.RPCImageRuntimePrepare {
		target, ok := s.driver.(runtime.LocalImageCopyTarget)
		if !ok {
			return nil, localImageError(method, selection, runtime.ErrLocalImageUnsupported)
		}
		copyRequired, err = target.NeedsLocalImageCopy(ctx, selection.Ref, image.ID, s.stateDir)
		if err != nil {
			return nil, localImageError(method, selection, err)
		}
	}
	return proto.RuntimeImageDetails{CopyRequired: copyRequired, ImageID: image.ID, RuntimeSizeBytes: image.RuntimeSizeBytes, Metadata: proto.ArchiveImageMetadata(meta)}, nil
}

func localImageError(method string, selection proto.RuntimeImageSelection, err error) *proto.RPCError {
	switch {
	case errors.Is(err, runtime.ErrLocalImageBusy):
		return &proto.RPCError{Code: proto.ErrorLocalImageBusy, Message: runtime.ErrLocalImageBusy.Error()}
	case errors.Is(err, runtime.ErrLocalImageChanged), errors.Is(err, runtime.ErrImageNotFound):
		return &proto.RPCError{Code: proto.ErrorLocalImageChanged, Message: "the selected image changed or disappeared; refresh the list"}
	case errors.Is(err, runtime.ErrLocalImageConflict):
		return &proto.RPCError{Code: proto.ErrorLocalImageConflict, Message: runtime.ErrLocalImageConflict.Error()}
	case errors.Is(err, runtime.ErrLocalImageUnsupported):
		return &proto.RPCError{Code: proto.ErrorLocalImageUnsupported, Message: runtime.ErrLocalImageUnsupported.Error()}
	default:
		cause := err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			cause += ": " + string(exit.Stderr)
		}
		cause = strings.TrimSpace(commandlog.Redact(cause))
		const maxDiagnostic = 1024
		if len(cause) > maxDiagnostic {
			cause = cause[:maxDiagnostic] + "…"
		}
		slog.Warn("node local image operation failed", "method", method, "namespace", selection.Namespace,
			"target_namespace", selection.TargetNamespace, "ref", commandlog.Redact(selection.Ref), "reason", cause)
		return &proto.RPCError{Code: proto.ErrorLocalImageUnavailable, Message: "the node could not complete the local image operation"}
	}
}

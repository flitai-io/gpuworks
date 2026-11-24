package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/imagearchive"
)

func runtimeImageRPC(ctx context.Context, driver runtime.Driver, lock *sync.Mutex, method string, raw json.RawMessage) (any, error) {
	var selection proto.RuntimeImageSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}
	d, ok := driver.(runtime.LocalImageStore)
	if !ok {
		return nil, localImageError(runtime.ErrLocalImageUnsupported)
	}
	timeout := time.Minute
	if method == proto.RPCImageRuntimeImport {
		timeout = proto.ImageOperationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if method == proto.RPCImageRuntimeList {
		result, err := d.ListLocalImages(ctx, selection.Namespace)
		if err != nil {
			return nil, localImageError(err)
		}
		return result, nil
	}
	if !runtime.ValidLocalImageRef(selection.Ref) || !runtime.ValidImageDigest(selection.Identity) {
		return nil, localImageError(runtime.ErrLocalImageChanged)
	}
	adopt := method == proto.RPCImageRuntimeImport
	if adopt {
		if !lock.TryLock() {
			return nil, &proto.RPCError{Code: proto.ErrorLocalImageUnavailable, Message: "another image operation is in progress; retry when it finishes"}
		}
		defer lock.Unlock()
	}
	image, err := d.ResolveLocalImage(ctx, selection, adopt)
	if err != nil {
		return nil, localImageError(err)
	}
	meta := imagearchive.MetadataFromConfig(image.Architecture, image.Labels)
	return proto.RuntimeImageDetails{ImageID: image.ID, RuntimeSizeBytes: image.RuntimeSizeBytes, Metadata: proto.ArchiveImageMetadata(meta)}, nil
}

func localImageError(err error) error {
	switch {
	case errors.Is(err, runtime.ErrLocalImageChanged), errors.Is(err, runtime.ErrImageNotFound):
		return &proto.RPCError{Code: proto.ErrorLocalImageChanged, Message: "the selected image changed or disappeared; refresh the list"}
	case errors.Is(err, runtime.ErrLocalImageConflict):
		return &proto.RPCError{Code: proto.ErrorLocalImageConflict, Message: runtime.ErrLocalImageConflict.Error()}
	case errors.Is(err, runtime.ErrLocalImageUnsupported):
		return &proto.RPCError{Code: proto.ErrorLocalImageUnsupported, Message: runtime.ErrLocalImageUnsupported.Error()}
	default:
		return &proto.RPCError{Code: proto.ErrorLocalImageUnavailable, Message: "the node could not complete the local image operation"}
	}
}

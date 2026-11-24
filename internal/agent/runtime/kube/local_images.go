package kube

import (
	"context"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (d *Driver) ListLocalImages(ctx context.Context, namespace string) (proto.RuntimeImageList, error) {
	s, ok := d.o.Images.(runtime.LocalImageStore)
	if !ok {
		return proto.RuntimeImageList{}, runtime.ErrLocalImageUnsupported
	}
	return s.ListLocalImages(ctx, namespace)
}

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, copyJob *runtime.LocalImageCopy) (runtime.ImageInspection, error) {
	s, ok := d.o.Images.(runtime.LocalImageStore)
	if !ok {
		return runtime.ImageInspection{}, runtime.ErrLocalImageUnsupported
	}
	return s.ResolveLocalImage(ctx, p, copyJob)
}

func (d *Driver) NeedsLocalImageCopy(ctx context.Context, ref, configID, stateDir string) (bool, error) {
	target, ok := d.o.Images.(runtime.LocalImageCopyTarget)
	if !ok {
		return false, runtime.ErrLocalImageUnsupported
	}
	return target.NeedsLocalImageCopy(ctx, ref, configID, stateDir)
}

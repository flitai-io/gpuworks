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

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, adopt bool) (runtime.ImageInspection, error) {
	s, ok := d.o.Images.(runtime.LocalImageStore)
	if !ok {
		return runtime.ImageInspection{}, runtime.ErrLocalImageUnsupported
	}
	return s.ResolveLocalImage(ctx, p, adopt)
}

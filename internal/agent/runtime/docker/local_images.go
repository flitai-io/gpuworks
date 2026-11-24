package docker

import (
	"context"
	"os/exec"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (d *Driver) ListLocalImages(ctx context.Context, ns string) (proto.RuntimeImageList, error) {
	if ns != "" {
		return proto.RuntimeImageList{}, runtime.ErrLocalImageChanged
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "ls", "--no-trunc", "--format", runtime.LocalImageListFormat).Output()
	if err != nil {
		return proto.RuntimeImageList{}, err
	}
	return proto.RuntimeImageList{Runtime: d.Name(), Items: runtime.ParseLocalImages(string(out))}, nil
}

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, _ bool) (runtime.ImageInspection, error) {
	if p.Runtime != d.Name() || p.Namespace != "" || p.TargetNamespace != "" {
		return runtime.ImageInspection{}, runtime.ErrLocalImageChanged
	}
	image, err := d.InspectImage(ctx, p.Ref)
	if err == nil && image.ID != p.Identity {
		err = runtime.ErrLocalImageChanged
	}
	return image, err
}

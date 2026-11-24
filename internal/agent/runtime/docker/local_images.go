package docker

import (
	"context"
	"fmt"
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
		return proto.RuntimeImageList{}, fmt.Errorf("docker image ls: %w", err)
	}
	return proto.RuntimeImageList{Runtime: d.Name(), Items: runtime.ParseLocalImages(string(out))}, nil
}

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, _ *runtime.LocalImageCopy) (runtime.ImageInspection, error) {
	if p.Runtime != d.Name() || p.Namespace != "" || p.TargetNamespace != "" {
		return runtime.ImageInspection{}, runtime.ErrLocalImageChanged
	}
	image, err := d.InspectImage(ctx, p.Ref)
	if err == nil && image.ID != p.Identity {
		err = runtime.ErrLocalImageChanged
	}
	return image, err
}

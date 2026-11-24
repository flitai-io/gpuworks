package nerdctl

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const localImportStagePrefix = "gpuworks.internal/local-image-import:"

func (d *Driver) ListLocalImages(ctx context.Context, ns string) (proto.RuntimeImageList, error) {
	if !runtime.ValidImageNamespace(ns) {
		return proto.RuntimeImageList{}, runtime.ErrLocalImageChanged
	}
	out, err := exec.CommandContext(ctx, "nerdctl", "-n", d.ns, "namespace", "ls", "-q").Output()
	if err != nil {
		return proto.RuntimeImageList{}, err
	}
	spaces := strings.Fields(string(out))
	if !slices.Contains(spaces, d.ns) {
		spaces = append(spaces, d.ns)
	}
	slices.Sort(spaces)
	if ns == "" {
		ns = d.ns
	}
	if !slices.Contains(spaces, ns) {
		return proto.RuntimeImageList{}, runtime.ErrLocalImageChanged
	}
	out, err = exec.CommandContext(ctx, "nerdctl", "-n", ns, "images", "--no-trunc", "--format", runtime.LocalImageListFormat).Output()
	if err != nil {
		return proto.RuntimeImageList{}, err
	}
	items := slices.DeleteFunc(runtime.ParseLocalImages(string(out)), func(row proto.RuntimeImage) bool {
		return strings.HasPrefix(row.Ref, localImportStagePrefix)
	})
	return proto.RuntimeImageList{Runtime: d.Name(), TargetNamespace: d.ns, Namespace: ns, Namespaces: spaces, Items: items}, nil
}

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, adopt bool) (runtime.ImageInspection, error) {
	if p.Runtime != d.Name() || p.TargetNamespace != d.ns || p.Namespace == "" || !runtime.ValidImageNamespace(p.Namespace) {
		return runtime.ImageInspection{}, runtime.ErrLocalImageChanged
	}
	src := NewInNamespace(p.Namespace)
	image, target, err := src.inspectImage(ctx, p.Ref)
	if err != nil {
		return image, err
	}
	if target != p.Identity {
		return image, runtime.ErrLocalImageChanged
	}
	if !adopt || src.ns == d.ns {
		return image, nil
	}

	// A target already usable on this node makes retrying a completed copy cheap.
	if present, err := d.sameLocalImage(ctx, p.Ref, image.ID); err != nil || present {
		return image, err
	}
	// Pin the immutable descriptor under a private tag. The source's real tag
	// may move while saving. Importing only the private tag also lets us check
	// identity and conflicts after the potentially long transfer.
	stage := localImportStagePrefix + strings.ToLower(rand.Text())
	if err := src.TagImage(ctx, p.Ref, stage); err != nil {
		return image, err
	}
	defer src.removeImportTag(stage)
	// Some nerdctl versions can inspect a digest but can tag only by name.
	// Verify the new, pinned name before any bytes go to the destination.
	_, pinned, err := src.inspectImage(ctx, stage)
	if err != nil {
		return image, err
	}
	if pinned != target {
		return image, runtime.ErrLocalImageChanged
	}
	defer d.removeImportTag(stage)
	if err := streamLocalImage(ctx, src, d, stage); err != nil {
		return image, err
	}
	staged, err := d.InspectImage(ctx, stage)
	if err != nil {
		return image, err
	}
	if staged.ID != image.ID {
		return image, runtime.ErrLocalImageChanged
	}
	if present, err := d.sameLocalImage(ctx, p.Ref, image.ID); err != nil || present {
		return image, err
	}
	if err := d.TagImage(ctx, stage, p.Ref); err != nil {
		return image, err
	}
	final, err := d.InspectImage(ctx, p.Ref)
	if err == nil && final.ID != image.ID {
		err = runtime.ErrLocalImageChanged
	}
	return final, err
}

func (d *Driver) sameLocalImage(ctx context.Context, ref, id string) (bool, error) {
	current, err := d.InspectImage(ctx, ref)
	if errors.Is(err, runtime.ErrImageNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.ID != id {
		return false, runtime.ErrLocalImageConflict
	}
	return true, nil
}

func (d *Driver) removeImportTag(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.RemoveImage(ctx, ref); err != nil && !errors.Is(err, runtime.ErrImageNotFound) {
		slog.Warn("remove temporary local import tag", "namespace", d.ns, "image", ref, "err", err)
	}
}

// The archive travels through a kernel pipe on the worker, even when nerdctl
// itself runs through the Kubernetes host wrapper. No layer visits the server.
func streamLocalImage(ctx context.Context, source, dest *Driver, ref string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	exported := make(chan error, 1)
	go func() {
		err := source.SaveImage(ctx, ref, w)
		w.Close()
		exported <- err
	}()
	loadErr := dest.LoadImage(ctx, ref, r)
	r.Close()
	if loadErr != nil {
		cancel()
	}
	return errors.Join(loadErr, <-exported)
}

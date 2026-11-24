package nerdctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"

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
		return proto.RuntimeImageList{}, fmt.Errorf("nerdctl namespace ls: %w", err)
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
		return proto.RuntimeImageList{}, fmt.Errorf("nerdctl images: %w", err)
	}
	items := slices.DeleteFunc(runtime.ParseLocalImages(string(out)), func(row proto.RuntimeImage) bool {
		return strings.HasPrefix(row.Ref, localImportStagePrefix)
	})
	return proto.RuntimeImageList{Runtime: d.Name(), TargetNamespace: d.ns, Namespace: ns, Namespaces: spaces, Items: items}, nil
}

func (d *Driver) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, copyJob *runtime.LocalImageCopy) (runtime.ImageInspection, error) {
	if p.Runtime != d.Name() || p.TargetNamespace != d.ns || p.Namespace == "" || !runtime.ValidImageNamespace(p.Namespace) {
		return runtime.ImageInspection{}, runtime.ErrLocalImageChanged
	}
	src := NewInNamespace(p.Namespace)
	image, target, err := src.inspectImage(ctx, p.Ref)
	if err != nil {
		return image, fmt.Errorf("inspect selected image: %w", err)
	}
	if target != p.Identity {
		return image, runtime.ErrLocalImageChanged
	}
	if copyJob == nil || src.ns == d.ns {
		return image, nil
	}

	journal, err := d.openImportJournal(copyJob.StateDir)
	if err != nil {
		return image, err
	}
	defer journal.Close()
	if err := d.recoverImportTags(ctx, journal.Name()); err != nil {
		return image, err
	}
	// A target already usable on this node makes retrying a completed copy cheap.
	if present, err := d.sameLocalImage(ctx, p.Ref, image.ID); err != nil || present {
		return image, err
	}
	client, err := d.openImagePublisher(ctx, p.Ref)
	if err != nil {
		return image, err
	}
	defer client.Close()
	// Pin the immutable descriptor under a private tag. The source's real tag
	// may move while saving. Importing only the private tag also lets us check
	// identity and conflicts after the potentially long transfer.
	stage, marker, err := journalImportTag(journal.Name(), src.ns)
	if err != nil {
		return image, err
	}
	defer func() {
		if err := d.clearImportTag(marker, src.ns, stage); err != nil {
			slog.Warn("clean up local image import", "image", stage, "err", err)
		}
	}()
	if err := src.TagImage(ctx, p.Ref, stage); err != nil {
		return image, fmt.Errorf("pin source image: %w", err)
	}
	// Some nerdctl versions can inspect a digest but can tag only by name.
	// Verify the new, pinned name before any bytes go to the destination.
	_, pinned, err := src.inspectImage(ctx, stage)
	if err != nil {
		return image, fmt.Errorf("inspect pinned source: %w", err)
	}
	if pinned != target {
		return image, runtime.ErrLocalImageChanged
	}
	if err := streamLocalImage(ctx, src, d, stage, copyJob); err != nil {
		return image, err
	}
	staged, err := d.InspectImage(ctx, stage)
	if err != nil {
		return image, fmt.Errorf("inspect staged destination: %w", err)
	}
	if staged.ID != image.ID {
		return image, runtime.ErrLocalImageChanged
	}
	return d.publishLocalImage(ctx, client, stage, p.Ref, image.ID)
}

func (d *Driver) sameLocalImage(ctx context.Context, ref, id string) (bool, error) {
	current, err := d.InspectImage(ctx, ref)
	if errors.Is(err, runtime.ErrImageNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect destination: %w", err)
	}
	if current.ID != id {
		return false, runtime.ErrLocalImageConflict
	}
	return true, nil
}

// Save writes the tar to stdout; keep bounded stderr for the agent diagnostic.
type exportDiagnostic []byte

func (b *exportDiagnostic) Write(p []byte) (int, error) {
	const limit = 4096
	*b = append(*b, p[:min(len(p), limit-len(*b))]...)
	return len(p), nil
}

func runImageExport(cmd *exec.Cmd) error {
	var stderr exportDiagnostic
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		exit.Stderr = stderr
	}
	return err
}

// The archive travels through a kernel pipe on the worker, even when nerdctl
// itself runs through the Kubernetes host wrapper. No layer visits the server.
func streamLocalImage(ctx context.Context, source, dest *Driver, ref string, progress io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	exported := make(chan error, 1)
	go func() {
		err := source.SaveImage(ctx, ref, io.MultiWriter(w, progress))
		w.Close()
		exported <- err
	}()
	loadErr := dest.LoadImage(ctx, ref, r)
	r.Close()
	if loadErr != nil {
		cancel()
	}
	saveErr := <-exported
	if saveErr != nil {
		saveErr = fmt.Errorf("export source image: %w", saveErr)
	}
	if loadErr != nil {
		loadErr = fmt.Errorf("load destination image: %w", loadErr)
	}
	return errors.Join(saveErr, loadErr)
}

package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// WithOtherRuntimes injects the node's other container runtimes as image
// sources. A node that switched runtimes still holds its images in the
// runtime it left; copying one over the local disk beats downloading it again.
// list is asked only when the active runtime, named active, lacks an image.
func WithOtherRuntimes(list func(ctx context.Context, active string) []runtime.LocalImageSource) EngineOption {
	return func(engine *Engine) {
		engine.otherRuntimes = list
	}
}

// otherRuntimeProgressInterval spaces the progress reports of a copy between
// runtimes. A variable so a test need not wait.
var otherRuntimeProgressInterval = time.Second

// importFromOtherRuntime copies spec's image into driver from another runtime
// on this node that holds it, and reports whether driver now holds it under
// spec.Image. A failed copy is reported and the next runtime, then the normal
// download, is tried. Only a tag reference is copied: an archive does not carry
// the registry digest a runtime records on pull, so a digest-pinned name
// cannot be recreated by a load.
func (e *Engine) importFromOtherRuntime(ctx context.Context, driver runtime.Driver, spec proto.InstanceDesiredState, name string) bool {
	if e.otherRuntimes == nil || imageDigest(spec.Image) != "" {
		return false
	}
	loader, ok := driver.(runtime.ImageLoader)
	if !ok {
		return false
	}
	for _, source := range e.otherRuntimes(ctx, driver.Name()) {
		if source == nil || strings.EqualFold(source.Name(), driver.Name()) {
			continue
		}
		held, err := source.HoldsImage(ctx, spec.Image)
		if err != nil {
			e.log.Debug("other runtime image check failed", "instance", spec.UUID, "image", spec.Image, "runtime", source.Name(), "err", err)
			continue
		}
		if !held {
			continue
		}
		e.reportStateProgress(spec, proto.InstPulling, name, "", "copying the image from the local "+source.Name()+" runtime", proto.PhaseImagePull, 0, 0, 0, 0)
		err = copyImageBetweenRuntimes(ctx, source, loader, spec.Image, func(done, rate int64) {
			e.reportStateProgress(spec, proto.InstPulling, name, "", "copying the image from the local "+source.Name()+" runtime", proto.PhaseImagePull, 0, done, 0, rate)
		})
		if err == nil {
			var present bool
			present, err = driver.ImageExists(ctx, spec.Image)
			if err == nil && !present {
				err = errors.New("the image is still missing after the copy")
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			e.log.Warn("image copy from another runtime failed", "instance", spec.UUID, "image", spec.Image, "from", source.Name(), "to", driver.Name(), "err", err)
			e.reportStateProgress(spec, proto.InstPulling, name, "", "image copy from the local "+source.Name()+" runtime failed: "+err.Error(), proto.PhaseImagePullRetry, 0, 0, 0, 0)
			continue
		}
		e.log.Info("image copied from another runtime", "instance", spec.UUID, "image", spec.Image, "from", source.Name(), "to", driver.Name())
		e.reportStateProgress(spec, proto.InstPulling, name, "", "image copied from the local "+source.Name()+" runtime", proto.PhaseImageVerify, 1, 0, 0, 0)
		return true
	}
	return false
}

// copyImageBetweenRuntimes streams source's archive of image straight into
// loader, so the copy needs no disk beyond what the target keeps. Whichever
// side fails first stops the other.
func copyImageBetweenRuntimes(ctx context.Context, source runtime.LocalImageSource, loader runtime.ImageLoader, image string, progress func(done, rate int64)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		err := source.SaveImage(ctx, image, pw)
		// Recorded before the load can see the stream end, so a failed save
		// is known as the cause of the failed load it leads to.
		saved <- err
		pw.CloseWithError(err)
	}()
	loadErr := loader.LoadImage(ctx, image, &copyMeter{r: pr, progress: progress, last: time.Now()})
	// A save that already failed is why the load failed; a save still running
	// when the load failed is stopped here, and its error is only the echo.
	var saveErr error
	saveDone := false
	select {
	case saveErr = <-saved:
		saveDone = true
	default:
	}
	if loadErr != nil {
		cancel()
	}
	// Unblocks a save still writing when the load returned without reading
	// the archive to its end.
	pr.CloseWithError(errors.New("image load ended"))
	if !saveDone {
		if err := <-saved; loadErr == nil {
			saveErr = err
		}
	}
	if saveErr != nil {
		return errors.New("save from " + source.Name() + ": " + saveErr.Error())
	}
	if loadErr != nil {
		return errors.New("load: " + loadErr.Error())
	}
	return nil
}

// copyMeter counts the archive bytes a load has read and reports them with
// the rate since the previous report, at most once per interval.
type copyMeter struct {
	r        io.Reader
	progress func(done, rate int64)
	done     int64
	reported int64
	last     time.Time
}

func (m *copyMeter) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.done += int64(n)
	if elapsed := time.Since(m.last); elapsed >= otherRuntimeProgressInterval {
		rate := int64(float64(m.done-m.reported) / elapsed.Seconds())
		m.progress(m.done, rate)
		m.reported, m.last = m.done, time.Now()
	}
	return n, err
}

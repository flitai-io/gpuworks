package udocker

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// The native pipeline replaces udocker's image handling: `udocker pull`
// (serial single-stream downloads plus a second pass to re-hash every blob in
// Python), `udocker load`, `udocker create` (two tar subprocesses per layer,
// each decoding that layer's gzip on one core) and the metadata commands. It
// writes the same UDOCKER_DIR layout, so the udocker CLI launcher can still
// operate on what it produces.
//
// Native failures are reported rather than retried through the CLI: the CLI,
// as GPUWorks invokes it (anonymous, https only), covers nothing native does
// not except schema 1 manifests. The CLI remains for images the CLI itself
// registered in the past, and behind GPUWORKS_UDOCKER_DISABLE_NATIVE.

// disableNativeEnv turns the native pull/create off and restores the pure
// udocker CLI path.
const disableNativeEnv = "GPUWORKS_UDOCKER_DISABLE_NATIVE"

// nativeEngine lazily builds the engine bound to this driver's UDOCKER_DIR.
// It returns nil when the native path is disabled, which callers read as
// "use the udocker CLI".
func (d *Driver) nativeEngine() *native.Engine {
	if envTrue(disableNativeEnv) {
		return nil
	}
	d.nativeOnce.Do(func() {
		d.native = native.NewEngine(d.udockerDir, d.runUserOwner())
	})
	return d.native
}

// runUserOwner resolves the dedicated udocker account to a uid/gid so that
// everything the agent writes as root is readable, and replaceable, by the
// user udocker itself runs as. Returns nil when the driver runs udocker as the
// current user.
func (d *Driver) runUserOwner() *native.Owner {
	if d.runUser == "" {
		return nil
	}
	account, err := user.Lookup(d.runUser)
	if err != nil {
		return nil
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil {
		return nil
	}
	return &native.Owner{UID: uid, GID: gid}
}

// nativeCreate extracts image into a container named name. handled is false
// only for an image the native layout does not hold -- one the udocker CLI
// registered in the past (v1 load layout, or blobs rewritten by the retired
// gzip-to-tar pass) -- which the caller hands to `udocker create`. A native
// image whose create fails is an error, not a reason to try the CLI.
func (d *Driver) nativeCreate(ctx context.Context, name, image string, onLine func(string)) (handled bool, err error) {
	engine := d.nativeEngine()
	if engine == nil || !engine.HasImage(image) {
		return false, nil
	}
	if err := engine.Create(ctx, name, image, onLine); err != nil {
		return true, fmt.Errorf("create container %s: %w", name, err)
	}
	return true, nil
}

// nativeImageComplete reports whether the native layout considers image fully
// present, which is a cheap local check compared with starting Python.
func (d *Driver) nativeImageComplete(image string) bool {
	engine := d.nativeEngine()
	return engine != nil && engine.HasImage(image)
}

func (d *Driver) nativeImageID(image string) string {
	engine := d.nativeEngine()
	if engine == nil || !engine.HasImage(image) {
		return ""
	}
	id, err := engine.Layout.ImageID(native.ParseRef(image))
	if err != nil {
		return ""
	}
	return id
}

// nativeInspect answers InspectImage from the native layout: the immutable
// manifest digest plus the config defaults, without starting the udocker CLI.
func (d *Driver) nativeInspect(image string) (runtime.ImageInspection, bool) {
	engine := d.nativeEngine()
	if engine == nil || !engine.HasImage(image) {
		return runtime.ImageInspection{}, false
	}
	ref := native.ParseRef(image)
	id, err := engine.Layout.ImageID(ref)
	if err != nil {
		return runtime.ImageInspection{}, false
	}
	inspection := runtime.ImageInspection{ID: id}
	if config, err := engine.Layout.LoadImageConfig(ref); err == nil {
		inspection.Entrypoint = config.Entrypoint
		inspection.Cmd = config.Cmd
		inspection.Volumes = config.Volumes
		if len(config.Env) > 0 {
			inspection.Env = make(map[string]string, len(config.Env))
			for _, entry := range config.Env {
				if k, v, ok := strings.Cut(entry, "="); ok {
					inspection.Env[k] = v
				}
			}
		}
	}
	return inspection, true
}

// nativeTagImage tags a natively registered image in the layout. It reports
// whether it handled the request; false leaves it to `udocker tag`.
func (d *Driver) nativeTagImage(source, target string) (bool, error) {
	engine := d.nativeEngine()
	if engine == nil || !engine.HasImage(source) {
		return false, nil
	}
	return true, engine.Layout.TagImage(native.ParseRef(source), native.ParseRef(target))
}

// nativeRemoveImage removes an image registered under its exact native
// reference. It reports whether it handled the request.
func (d *Driver) nativeRemoveImage(image string) (bool, error) {
	engine := d.nativeEngine()
	if engine == nil {
		return false, nil
	}
	ref := native.ParseRef(image)
	if _, err := os.Stat(filepath.Join(engine.Layout.TagDir(ref), "TAG")); err != nil {
		return false, nil //nolint:nilerr // No native candidate: let the existing CLI removal path decide the result.
	}
	return true, engine.Layout.RemoveImage(ref)
}

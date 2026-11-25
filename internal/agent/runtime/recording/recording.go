// Package recording provides an explicit environment-selected fake runtime
// for L2 process-boundary tests. Production startup never selects it unless
// GPUWORKS_FAKE_RUNTIME_RECORD names an output file.
//
// The driver also records the order of the image operations it was asked to
// perform and lets an in-process test inject each failure the real runtimes
// can produce (a load that fails, a load that leaves the canonical reference
// absent with or without naming the reference it did create, a tag that
// fails, an inspect that fails). A zero-valued Driver succeeds at everything,
// which is what the environment-selected production instance needs.
package recording

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
)

type Driver struct {
	path    string
	mu      sync.Mutex
	running map[string]bool

	calls         []string
	loadedPaths   []string
	loadFileErr   error
	reportRef     *string
	missingImages map[string]bool
	tagErr        error
	inspectErr    error
	removeErr     error
	inspection    runtime.ImageInspection
	loadFileHook  func(path string)

	// name is the runtime the driver stands in for.
	name string

	// engineBin, when set, is a fake engine started for every serving
	// container on the API port its command names (engine.go).
	engineBin string
	engines   map[string]*exec.Cmd
}

// New returns a driver that records to path under docker's name: a node is
// admitted for a vendor's cards only on a runtime that vendor lists, and no
// vendor lists a made-up name.
func New(path string) *Driver {
	return &Driver{path: path, name: runtime.DriverDocker, running: map[string]bool{}, engines: map[string]*exec.Cmd{}}
}

// As makes the driver stand in for another runtime by reporting its name.
func (d *Driver) As(name string) *Driver {
	d.name = name
	return d
}

func (d *Driver) Name() string { return d.name }

// DeviceView says a recorded container sees only the devices it was
// granted, as one of docker's would.
func (d *Driver) DeviceView() runtime.DeviceView { return runtime.ViewGranted }

// ImageStoreRoot: a recorded image is kept nowhere, whatever runtime the
// driver is named after.
func (d *Driver) ImageStoreRoot() string { return "" }

func (d *Driver) Detect(context.Context) (bool, error) { return true, nil }

func (d *Driver) ImageExists(_ context.Context, image string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "ImageExists("+image+")")
	if d.missingImages[image] {
		return false, nil
	}
	return true, nil
}

func (d *Driver) PullImage(context.Context, string, func(string)) error { return nil }
func (d *Driver) Prepare(context.Context, runtime.WorkloadSpec) error   { return nil }
func (d *Driver) Run(_ context.Context, spec runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return "", err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return "", err
	}
	id := containerID(spec.Name)
	d.running[id] = spec.Image != assets.PartitionProbeImage
	if d.running[id] && d.engineBin != "" {
		if err := d.startEngineLocked(id, spec); err != nil {
			return "", err
		}
	}
	return id, nil
}
func (d *Driver) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	id = containerID(id)
	d.running[id] = false
	d.stopEngineLocked(id)
	d.mu.Unlock()
	return nil
}
func (d *Driver) Remove(_ context.Context, id string) error {
	d.mu.Lock()
	id = containerID(id)
	delete(d.running, id)
	d.stopEngineLocked(id)
	d.mu.Unlock()
	return nil
}

// containerID is the id Run returned for a container, given that id or the
// container's name: the agent addresses a container by either.
func containerID(nameOrID string) string {
	if strings.HasPrefix(nameOrID, "recording-") {
		return nameOrID
	}
	return "recording-" + nameOrID
}
func (d *Driver) ListContainers(context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := make([]string, 0, len(d.running))
	for id := range d.running {
		names = append(names, id)
	}
	sort.Strings(names)
	return names, nil
}
func (d *Driver) RunningContainers(context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for id, running := range d.running {
		if running {
			names = append(names, id)
		}
	}
	sort.Strings(names)
	return names, nil
}
func (d *Driver) Status(_ context.Context, id string) (runtime.Status, error) {
	d.mu.Lock()
	running, exists := d.running[containerID(id)]
	d.mu.Unlock()
	return runtime.Status{Exists: exists, Running: running, Health: "healthy"}, nil
}
func (d *Driver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(&emptyReader{}), nil
}
func (d *Driver) Exec(context.Context, string, []string) ([]byte, error) { return nil, nil }

func (d *Driver) SaveImage(_ context.Context, _ string, w io.Writer) error {
	_, err := w.Write([]byte("recording-image"))
	return err
}

func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	_, err := d.LoadImageRef(ctx, image, r)
	return err
}

// LoadImageRef consumes the stream and reports the reference the archive
// carried, the way docker's and nerdctl's load output names it. The injected
// failures and references apply here exactly as they do to a file load, so a
// test can drive the relayed path through the same cases.
func (d *Driver) LoadImageRef(_ context.Context, image string, r io.Reader) (string, error) {
	d.mu.Lock()
	d.calls = append(d.calls, "LoadImage("+image+")")
	ref, loadErr := d.loadedRefLocked(image)
	d.mu.Unlock()
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", err
	}
	if loadErr != nil {
		return "", loadErr
	}
	return ref, nil
}

// loadedRefLocked is the one place both load entry points decide what the
// archive carried and which references the load made present.
func (d *Driver) loadedRefLocked(image string) (string, error) {
	if d.loadFileErr != nil {
		return "", d.loadFileErr
	}
	ref := image
	if d.reportRef != nil {
		ref = *d.reportRef
	}
	if ref != "" {
		// A load makes present whatever reference the archive carried.
		delete(d.missingImages, ref)
	}
	return ref, nil
}

// LoadImageFile records the archive path it was handed. The path is kept so a
// test can assert the archive really was loaded from where it was downloaded
// rather than from a staged copy.
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	_, err := d.LoadImageFileRef(ctx, image, path)
	return err
}

func (d *Driver) LoadImageFileRef(_ context.Context, image, path string) (string, error) {
	d.mu.Lock()
	d.calls = append(d.calls, "LoadImageFile("+image+")")
	d.loadedPaths = append(d.loadedPaths, path)
	hook := d.loadFileHook
	// By default the archive carries exactly the reference it was asked for and
	// the runtime says so, which is what docker and nerdctl print. A test that
	// needs the other cases injects them.
	ref, err := d.loadedRefLocked(image)
	d.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	if err != nil {
		return "", err
	}
	return ref, nil
}

func (d *Driver) TagImage(_ context.Context, source, target string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "TagImage("+source+"->"+target+")")
	if d.tagErr != nil {
		return d.tagErr
	}
	// Tagging makes the target reference present, which is the whole point of
	// the fallback the agent runs when an archive carried another reference.
	delete(d.missingImages, target)
	return nil
}

// RemoveImage records the deletion so a test can assert which reference a node
// was asked to drop.
func (d *Driver) RemoveImage(_ context.Context, image string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "RemoveImage("+image+")")
	return d.removeErr
}

func (d *Driver) InspectImage(_ context.Context, image string) (runtime.ImageInspection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "InspectImage("+image+")")
	if d.inspectErr != nil {
		return runtime.ImageInspection{}, d.inspectErr
	}
	return d.inspection, nil
}

// Calls returns the recorded image-operation sequence.
func (d *Driver) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// LoadedPaths returns every archive path handed to LoadImageFile.
func (d *Driver) LoadedPaths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.loadedPaths...)
}

// InjectLoadFailure makes the next LoadImageFile fail.
func (d *Driver) InjectLoadFailure(err error) {
	d.mu.Lock()
	d.loadFileErr = err
	d.mu.Unlock()
}

// InjectLoadedRef makes LoadImageFile report ref as the reference the archive
// actually carried, the way docker's "Loaded image:" line does.
func (d *Driver) InjectLoadedRef(ref string) {
	d.mu.Lock()
	d.reportRef = &ref
	d.mu.Unlock()
}

// InjectNoLoadedRef makes a successful load name no reference at all, the way
// docker's "Loaded image ID:" line does for a reference-less archive.
func (d *Driver) InjectNoLoadedRef() {
	empty := ""
	d.mu.Lock()
	d.reportRef = &empty
	d.mu.Unlock()
}

// InjectMissingImage makes ImageExists report image as absent until it is
// tagged into place.
func (d *Driver) InjectMissingImage(image string) {
	d.mu.Lock()
	if d.missingImages == nil {
		d.missingImages = map[string]bool{}
	}
	d.missingImages[image] = true
	d.mu.Unlock()
}

// InjectTagFailure makes TagImage fail.
func (d *Driver) InjectTagFailure(err error) {
	d.mu.Lock()
	d.tagErr = err
	d.mu.Unlock()
}

// InjectInspectFailure makes InspectImage fail. Hydration is best effort, so
// callers must still reach a terminal success state.
func (d *Driver) InjectInspectFailure(err error) {
	d.mu.Lock()
	d.inspectErr = err
	d.mu.Unlock()
}

// InjectInspection sets the metadata InspectImage reports.
func (d *Driver) InjectInspection(inspection runtime.ImageInspection) {
	d.mu.Lock()
	d.inspection = inspection
	d.mu.Unlock()
}

// InjectRemoveFailure makes RemoveImage fail, as a runtime does while a
// container still uses the image.
func (d *Driver) InjectRemoveFailure(err error) {
	d.mu.Lock()
	d.removeErr = err
	d.mu.Unlock()
}

// OnLoadImageFile runs hook with the archive path while the load is in
// progress, so a test can observe the file that is about to be consumed.
func (d *Driver) OnLoadImageFile(hook func(path string)) {
	d.mu.Lock()
	d.loadFileHook = hook
	d.mu.Unlock()
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

var (
	_ runtime.Driver              = (*Driver)(nil)
	_ runtime.ImageArchiver       = (*Driver)(nil)
	_ runtime.LoadedRefReporter   = (*Driver)(nil)
	_ runtime.StreamedRefReporter = (*Driver)(nil)
	_ runtime.ImageTagger         = (*Driver)(nil)
	_ runtime.ImageInspector      = (*Driver)(nil)
)

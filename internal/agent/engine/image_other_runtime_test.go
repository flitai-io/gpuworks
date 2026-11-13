package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const otherRuntimeImage = "registry.example/team/runner:v1"

// loadingDriver is the active runtime: an image exists once it was pulled,
// tagged or loaded, and a load reads its archive to the end like a runtime
// CLI reading stdin.
type loadingDriver struct {
	*imageToleranceDriver
	name    string
	loaded  []byte
	loadErr error
	// loadReads, with loadErr, is how much a failing load reads first.
	loadReads int
	// loadKeepsNothing models a load that reports success but leaves the
	// image missing.
	loadKeepsNothing bool
}

func newLoadingDriver(name string) *loadingDriver {
	return &loadingDriver{imageToleranceDriver: &imageToleranceDriver{present: map[string]bool{}}, name: name}
}

func (d *loadingDriver) Name() string { return d.name }

func (d *loadingDriver) LoadImage(_ context.Context, image string, r io.Reader) error {
	if d.loadErr != nil {
		buf := make([]byte, d.loadReads)
		n, _ := io.ReadFull(r, buf)
		d.loaded = buf[:n]
		return d.loadErr
	}
	data, err := io.ReadAll(r)
	d.loaded = data
	if err != nil {
		return err
	}
	if !d.loadKeepsNothing {
		d.present[image] = true
	}
	return nil
}

// fakeImageSource is another runtime on the node. Its save writes archive,
// or the first saveFailAfter bytes of it and then fails with saveErr.
type fakeImageSource struct {
	name          string
	held          map[string]bool
	holdErr       error
	archive       []byte
	saveErr       error
	saveFailAfter int

	mu       sync.Mutex
	asked    []string
	saved    []string
	saveDone error
}

func (s *fakeImageSource) Name() string { return s.name }

func (s *fakeImageSource) HoldsImage(_ context.Context, image string) (bool, error) {
	s.mu.Lock()
	s.asked = append(s.asked, image)
	s.mu.Unlock()
	return s.held[image], s.holdErr
}

func (s *fakeImageSource) SaveImage(_ context.Context, image string, w io.Writer) error {
	s.mu.Lock()
	s.saved = append(s.saved, image)
	s.mu.Unlock()
	data := s.archive
	if s.saveErr != nil {
		data = data[:s.saveFailAfter]
	}
	_, err := w.Write(data)
	if err == nil {
		err = s.saveErr
	}
	s.mu.Lock()
	s.saveDone = err
	s.mu.Unlock()
	return err
}

// startWithOtherRuntimes runs one start attempt with sources as the node's
// other runtimes and returns every status reported and the active runtime
// names the engine asked about.
func startWithOtherRuntimes(t *testing.T, driver runtime.Driver, spec proto.InstanceDesiredState, sources ...runtime.LocalImageSource) ([]proto.InstanceStatus, []string) {
	t.Helper()
	h := newSleepHTTPServer(t)
	var mu sync.Mutex
	var statuses []proto.InstanceStatus
	var actives []string
	e := NewWithProber(driver, func(st proto.InstanceStatus) {
		mu.Lock()
		statuses = append(statuses, st)
		mu.Unlock()
	}, slog.Default(), t.TempDir(), injectionProber{}, WithOtherRuntimes(func(_ context.Context, active string) []runtime.LocalImageSource {
		actives = append(actives, active)
		return sources
	}))
	spec.UUID = "other-runtime"
	spec.APIPort, spec.Ports, spec.StartTimeout = h.port(t), []int{h.port(t)}, 1
	e.startOrAdopt(t.Context(), spec)
	mu.Lock()
	defer mu.Unlock()
	return statuses, actives
}

func hasMessage(statuses []proto.InstanceStatus, phase, text string) bool {
	for _, st := range statuses {
		if st.Phase == phase && strings.Contains(st.Message, text) {
			return true
		}
	}
	return false
}

func archiveBytes(n int) []byte {
	return bytes.Repeat([]byte("layer-bytes."), n/12+1)[:n]
}

// The runtime a node switched away from still holds the image: it is copied
// over the local disk, byte for byte, and nothing is downloaded.
func TestImageMissingFromTheActiveRuntimeIsCopiedFromAnotherRuntime(t *testing.T) {
	driver := newLoadingDriver("nerdctl")
	source := &fakeImageSource{name: "docker", held: map[string]bool{otherRuntimeImage: true}, archive: archiveBytes(256 << 10)}
	statuses, actives := startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{
		Image: otherRuntimeImage, ImagePullRefs: []string{otherRuntimeImage},
	}, source)
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(actives) != 1 || actives[0] != "nerdctl" {
		t.Fatalf("other runtimes asked with active=%v, want [nerdctl]", actives)
	}
	if driver.pullCalls != 0 {
		t.Fatalf("pulled %v although another runtime held the image", driver.pullRefs)
	}
	if !bytes.Equal(driver.loaded, source.archive) {
		t.Fatalf("loaded %d bytes, want the %d the source saved", len(driver.loaded), len(source.archive))
	}
	if len(source.saved) != 1 || source.saved[0] != otherRuntimeImage {
		t.Fatalf("saved %v", source.saved)
	}
	if len(driver.runImages) != 1 || driver.runImages[0] != otherRuntimeImage {
		t.Fatalf("launched from %v", driver.runImages)
	}
	if !hasMessage(statuses, proto.PhaseImagePull, "copying the image from the local docker runtime") ||
		!hasMessage(statuses, proto.PhaseImageVerify, "image copied from the local docker runtime") {
		t.Fatalf("copy not reported: %+v", statuses)
	}
}

// The progress stream carries the bytes copied so far, as a download would.
func TestImageCopyBetweenRuntimesReportsBytesCopied(t *testing.T) {
	saved := otherRuntimeProgressInterval
	otherRuntimeProgressInterval = 0
	t.Cleanup(func() { otherRuntimeProgressInterval = saved })
	driver := newLoadingDriver("nerdctl")
	source := &fakeImageSource{name: "docker", held: map[string]bool{otherRuntimeImage: true}, archive: archiveBytes(200 << 10)}
	statuses, _ := startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: otherRuntimeImage}, source)
	var last int64
	for _, st := range statuses {
		if st.Phase == proto.PhaseImagePull && st.BytesDone > 0 {
			if st.BytesDone < last {
				t.Fatalf("bytes copied went back from %d to %d", last, st.BytesDone)
			}
			last = st.BytesDone
		}
	}
	if last != int64(len(source.archive)) {
		t.Fatalf("last progress %d bytes, want %d", last, len(source.archive))
	}
}

// Every way a copy can fail leaves the instance to the normal download, and
// the failure is reported under the source's name.
func TestFailedCopyBetweenRuntimesFallsBackToTheDownload(t *testing.T) {
	for name, tc := range map[string]struct {
		source *fakeImageSource
		setup  func(*loadingDriver)
		want   string
	}{
		"save fails partway": {
			source: &fakeImageSource{saveErr: errTest("docker save: no space left"), saveFailAfter: 1000},
			want:   "save from docker: docker save: no space left",
		},
		"load fails before reading it all": {
			source: &fakeImageSource{},
			setup:  func(d *loadingDriver) { d.loadErr, d.loadReads = errTest("unexpected EOF in layer"), 4096 },
			want:   "load: unexpected EOF in layer",
		},
		"load leaves the image missing": {
			source: &fakeImageSource{},
			setup:  func(d *loadingDriver) { d.loadKeepsNothing = true },
			want:   "the image is still missing after the copy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			driver := newLoadingDriver("udocker")
			if tc.setup != nil {
				tc.setup(driver)
			}
			tc.source.name = "docker"
			tc.source.held = map[string]bool{otherRuntimeImage: true}
			tc.source.archive = archiveBytes(1 << 20)
			statuses, _ := startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{
				Image: otherRuntimeImage, ImagePullRefs: []string{otherRuntimeImage},
			}, tc.source)
			if msg := failedMessage(statuses); msg != "" {
				t.Fatalf("start failed: %s", msg)
			}
			if !hasMessage(statuses, proto.PhaseImagePullRetry, "image copy from the local docker runtime failed: "+tc.want) {
				t.Fatalf("failure %q not reported: %+v", tc.want, statuses)
			}
			if len(driver.pullRefs) != 1 || driver.pullRefs[0] != otherRuntimeImage {
				t.Fatalf("pulls = %v, want the download after the failed copy", driver.pullRefs)
			}
			tc.source.mu.Lock()
			defer tc.source.mu.Unlock()
			if len(tc.source.saved) != 1 {
				t.Fatalf("saves = %v", tc.source.saved)
			}
		})
	}
}

// A load that stops reading must not leave the save blocked on the pipe.
func TestCopyBetweenRuntimesReleasesASaveTheLoadStoppedReading(t *testing.T) {
	source := &fakeImageSource{name: "docker", archive: archiveBytes(4 << 20)}
	loader := newLoadingDriver("nerdctl")
	loader.loadErr, loader.loadReads = errTest("bad archive"), 10
	err := copyImageBetweenRuntimes(t.Context(), source, loader, otherRuntimeImage, func(int64, int64) {})
	if err == nil || err.Error() != "load: bad archive" {
		t.Fatalf("err = %v, want the load's own error", err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.saveDone == nil {
		t.Fatal("the save finished cleanly although nobody read its archive")
	}
}

// Sources are tried in order: one that cannot answer, one that lacks the
// image and the active runtime itself are passed over.
func TestCopyBetweenRuntimesSkipsSourcesThatCannotHelp(t *testing.T) {
	driver := newLoadingDriver("udocker")
	broken := &fakeImageSource{name: "docker", holdErr: errTest("docker daemon not answering")}
	lacking := &fakeImageSource{name: "nerdctl", held: map[string]bool{}}
	self := &fakeImageSource{name: "UDOCKER", held: map[string]bool{otherRuntimeImage: true}, archive: []byte("self")}
	holder := &fakeImageSource{name: "nerdctl-2", held: map[string]bool{otherRuntimeImage: true}, archive: archiveBytes(4096)}
	statuses, _ := startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: otherRuntimeImage}, broken, lacking, self, holder)
	if msg := failedMessage(statuses); msg != "" {
		t.Fatalf("start failed: %s", msg)
	}
	if len(self.asked) != 0 {
		t.Fatal("the active runtime was asked as its own source")
	}
	if len(broken.saved)+len(lacking.saved) != 0 || len(holder.saved) != 1 {
		t.Fatalf("saved: broken=%v lacking=%v holder=%v", broken.saved, lacking.saved, holder.saved)
	}
	if driver.pullCalls != 0 || !bytes.Equal(driver.loaded, holder.archive) {
		t.Fatalf("pulls=%d loaded %d bytes", driver.pullCalls, len(driver.loaded))
	}
}

// No other runtime is consulted when there is nothing it could do: the image
// is already present, it is digest-pinned (a load cannot recreate a registry
// digest), the active runtime cannot load archives, or the image check itself
// failed.
func TestOtherRuntimesAreNotAskedWhenACopyCannotHelp(t *testing.T) {
	holder := func() *fakeImageSource {
		return &fakeImageSource{name: "docker", held: map[string]bool{otherRuntimeImage: true, canonicalPin: true}, archive: []byte("x")}
	}
	t.Run("already present", func(t *testing.T) {
		driver := newLoadingDriver("nerdctl")
		driver.present[otherRuntimeImage] = true
		source := holder()
		_, actives := startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: otherRuntimeImage}, source)
		if len(actives) != 0 || len(source.asked) != 0 {
			t.Fatalf("asked %v", source.asked)
		}
	})
	t.Run("digest pinned", func(t *testing.T) {
		driver := newLoadingDriver("nerdctl")
		source := holder()
		startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: canonicalPin, ImagePullRefs: []string{canonicalPin}}, source)
		if len(source.asked) != 0 || len(driver.pullRefs) != 1 {
			t.Fatalf("asked %v, pulls %v", source.asked, driver.pullRefs)
		}
	})
	t.Run("active runtime cannot load", func(t *testing.T) {
		driver := &imageToleranceDriver{present: map[string]bool{}}
		source := holder()
		startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: otherRuntimeImage, ImagePullRefs: []string{otherRuntimeImage}}, source)
		if len(source.asked) != 0 || len(driver.pullRefs) != 1 {
			t.Fatalf("asked %v, pulls %v", source.asked, driver.pullRefs)
		}
	})
	t.Run("image check failed", func(t *testing.T) {
		driver := &checkFailingLoader{loadingDriver: newLoadingDriver("nerdctl")}
		source := holder()
		startWithOtherRuntimes(t, driver, proto.InstanceDesiredState{Image: otherRuntimeImage, ImagePullRefs: []string{otherRuntimeImage}}, source)
		if len(source.asked) != 0 {
			t.Fatalf("asked %v after a failed image check", source.asked)
		}
	})
}

// checkFailingLoader cannot answer its first image check.
type checkFailingLoader struct {
	*loadingDriver
	checked bool
}

func (d *checkFailingLoader) ImageExists(ctx context.Context, ref string) (bool, error) {
	if !d.checked {
		d.checked = true
		return false, errors.New("image inspect timed out")
	}
	return d.loadingDriver.ImageExists(ctx, ref)
}

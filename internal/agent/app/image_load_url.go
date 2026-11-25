package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// imageArchiveDirName is the subdirectory of the runtime's image store the
	// downloaded archives live in. Keeping them next to the image store means
	// the admission check below measures the one filesystem that has to hold
	// both the archive and the layers expanded out of it.
	imageArchiveDirName = "gpuworks-image-archives"
	// imageArchiveSpaceNumerator/Denominator express the 2.2x headroom an
	// archive needs: the tar itself plus the expanded layers, which coexist
	// until the load finishes. Integer arithmetic keeps the check exact for
	// sizes past the float53 boundary.
	imageArchiveSpaceNumerator   = 22
	imageArchiveSpaceDenominator = 10
	// imageArchiveSafetyMargin mirrors the model downloader's margin: a disk
	// with nothing to spare beyond the archive is not a usable disk.
	imageArchiveSafetyMargin = 1 << 30
	// imageArchiveRedirects bounds redirect chains for an operator-supplied
	// archive URL.
	imageArchiveRedirects = 5
	// imageArchiveConnections is the parallel range-request count, matching the
	// model downloader.
	imageArchiveConnections = 4
)

// rpcImageLoadURL downloads an image archive published over plain HTTP and
// loads it under the canonical catalog reference. It mirrors rpcImagePull:
// the RPC acknowledges immediately, the work runs in a background goroutine
// serialized on the image slot, and every state change is reported through
// status.image_node.
func (i *nodeImages) rpcImageLoadURL(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageLoadURLParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("image load requires canonical_ref and archive_url")
	}
	p.CanonicalRef = strings.TrimSpace(p.CanonicalRef)
	p.ArchiveURL = strings.TrimSpace(p.ArchiveURL)
	if p.CanonicalRef == "" || p.ArchiveURL == "" {
		return nil, errors.New("image load requires canonical_ref and archive_url")
	}
	if strings.Contains(p.CanonicalRef, "@sha256:") {
		return nil, errors.New("digest image refs are not supported")
	}
	if err := checkArchiveURL(p.ArchiveURL); err != nil {
		return nil, err
	}
	loadCtx := context.Background()
	if ctx != nil {
		loadCtx = context.WithoutCancel(ctx)
	}
	go func() {
		loadCtx, cancelLoad := context.WithTimeout(loadCtx, imageOperationTimeout)
		defer cancelLoad()
		unregister := i.ops.begin(p.CanonicalRef, p.TransferID, cancelLoad)
		defer unregister()
		i.lockSlot(p.CanonicalRef, "syncing")
		defer i.slot.Unlock()
		i.loadImageArchiveURL(loadCtx, p)
	}()
	return proto.ImageLoadURLResult{State: "syncing", CanonicalRef: p.CanonicalRef}, nil
}

// checkArchiveURL enforces the same rules the server applied before handing
// the URL over: http/https only and no embedded credentials. The agent
// re-checks because a node must not fetch an arbitrary scheme just because a
// control message said so.
func checkArchiveURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid archive url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("archive url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("archive url has no host")
	}
	if u.User != nil {
		return errors.New("archive url must not embed credentials")
	}
	return nil
}

func (i *nodeImages) loadImageArchiveURL(ctx context.Context, p proto.ImageLoadURLParams) {
	failed := func(message string) proto.ImageNodeStatus {
		return proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", Message: message}
	}
	report := func(status proto.ImageNodeStatus) {
		if status.State == "failed" {
			i.deps.log().Warn("image archive url load failed", "image", p.CanonicalRef, "err", status.Message)
		} else {
			i.deps.log().Info("image archive url loaded", "image", p.CanonicalRef, "host", archiveURLHost(p.ArchiveURL))
		}
		i.deps.report(status)
	}
	d := i.deps.driver()
	if d == nil {
		report(failed("runtime driver is unavailable"))
		return
	}
	loader, ok := d.(runtime.ImageFileLoader)
	if !ok {
		report(failed("runtime " + i.driverName() + " cannot load an image archive from a file"))
		return
	}
	dir, err := i.imageArchiveDir()
	if err != nil {
		report(failed("image archive directory unavailable: " + err.Error()))
		return
	}
	if err := checkImageArchiveSpace(dir, p.SizeBytes); err != nil {
		// Deliberately before any request: a node that cannot hold the archive
		// must not spend bandwidth discovering that.
		report(failed(err.Error()))
		return
	}
	archive := filepath.Join(dir, imageArchiveFileName(p)+".tar")
	status := i.fetchAndLoadImageArchive(ctx, d, loader, p, archive)
	// The staged archive is deleted before the outcome is announced, on the
	// success and the failure path alike: whatever reacts to the terminal state
	// must never find the node still holding a full copy of a multi-gigabyte
	// tar. A leftover from a killed process is swept up by the next attempt,
	// which writes to the same deterministic path.
	removeArchive(archive)
	report(status)
}

// fetchAndLoadImageArchive downloads the archive, loads it, tags it to the
// canonical reference when the archive carried another one, and hydrates the
// catalog metadata. Progress is reported as it goes; the returned status is
// the terminal one, which the caller announces after the staged file is gone.
func (i *nodeImages) fetchAndLoadImageArchive(ctx context.Context, d runtime.Driver, loader runtime.ImageFileLoader, p proto.ImageLoadURLParams, archive string) proto.ImageNodeStatus {
	failed := func(message string) proto.ImageNodeStatus {
		return proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "failed", TransferID: p.TransferID, Message: message}
	}
	i.deps.report(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "syncing", TransferID: p.TransferID, SizeBytes: p.SizeBytes, Message: "downloading " + archiveURLHost(p.ArchiveURL)})
	net := i.deps.network()
	err := download.Download(ctx, download.Options{
		URL:          p.ArchiveURL,
		Dest:         archive,
		Concurrency:  imageArchiveConnections,
		RateLimitKBs: p.RateLimitKBs,
		Client:       imageArchiveClient(httpClientFor(net)),
	}, func(progress download.Progress) {
		total := progress.Total
		if total <= 0 {
			total = p.SizeBytes
		}
		i.deps.report(proto.ImageNodeStatus{
			ImageRef: p.CanonicalRef, State: "syncing", TransferID: p.TransferID, SizeBytes: total,
			TransferredBytes: progress.Done, RateBPS: progress.RateBytesPerS,
		})
	})
	if err != nil {
		return failed(fmt.Sprintf("download %s: %s", archiveURLHost(p.ArchiveURL), imageOperationError(ctx, err)))
	}

	i.deps.report(proto.ImageNodeStatus{ImageRef: p.CanonicalRef, State: "syncing", TransferID: p.TransferID, SizeBytes: p.SizeBytes, Message: "loading image archive"})
	status, err := loadImageArchive(ctx, d, imageArchiveLoad{
		CanonicalRef: p.CanonicalRef,
		RuntimeName:  i.driverName(),
		Loaded:       "image archive downloaded and loaded",
		Load: func(loadCtx context.Context) (string, error) {
			if reporter, ok := loader.(runtime.LoadedRefReporter); ok {
				return reporter.LoadImageFileRef(loadCtx, p.CanonicalRef, archive)
			}
			return p.CanonicalRef, loader.LoadImageFile(loadCtx, p.CanonicalRef, archive)
		},
	})
	if err != nil {
		return failed(imageOperationError(ctx, err))
	}
	status.TransferID = p.TransferID
	if info, statErr := os.Stat(archive); statErr == nil {
		status.SizeBytes, status.TransferredBytes = info.Size(), info.Size()
	}
	return status
}

// imageArchiveFileName names the staged archive after the transfer it belongs
// to, so two concurrent transfers of the same image cannot collide and a
// restarted agent leaves a file the background cleanup can recognise.
func imageArchiveFileName(p proto.ImageLoadURLParams) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '_'
		}
	}, p.CanonicalRef)
	if name == "" {
		name = "image"
	}
	return name
}

// removeArchive deletes the staged archive and the downloader's resume
// sidecars. It runs on both the success and the failure path: leaving a
// multi-gigabyte tar behind would be a slow disk leak.
func removeArchive(path string) {
	for _, name := range []string{path, path + ".part", path + ".part.state"} {
		_ = os.Remove(name)
	}
}

// imageArchiveDir returns the directory staged archives are written to. It
// prefers the runtime's own image store so the admission check and the load
// see one filesystem, and falls back to the agent state directory when that
// store is not writable by this agent.
func (i *nodeImages) imageArchiveDir() (string, error) {
	roots := []string{i.imageStoreRoot(), filepath.Join(i.deps.stateDir(), "images")}
	var firstErr error
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		dir := filepath.Join(root, imageArchiveDirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return dir, nil
	}
	if firstErr == nil {
		firstErr = errors.New("no writable image archive directory")
	}
	return "", firstErr
}

// imageStoreRoot is where this node's runtime keeps its unpacked layers. A
// stand-in driver answers for itself: the name it reports is another
// runtime's, and that runtime's store is not its own.
func (i *nodeImages) imageStoreRoot() string {
	if standIn, ok := i.deps.driver().(runtime.ImageStoreRooter); ok {
		return standIn.ImageStoreRoot()
	}
	return runtimeImageStoreRoot(i.driverName(), i.deps.modelsRoot())
}

// runtimeImageStoreRoot is where each runtime keeps its unpacked layers.
func runtimeImageStoreRoot(driver, modelsDir string) string {
	switch driver {
	case "udocker":
		return filepath.Join(filepath.Dir(modelsDir), "udocker")
	case "nerdctl":
		return "/var/lib/containerd"
	case "docker":
		return "/var/lib/docker"
	default:
		return ""
	}
}

// checkImageArchiveSpace refuses a download the node cannot hold. A zero or
// negative size means the server could not learn the archive's size, in which
// case there is nothing to check and the download proceeds.
func checkImageArchiveSpace(dir string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return nil
	}
	required := sizeBytes/imageArchiveSpaceDenominator*imageArchiveSpaceNumerator + imageArchiveSafetyMargin
	if required < sizeBytes {
		return fmt.Errorf("image archive size %d is out of range", sizeBytes)
	}
	usage := statManagedPath("image", dir)
	if usage.AvailableBytes <= 0 {
		return fmt.Errorf("insufficient disk space at %s: available=%d required=%d", dir, usage.AvailableBytes, required)
	}
	if usage.AvailableBytes < required {
		return fmt.Errorf("insufficient disk space at %s: available=%d required=%d (archive=%d expansion_and_margin=%d)", dir, usage.AvailableBytes, required, sizeBytes, required-sizeBytes)
	}
	return nil
}

// imageArchiveClient bounds the redirect chain on the client the downloader
// will use. base may be nil, which is the downloader's "use your default"
// signal; a bounded copy is still returned so the bound always applies.
func imageArchiveClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		client = &http.Client{Timeout: base.Timeout, Transport: base.Transport, Jar: base.Jar}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= imageArchiveRedirects {
			return fmt.Errorf("stopped after %d redirects", imageArchiveRedirects)
		}
		return checkArchiveURL(req.URL.String())
	}
	return client
}

func archiveURLHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "the archive url"
	}
	return u.Host
}

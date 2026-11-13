package udocker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// ImageExists checks the udocker local image list.
func (d *Driver) ImageExists(ctx context.Context, image string) (bool, error) {
	if err := d.ensureProvisioned(ctx); err != nil {
		return false, err
	}
	cliImage, pinned := udockerCLIImage(image)
	if d.nativeImageComplete(image) {
		return true, nil
	}
	out, err := d.udo(ctx, "images")
	if err != nil {
		return false, err
	}
	if !udockerImageListed(out, cliImage) {
		return false, nil
	}
	if !pinned {
		return true, nil
	}
	return d.pinnedImageVerified(image)
}

// LoadImage imports a docker-save or OCI archive stream. The native loader
// consumes the stream directly and registers the image under exactly image.
// The udocker CLI path (native disabled) stages it first: although udocker
// documents stdin support, its tar subprocess can retain the pipe and block
// waiting for EOF, so the reliable -i file path is used in both user modes.
func (d *Driver) LoadImage(ctx context.Context, image string, r io.Reader) error {
	if r == nil {
		return fmt.Errorf("udocker load requires an image archive")
	}
	repository := imageRepository(image)
	if repository == "" {
		return fmt.Errorf("udocker load requires an image reference")
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	if engine := d.nativeEngine(); engine != nil {
		return engine.Load(ctx, image, r)
	}
	archive, err := os.CreateTemp(d.stateDir, "image-load-*.tar")
	if err != nil {
		return fmt.Errorf("stage udocker image archive: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	if _, err := io.Copy(archive, r); err != nil {
		_ = archive.Close()
		return fmt.Errorf("stage udocker image archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close udocker image archive: %w", err)
	}
	if err := os.Chmod(archivePath, 0o644); err != nil {
		return fmt.Errorf("make udocker image archive readable: %w", err)
	}
	return d.loadStagedArchive(ctx, repository, archivePath)
}

// LoadImageFile imports an archive that is already a complete file on disk.
// Unlike LoadImage it stages nothing: the agent downloaded the archive itself,
// so a second copy would mean three simultaneous copies of a multi-gigabyte
// tar on the same filesystem.
func (d *Driver) LoadImageFile(ctx context.Context, image, path string) error {
	repository := imageRepository(image)
	if repository == "" {
		return fmt.Errorf("udocker load requires an image reference")
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("udocker load requires an image archive path")
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	if engine := d.nativeEngine(); engine != nil {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open image archive: %w", err)
		}
		defer file.Close()
		return engine.Load(ctx, image, file)
	}
	return d.loadStagedArchive(ctx, repository, path)
}

// loadStagedArchive runs the one CLI invocation both load entry points share.
// The repository is always passed explicitly so an imported archive cannot
// acquire a generated name when a runtime handles its embedded tag differently.
func (d *Driver) loadStagedArchive(ctx context.Context, repository, archivePath string) error {
	// The CLI runs in processDir, not in the agent's cwd.
	if abs, err := filepath.Abs(archivePath); err == nil {
		archivePath = abs
	}
	if _, err := d.udo(ctx, "load", "-i="+archivePath, repository); err != nil {
		return fmt.Errorf("udocker load: %w", err)
	}
	return nil
}

func imageRepository(image string) string {
	image = strings.TrimSpace(image)
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	if colon, slash := strings.LastIndexByte(image, ':'), strings.LastIndexByte(image, '/'); colon > slash {
		image = image[:colon]
	}
	return strings.TrimSpace(image)
}

var (
	_ runtime.ImageLoader      = (*Driver)(nil)
	_ runtime.ImageFileLoader  = (*Driver)(nil)
	_ runtime.ImageArchiver    = (*Driver)(nil)
	_ runtime.LocalImageSource = (*Driver)(nil)
	_ runtime.ImageRemover     = (*Driver)(nil)
)

// HoldsImage reports whether the native image store holds image, the only
// images SaveImage can stream. It reads the store on disk and does not set
// udocker up, so a node that never used this runtime pays nothing for asking.
func (d *Driver) HoldsImage(_ context.Context, image string) (bool, error) {
	return d.nativeImageComplete(image), nil
}

// SaveImage writes image as an archive that Docker, nerdctl and another
// runveil node load, which makes this runtime a source for node-to-node image
// sync. Only natively stored images can be saved; one the udocker CLI
// registered has no manifest to stream from and must be pulled or loaded again.
func (d *Driver) SaveImage(ctx context.Context, image string, w io.Writer) error {
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	engine := d.nativeEngine()
	if engine == nil {
		return fmt.Errorf("saving images needs the native image store, which %s disables", disableNativeEnv)
	}
	ref := native.ParseRef(image)
	if !engine.Layout.HasImage(ref) {
		return fmt.Errorf("image %s is not in the native image store; pull or load it again to make it transferable", image)
	}
	return engine.Layout.SaveArchive(ctx, ref, w)
}

// RemoveImage deletes an image from udocker's local repository. A natively
// registered image is removed by its exact reference, with blob and base
// rootfs reclamation; one the udocker CLI registered goes through `rmi`, which
// keys images by repository name, so the tag is dropped the same way `images`
// lists them.
func (d *Driver) RemoveImage(ctx context.Context, image string) error {
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	if handled, err := d.nativeRemoveImage(image); handled {
		return err
	}
	repository := imageRepository(image)
	if repository == "" {
		return fmt.Errorf("udocker rmi requires an image reference")
	}
	if _, err := d.udo(ctx, "rmi", repository); err != nil {
		return fmt.Errorf("udocker rmi: %w", err)
	}
	return nil
}

// InspectImage exposes best-effort image metadata. udocker's image inspect
// output differs by release, so ID is populated when available and callers
// must treat every field as optional.
func (d *Driver) InspectImage(ctx context.Context, image string) (runtime.ImageInspection, error) {
	if err := d.ensureProvisioned(ctx); err != nil {
		return runtime.ImageInspection{}, err
	}
	cliImage, pinned := udockerCLIImage(image)
	if pinned {
		verified, err := d.pinnedImageVerified(image)
		if err != nil {
			return runtime.ImageInspection{}, err
		}
		if !verified {
			return runtime.ImageInspection{}, fmt.Errorf("udocker image %q has not been verified against its pinned digest", image)
		}
	}
	if inspection, ok := d.nativeInspect(image); ok {
		return inspection, nil
	}
	nativeID := d.nativeImageID(image)
	out, err := d.udo(ctx, "images", "--list", cliImage)
	if err != nil {
		if nativeID != "" {
			return runtime.ImageInspection{ID: nativeID}, nil
		}
		return runtime.ImageInspection{}, err
	}
	id := strings.TrimSpace(out)
	if nativeID != "" {
		id = nativeID
	}
	return runtime.ImageInspection{ID: id}, nil
}

// PullImage pulls with the native registry client. A native failure is the
// answer: the udocker CLI covers nothing native does not except schema1
// manifests (see artifacts/work/2026-09-24-runveil/research-phase2-3.md), so
// it is reachable only through GPUWORKS_UDOCKER_DISABLE_NATIVE, an explicit
// escape hatch, instead of silently retrying every failed pull through it.
func (d *Driver) PullImage(ctx context.Context, image string, onLine func(string)) error {
	if err := runtime.ValidatePullProxy(ctx); err != nil {
		return err
	}
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	if engine := d.nativeEngine(); engine != nil {
		if err := engine.Pull(ctx, image, onLine); err != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		// Digest references are registered under their immutable digest and
		// verified against it, so no separate verification marker is needed.
		return nil
	}
	return d.cliPull(ctx, image, onLine)
}

// cliPull is `udocker pull`, used only with the native pipeline disabled.
func (d *Driver) cliPull(ctx context.Context, image string, onLine func(string)) error {
	if onLine != nil && d.offline {
		onLine("udocker offline bundle detected; installation skipped")
	}
	if _, ok := runtime.RegistryAuthFor(ctx, image); ok {
		// The udocker CLI takes credentials only on its command line or in its
		// own configuration; neither is acceptable for a secret, so the pull
		// continues anonymously.
		slog.Debug("registry login not supported by the udocker CLI pull; pulling anonymously", "image", image)
	}
	cliImage, pinned := udockerCLIImage(image)
	var expected []manifestLayer
	if pinned {
		var err error
		expected, err = resolvePinnedDescriptors(ctx, image)
		if err != nil {
			return fmt.Errorf("resolve pinned udocker image %q: %w", image, err)
		}
	}
	slog.Info("runtime command", "runtime", "udocker", "args", []string{"pull", cliImage})
	cmd := d.cliPullCommand(ctx, cliImage)
	baselineBytes := directoryFileBytes(filepath.Join(d.udockerDir, "layers"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	progressCtx, stopProgress := context.WithCancel(ctx)
	defer stopProgress()
	lines := make(chan string, 32)
	var readers, progress sync.WaitGroup
	read := func(r io.Reader) {
		defer readers.Done()
		s := bufio.NewScanner(r)
		for s.Scan() {
			if line := strings.TrimSpace(s.Text()); line != "" && onLine != nil {
				lines <- line
			}
		}
	}
	readers.Add(2)
	go read(stdout)
	go read(stderr)
	progress.Add(1)
	go func() {
		defer progress.Done()
		if onLine != nil {
			d.reportStoreProgress(progressCtx, cliImage, baselineBytes, lines)
		}
	}()
	// Wait closes the pipes, so it may only run once both have been read to
	// EOF; calling it earlier drops the last lines of output.
	waitDone := make(chan error, 1)
	go func() {
		readers.Wait()
		waitDone <- cmd.Wait()
		stopProgress()
		progress.Wait()
		close(lines)
	}()
	for line := range lines {
		if onLine != nil {
			onLine(line)
		}
	}
	pullErr := <-waitDone
	if pullErr != nil {
		return pullErr
	}
	if pinned {
		if err := d.recordPinnedImageVerification(image, cliImage, expected); err != nil {
			return err
		}
	}
	return nil
}

// manifestLayer is one descriptor of a pinned manifest, as recorded in the
// verification marker a CLI pull of a digest reference leaves behind.
type manifestLayer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// resolvePinnedDescriptors resolves the manifest a digest reference names for
// this platform: the descriptors the tag udocker pulls must match.
func resolvePinnedDescriptors(ctx context.Context, image string) ([]manifestLayer, error) {
	resolved, err := native.NewClient().Resolve(ctx, native.ParseRef(image))
	if err != nil {
		return nil, err
	}
	out := []manifestLayer{{Digest: resolved.Manifest.Config.Digest, Size: resolved.Manifest.Config.Size}}
	for _, layer := range resolved.Manifest.Layers {
		out = append(out, manifestLayer{Digest: layer.Digest, Size: layer.Size})
	}
	return out, nil
}

type udockerManifest struct {
	Config struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"layers"`
}

type pinnedImageVerification struct {
	Version     int             `json:"version"`
	Image       string          `json:"image"`
	CLIImage    string          `json:"cli_image"`
	Descriptors []manifestLayer `json:"descriptors"`
}

// udockerCLIImage bridges an udocker CLI limitation without weakening the
// catalog contract. udocker 1.3 rejects repo:tag@sha256:... references, so the
// CLI receives repo:tag while GPUWorks verifies the resulting local manifest
// against the pinned digest before create/launch is allowed.
func udockerCLIImage(image string) (string, bool) {
	image = strings.TrimSpace(image)
	at := strings.LastIndexByte(image, '@')
	if at < 0 {
		return image, false
	}
	base := strings.TrimSpace(image[:at])
	digest := strings.TrimSpace(image[at+1:])
	if base == "" || !strings.HasPrefix(digest, "sha256:") {
		return image, false
	}
	return normalizeImage(base), true
}

func (d *Driver) recordPinnedImageVerification(image, cliImage string, expected []manifestLayer) error {
	manifest, ok := pullManifest(filepath.Join(d.udockerDir, "repos"), cliImage)
	if !ok {
		return fmt.Errorf("verify pinned udocker image %q: local manifest is missing", image)
	}
	actual := manifestDescriptors(manifest)
	if !sameManifestDescriptors(actual, expected) {
		return fmt.Errorf("verify pinned udocker image %q: pulled tag does not match pinned manifest", image)
	}
	marker := pinnedImageVerification{
		Version:     1,
		Image:       strings.TrimSpace(image),
		CLIImage:    cliImage,
		Descriptors: normalizedManifestDescriptors(expected),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode pinned udocker image verification: %w", err)
	}
	dir := filepath.Join(d.stateDir, "verified-images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create pinned udocker image verification directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".verification-*")
	if err != nil {
		return fmt.Errorf("stage pinned udocker image verification: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write pinned udocker image verification: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod pinned udocker image verification: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close pinned udocker image verification: %w", err)
	}
	if err := os.Rename(tmpName, d.pinnedImageVerificationPath(image)); err != nil {
		return fmt.Errorf("commit pinned udocker image verification: %w", err)
	}
	return nil
}

func (d *Driver) pinnedImageVerified(image string) (bool, error) {
	data, err := os.ReadFile(d.pinnedImageVerificationPath(image))
	if os.IsNotExist(err) {
		if d.nativeImageComplete(image) {
			return true, nil
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read pinned udocker image verification: %w", err)
	}
	var marker pinnedImageVerification
	if err := json.Unmarshal(data, &marker); err != nil {
		return false, nil //nolint:nilerr // A corrupt verification marker requires verification again, not a failed pull.
	}
	cliImage, pinned := udockerCLIImage(image)
	if !pinned || marker.Version != 1 || marker.Image != strings.TrimSpace(image) || marker.CLIImage != cliImage {
		return false, nil
	}
	manifest, ok := pullManifest(filepath.Join(d.udockerDir, "repos"), cliImage)
	if !ok {
		return false, nil
	}
	return sameManifestDescriptors(manifestDescriptors(manifest), marker.Descriptors), nil
}

func (d *Driver) pinnedImageVerificationPath(image string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(image)))
	return filepath.Join(d.stateDir, "verified-images", fmt.Sprintf("%x.json", sum))
}

func manifestDescriptors(manifest udockerManifest) []manifestLayer {
	descriptors := make([]manifestLayer, 0, len(manifest.Layers)+1)
	for _, layer := range manifest.Layers {
		descriptors = append(descriptors, manifestLayer{Digest: layer.Digest, Size: layer.Size})
	}
	if manifest.Config.Digest != "" {
		descriptors = append(descriptors, manifestLayer{Digest: manifest.Config.Digest, Size: manifest.Config.Size})
	}
	return descriptors
}

func sameManifestDescriptors(a, b []manifestLayer) bool {
	a = normalizedManifestDescriptors(a)
	b = normalizedManifestDescriptors(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) > 0
}

func normalizedManifestDescriptors(in []manifestLayer) []manifestLayer {
	out := append([]manifestLayer(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Digest == out[j].Digest {
			return out[i].Size < out[j].Size
		}
		return out[i].Digest < out[j].Digest
	})
	return out
}

func pullManifest(root, image string) (udockerManifest, bool) {
	repository, tag := splitImageRef(image)
	if repository == "" || tag == "" {
		return udockerManifest{}, false
	}
	path := filepath.Join(root, filepath.FromSlash(repository), tag, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		return udockerManifest{}, false
	}
	var manifest udockerManifest
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Layers) == 0 {
		return udockerManifest{}, false
	}
	return manifest, true
}

// splitImageRef names the directory udocker keeps a pulled tag under:
// repos/<repository>/<tag>, where repository is the reference as given to
// `udocker pull`, registry host included (udocker localrepo.setup_imagerepo
// uses it verbatim; "quay.io/a/b:t" lives in repos/quay.io/a/b/t, "a/b:t" in
// repos/a/b/t).
func splitImageRef(image string) (repository, tag string) {
	image = strings.TrimSpace(image)
	if image == "" || strings.Contains(image, "@") {
		return "", ""
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:]
	}
	return image, "latest"
}

// TagImage records source under canonical as well. Like every other entry
// point it provisions first: the native engine fixes its file owner (the run
// user) when it is first built, so building it earlier would leave every
// file it writes owned by the agent instead of the user workloads run as.
func (d *Driver) TagImage(ctx context.Context, source, canonical string) error {
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	if handled, err := d.nativeTagImage(source, canonical); handled {
		return err
	}
	_, err := d.udo(ctx, "tag", source, canonical)
	return err
}

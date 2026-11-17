package native

import (
	"gpuworks.ai/gpuworks/internal/agent/connbudget"
	"gpuworks.ai/gpuworks/internal/agent/runtime"

	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
)

// unpackDebugEnv turns on per-layer unpack timings.
const unpackDebugEnv = "GPUWORKS_UDOCKER_UNPACK_DEBUG"

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Engine performs pulls and container creation for one UDOCKER_DIR.
type Engine struct {
	Layout     Layout
	Client     *Client
	Workers    int   // concurrent layer downloads
	RangeParts int   // range parts for one large blob
	RangeMin   int64 // blobs at least this large are split into parts
	Unpackers  int   // concurrent layer unpackers (0 = one per core)

	// Budget is the per-registry-host connection budget every pull on this
	// engine shares (see Downloader.Budget).
	Budget *connbudget.Hosts

	// Sidecars holds plain-tar copies of large layers, inflated during the
	// pull so the create that follows does not have to. Nil disables it.
	Sidecars *sidecarStore

	// Reflink overrides the reflink probe that decides whether a base rootfs
	// cache is kept (see basecache.go). Nil probes UDOCKER_DIR.
	Reflink func(dir string) bool
	// FreeSpace reports the free bytes at a path (-1 when unknown); nil asks
	// the filesystem. Tests stand in a full disk with it.
	FreeSpace func(path string) int64
	base      baseCache
	prepared  preparedJobs
}

// Defaults sized for the nodes this driver targets: enough layers in flight to
// keep a throttled link busy, and a range split that only kicks in for the
// multi-gigabyte layers where a single stream is the limit. How many
// connections that really opens is the connection budget's call (see
// DefaultMaxConnections), so neither number multiplies into a connection count.
const (
	DefaultWorkers    = 6
	DefaultRangeParts = 4
	DefaultRangeMin   = 256 << 20

	// Only layers this large are worth inflating ahead of time; smaller ones
	// are a rounding error next to the multi-gigabyte layer that dominates.
	defaultSidecarMin = 512 << 20
	// Free space to leave untouched. Nodes run engines and download models on
	// the same filesystem, so the cache must never be the reason a pull or a
	// model download runs the disk out.
	defaultSidecarReserve = 20 << 30
	// Ceiling on the cache itself, roughly two engine images' large layers
	// inflated. Past that the oldest entries are reclaimed, so the cache stays
	// bounded on a node that pulls many images over its lifetime.
	defaultSidecarMaxTotal = 48 << 30
)

// disableSidecarEnv turns off inflating large layers during the pull.
const disableSidecarEnv = "GPUWORKS_UDOCKER_DISABLE_TAR_CACHE"

// NewEngine builds an Engine for a UDOCKER_DIR, optionally handing everything
// it writes to a dedicated uid/gid.
func NewEngine(udockerDir string, owner *Owner) *Engine {
	engine := &Engine{
		Layout:     Layout{Dir: udockerDir, Owner: owner},
		Client:     NewClient(),
		Workers:    DefaultWorkers,
		RangeParts: DefaultRangeParts,
		RangeMin:   DefaultRangeMin,
		Budget:     connbudget.NewHosts(MaxConnectionsFromEnv()),
	}
	if !envTrue(disableSidecarEnv) {
		// Kept outside layers/ so nothing here can be mistaken for a blob by
		// udocker's own repository bookkeeping.
		engine.Sidecars = &sidecarStore{
			Dir:      filepath.Join(udockerDir, "tarcache"),
			Owner:    owner,
			MinSize:  defaultSidecarMin,
			Reserve:  defaultSidecarReserve,
			MaxTotal: defaultSidecarMaxTotal,
		}
	}
	return engine
}

func (e *Engine) downloader() *Downloader {
	return &Downloader{
		Client:     e.Client,
		Dir:        e.Layout.LayersDir(),
		Workers:    e.Workers,
		RangeParts: e.RangeParts,
		RangeMin:   e.RangeMin,
		Budget:     e.Budget,
		Chown:      e.Layout.Owner,
	}
}

// HasImage reports whether image is registered locally with all of its blobs.
func (e *Engine) HasImage(image string) bool {
	return e.Layout.HasImage(ParseRef(image))
}

// Pull resolves image, downloads every missing blob concurrently, and
// registers the repository metadata. It writes the tag directory only after
// all content has been verified, so an interrupted pull is never mistaken for
// a complete one. A registry refusal and a full disk come back marked with
// runtime.ErrRegistryRefused and runtime.ErrNoSpace.
func (e *Engine) Pull(ctx context.Context, image string, onLine func(string)) error {
	return pullFailure(e.pull(ctx, image, onLine))
}

// pullFailure marks the failures the image puller acts on: the registry
// answering 401/403/404, and the node's disk filling up.
func pullFailure(err error) error {
	var status *statusError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errUnpackSpace) || runtime.DiskFull(err):
		return runtime.MarkPullFailure(err, runtime.ErrNoSpace)
	case errors.As(err, &status) && (status.status == http.StatusUnauthorized || status.status == http.StatusForbidden || status.status == http.StatusNotFound):
		return runtime.MarkPullFailure(err, runtime.ErrRegistryRefused)
	}
	return err
}

func (e *Engine) pull(ctx context.Context, image string, onLine func(string)) error {
	ref := ParseRef(image)
	resolved, err := e.Client.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	descs := append([]Descriptor{resolved.Manifest.Config}, resolved.Manifest.Layers...)
	reporter := newByteReporter(descs, onLine, runtime.PullLineDownloading)
	reporter.start()
	downloader := e.downloader()

	// A pull is network bound, so inflate the biggest layers into plain tars
	// with the otherwise idle cores while their bytes are still arriving. The
	// create that follows then only has to write files, which is the half of
	// its work that can actually use several cores.
	if e.Sidecars != nil {
		e.Sidecars.sweep(e.Layout.LayersDir())
		planned := e.Sidecars.Plan(resolved.Manifest.Layers)
		if len(planned) > 0 {
			wanted := make(map[string]bool, len(planned))
			var total int64
			for _, desc := range planned {
				wanted[desc.Digest] = true
				total += desc.Size
			}
			report(onLine, fmt.Sprintf("decompressing %d large layer(s) (%s) during download for a faster container start",
				len(planned), humanBytes(total)))
			downloader.StreamWant = func(desc Descriptor) bool { return wanted[desc.Digest] }
			downloader.Stream = func(streamCtx context.Context, desc Descriptor, r io.Reader) (func(bool), error) {
				return e.Sidecars.Write(streamCtx, desc, r)
			}
		}
	}
	var abortPrepare func()
	if runtime.LaunchIntent(ctx) {
		// A container is created from this image next: unpack each layer as
		// soon as its blob is verified instead of after the whole pull.
		downloader.Fetched, abortPrepare = e.startPrepare(resolved, digestOf(resolved.ManifestJSON))
	}
	if err := downloader.FetchAll(ctx, ref, descs, reporter.add, reporter.restart); err != nil {
		if abortPrepare != nil {
			abortPrepare()
		}
		return err
	}
	reporter.finish()
	if err := e.Layout.RegisterImage(resolved); err != nil {
		if abortPrepare != nil {
			abortPrepare()
		}
		return err
	}
	report(onLine, fmt.Sprintf("pulled %d layer(s) (%s) natively", len(resolved.Manifest.Layers), humanBytes(reporter.total)))
	return nil
}

// Create extracts a registered image into a new container directory. It is the
// native replacement for `udocker create`: one pass per layer, all layers at
// once, decompressing straight into ROOT instead of writing an intermediate
// tar and then reading it twice.
func (e *Engine) Create(ctx context.Context, name, image string, onLine func(string)) error {
	ref := ParseRef(image)
	manifest, err := e.Layout.LoadManifest(ref)
	if err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(e.Layout.LayersDir(), manifest.Config.Digest))
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	dir := e.Layout.ContainerDir(name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("container %s already exists", name)
	}
	// Build under a temporary name so a failed or interrupted create never
	// leaves a half-populated ROOT that udocker would accept as a container.
	staging := dir + CreateStagingSuffix
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(staging)
		}
	}()

	layers := make([]LayerSource, 0, len(manifest.Layers))
	effective := make([]Descriptor, 0, len(manifest.Layers))
	store := e.Layout.LayersDir()
	cached := 0
	for _, desc := range manifest.Layers {
		path := filepath.Join(store, desc.Digest)
		if info, err := os.Stat(path); err != nil || info.Size() != desc.Size {
			return fmt.Errorf("layer %s missing from the local store", shortDigest(desc.Digest))
		}
		size := desc.Size
		// A sidecar written during the pull holds this layer already inflated,
		// so reading it skips the single-threaded gzip decode entirely. The
		// unpacker detects the plain tar from its magic bytes.
		sidecar := ""
		if candidate, ok := e.Sidecars.Get(desc); ok {
			if info, err := os.Stat(candidate); err == nil {
				sidecar, size = candidate, info.Size()
				cached++
			}
		}
		blob := path
		layers = append(layers, LayerSource{
			Digest: desc.Digest,
			Size:   size,
			// The sidecar is only ever an optimization, so a missing or
			// unreadable one must degrade to the original blob rather than
			// fail the create: it can be swept between this stat and the open,
			// and losing a cache entry is not a reason to fail a deployment.
			Open: func(context.Context) (io.ReadCloser, error) {
				if sidecar != "" {
					if file, err := os.Open(sidecar); err == nil {
						return file, nil
					}
				}
				return os.Open(blob)
			},
		})
		effective = append(effective, Descriptor{Digest: desc.Digest, Size: size})
	}
	if cached > 0 {
		report(onLine, fmt.Sprintf("using %d pre-decompressed layer(s)", cached))
	}

	reporter := newByteReporter(effective, onLine, runtime.PullLineExtracting)
	root := filepath.Join(staging, "ROOT")
	manifestID, _ := e.Layout.ImageID(ref)
	baseRoot := ""
	if manifestID != "" && e.reflinkSupported() {
		baseRoot = filepath.Join(e.Layout.baseDir(manifestID), "ROOT")
	}
	fromBase := baseRoot != "" && isCompleteBase(baseRoot)
	if fromBase {
		if err := cloneRoot(ctx, baseRoot, root); err != nil {
			return fmt.Errorf("clone cached base rootfs: %w", err)
		}
		report(onLine, "reused immutable base rootfs")
	}
	prepared := !fromBase && manifestID != "" && e.claimPrepared(ctx, manifestID, root, onLine)
	unpacker := &Unpacker{Root: root, Workers: e.Unpackers, Owner: e.Layout.Owner, Progress: reporter.add}
	if envTrue(unpackDebugEnv) {
		unpacker.OnLayerDone = func(digest string, elapsed time.Duration, size int64) {
			reporter.say(fmt.Sprintf("unpack timing: %s took %s (%s compressed)",
				shortDigest(digest), elapsed.Round(100*time.Millisecond), humanBytes(size)))
		}
	}
	started := time.Now()
	if !fromBase && !prepared {
		if err := e.checkUnpackSpace(filepath.Dir(staging), effective); err != nil {
			return err
		}
		if err := unpacker.Unpack(ctx, layers); err != nil {
			return err
		}
		// Byte progress only describes an unpack; a clone or a claimed
		// prepared tree extracted nothing here.
		reporter.finish()
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	success = true
	if err := e.Layout.RegisterContainer(name, ref, config); err != nil {
		return err
	}
	if !fromBase {
		e.scheduleBase(manifestID, image, layers, onLine)
	}
	switch {
	case fromBase:
		report(onLine, fmt.Sprintf("prepared container from cached base in %s", time.Since(started).Round(time.Second)))
	case prepared:
	default:
		report(onLine, fmt.Sprintf("unpacked %d layer(s) in %s using %d workers",
			len(layers), time.Since(started).Round(time.Second), unpackWorkers(e.Unpackers, len(layers))))
	}
	return nil
}

func isCompleteBase(root string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(root), ".complete"))
	if err != nil {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

func cloneRoot(ctx context.Context, source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	slog.Info("runtime command", "runtime", "udocker", "args", []string{"cp", "-a", "--reflink=auto", source, target})
	if err := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", source, target).Run(); err == nil {
		return nil
	}
	// BSD/macOS cp has no --reflink flag; preserve correctness there while
	// retaining reflink acceleration on Linux filesystems that support it.
	slog.Info("runtime command", "runtime", "udocker", "args", []string{"cp", "-a", source, target})
	return exec.CommandContext(ctx, "cp", "-a", source, target).Run()
}

func unpackWorkers(configured, layers int) int {
	if configured > 0 {
		return configured
	}
	if layers < goruntime.NumCPU() {
		return layers
	}
	return goruntime.NumCPU()
}

func report(onLine func(string), line string) {
	if onLine != nil {
		onLine(line)
	}
}

// byteReporter turns concurrent per-layer byte counts into the shared pull
// lines (runtime.PullLineDownloading and friends), so a native pull shows the
// same live counters an udocker pull did.
type byteReporter struct {
	mu sync.Mutex
	// out serializes onLine: downloads and unpack workers report from many
	// goroutines, and the callers' callbacks (pull progress parsing, instance
	// state reports) are not written for concurrent use.
	out    sync.Mutex
	onLine func(string)
	line   func(digest string, done, total int64) string
	size   map[string]int64
	done   map[string]int64
	last   map[string]time.Time
	total  int64
}

func newByteReporter(descs []Descriptor, onLine func(string), line func(digest string, done, total int64) string) *byteReporter {
	r := &byteReporter{
		onLine: onLine,
		line:   line,
		size:   make(map[string]int64, len(descs)),
		done:   make(map[string]int64, len(descs)),
		last:   make(map[string]time.Time, len(descs)),
	}
	for _, desc := range descs {
		r.size[desc.Digest] = desc.Size
		r.total += desc.Size
	}
	return r
}

func (r *byteReporter) start() {
	if r.onLine == nil {
		return
	}
	for digest := range r.size {
		r.emit(digest, true)
	}
}

func (r *byteReporter) add(digest string, n int64) {
	if r.onLine == nil || n <= 0 {
		return
	}
	r.mu.Lock()
	r.done[digest] += n
	if limit := r.size[digest]; limit > 0 && r.done[digest] > limit {
		r.done[digest] = limit
	}
	due := time.Since(r.last[digest]) >= time.Second
	r.mu.Unlock()
	if due {
		r.emit(digest, false)
	}
}

func (r *byteReporter) restart(digest string) {
	if r.onLine == nil {
		return
	}
	r.mu.Lock()
	r.done[digest] = 0
	r.mu.Unlock()
	r.emit(digest, true)
}

func (r *byteReporter) finish() {
	if r.onLine == nil {
		return
	}
	for digest := range r.size {
		r.mu.Lock()
		r.done[digest] = r.size[digest]
		r.mu.Unlock()
		r.emit(digest, true)
	}
}

// say reports a free-form line from any goroutine, serialized with emit.
func (r *byteReporter) say(line string) {
	if r.onLine == nil {
		return
	}
	r.out.Lock()
	defer r.out.Unlock()
	r.onLine(line)
}

func (r *byteReporter) emit(digest string, force bool) {
	if r.onLine == nil {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if !force && now.Sub(r.last[digest]) < time.Second {
		r.mu.Unlock()
		return
	}
	r.last[digest] = now
	line := r.line(digest, r.done[digest], r.size[digest])
	r.mu.Unlock()
	r.out.Lock()
	defer r.out.Unlock()
	r.onLine(line)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp]), ".0")
}

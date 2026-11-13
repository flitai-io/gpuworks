package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/agent/modelfetch"
	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

const modelCompleteMarker = modelscan.CompletionMarker

type modelCompletion struct {
	Version   int              `json:"version"`
	Source    string           `json:"source"`
	SourceID  string           `json:"source_id"`
	Revision  string           `json:"revision"`
	Subfolder string           `json:"subfolder,omitempty"`
	Files     []modelrepo.File `json:"files"`
	Bytes     int64            `json:"bytes"`
	Created   time.Time        `json:"created_at"`
}

// modelDownload is a running download of one model file task. While it is
// registered it is the only writer of its target directory: eviction cancels
// it and waits for done before it removes anything, so a transfer can never
// outlive the delete and write into, or recreate, the evicted directory.
type modelDownload struct {
	transferID string
	copy       bool
	target     string
	cancel     context.CancelFunc
	done       chan struct{}
}

// reconcileModelFile ensures a model repo is present at its target path,
// downloading it (all repo files) in the background if needed and reporting
// progress. Idempotent: a repo already present short-circuits to "present".
// A task from the server is the newest word on its key, so whatever was parked
// for the key is replaced by it.
func (n *NodeRuntime) reconcileModelFile(ctx context.Context, mf proto.ModelFileTask) {
	if mf.DesiredState == "present" && n.downloadTaskStopped(mf) {
		return
	}
	if mf.DesiredState == "paused" {
		n.dropParkedTransfer(mf.Key, mf.TransferID)
	} else {
		n.dropWaiting(mf.Key)
	}
	n.reconcileModelFileTask(ctx, mf, false)
}

// reconcileModelFileTask is reconcileModelFile for a task that is not already
// replacing the parked one: retry marks a task run from a queue snapshot, which
// acts only while it is still the task parked for its key.
func (n *NodeRuntime) reconcileModelFileTask(ctx context.Context, mf proto.ModelFileTask, retry bool) {
	fail := func(err error) {
		if n.settleParkedModelFile(mf, retry) {
			n.failModelFile(mf, err)
		}
	}
	if mf.DesiredState != "present" {
		// Withdrawn: a task parked for its storage must not run when the
		// storage recovers, and the transfer that was writing for it stops.
		n.dlMu.Lock()
		if mf.DesiredState == "paused" && mf.TransferID != "" {
			n.fenceDownloadLocked(mf.Key, mf.TransferID)
		}
		if active := n.downloading[mf.Key]; active != nil && (mf.TransferID == "" || mf.TransferID == active.transferID) {
			active.cancel()
		}
		n.dlMu.Unlock()
		n.progress.discard(modelFileProgressKey(mf.Key))
		return
	}
	// Resolve the (possibly relative) target path to an absolute one under the
	// task's storage root — docker bind mounts require absolute paths. A task
	// for a storage that is not usable here yet waits for the next storage
	// check instead of writing into a bare mount point.
	root, ready, err := n.storageRoot(mf)
	if err != nil {
		fail(err)
		return
	}
	if !ready {
		n.waitForStorage(mf, retry)
		return
	}
	absTarget, err := resolveStorageTarget(root, taskTarget(mf))
	if err != nil {
		fail(err)
		return
	}
	// A write into a shared storage is assembled in a staging directory and
	// renamed into place, so other nodes never see a half-written model.
	workDir := absTarget
	if strings.TrimSpace(mf.Staging) != "" {
		if workDir, err = stagingWorkDir(root, mf.Staging); err != nil {
			fail(err)
			return
		}
	}

	if n.copyTaskRunning(mf, retry) {
		return
	}
	// Public downloads are trusted only with a completion marker whose entire
	// file manifest still matches disk. Peer transfers are installed atomically,
	// and registered paths are an explicit user assertion, so those may use the
	// local structural validation while still reporting their real byte size.
	// The marker lives on the storage, which may be a hung NFS mount: it is read
	// outside dlMu, which every download and eviction on the node takes.
	marker, size, complete, err := completionOnDiskForTask(n, mf, absTarget)
	if err != nil {
		n.log.Debug("model file check postponed: the storage did not answer", "key", mf.Key, "path", absTarget, "err", err)
		n.parkStorageModelFile(mf, retry) // the next storage check retries it; an immediate retry would only spin
		return
	}
	if complete {
		if !n.settleParkedModelFile(mf, retry) {
			return
		}
		n.reportModelFile(proto.ModelFileStatus{
			Key: mf.Key, State: proto.MFPresent, Path: absTarget,
			BytesTotal: size, BytesDone: size,
			Revision:    manifestRevision(marker.Files),
			Acquisition: mf.Acquisition, TransferID: mf.TransferID,
		})
		return
	}
	n.dlMu.Lock()
	if retry && !n.modelFileParked(mf) { // evicted or replaced while the marker was read
		n.dlMu.Unlock()
		return
	}
	if n.downloading[mf.Key] != nil { // started while the marker was read
		n.dlMu.Unlock()
		n.settleParkedModelFile(mf, retry)
		return
	}
	copyWriter := mf.Acquisition == proto.ModelAcquireCopy
	if (copyWriter || mf.Acquisition == proto.ModelAcquireDownload) && !n.writerSlotAvailableLocked(mf.Key, absTarget, copyWriter) {
		parkWriterModelFileForTask(n, mf, retry)
		n.dlMu.Unlock()
		return
	}
	if copyWriter || mf.Acquisition == proto.ModelAcquireDownload {
		// Claiming the queue entry and registering the writer happen under the
		// same dlMu hold that checked the slot, so an owner returning cannot
		// miss this task and eviction cannot slip between the check and the start.
		if !n.settleParkedModelFile(mf, retry) {
			n.dlMu.Unlock()
			return
		}
		if copyWriter {
			n.startTransferLocked(ctx, mf.Key, absTarget, true, mf.TransferID, func(ctx context.Context) {
				n.copyModelDir(ctx, mf, workDir, absTarget)
			})
			return
		}
		n.startTransferLocked(ctx, mf.Key, absTarget, false, mf.TransferID, func(ctx context.Context) {
			n.downloadRepo(ctx, mf, workDir, absTarget)
		})
		return
	}
	n.dlMu.Unlock()
	if !n.settleParkedModelFile(mf, retry) {
		return
	}
	trustedAtomicPeer := mf.Acquisition == proto.ModelAcquirePeer && mf.TransferID != ""
	if mf.Acquisition == proto.ModelAcquireRegistered || trustedAtomicPeer {
		if size, err := validateModelDir(absTarget); err == nil {
			n.reportModelFile(proto.ModelFileStatus{
				Key: mf.Key, State: proto.MFPresent, Path: absTarget,
				BytesTotal: size, BytesDone: size,
				Acquisition: mf.Acquisition, TransferID: mf.TransferID,
			})
		} else if mf.Acquisition == proto.ModelAcquireRegistered && os.IsNotExist(err) {
			n.reportModelFile(proto.ModelFileStatus{
				Key: mf.Key, State: proto.MFAbsent, Path: absTarget,
				Acquisition: proto.ModelAcquireRegistered,
				Message:     "registered model path is absent: " + err.Error(),
			})
		}
	}
}

// startTransferLocked registers a transfer that writes target under key in
// n.downloading and runs it in the background. transferID is the server's name
// for the transfer (empty from a server that predates download IDs); a copy is
// told apart from a download because only a copy is canceled by a task naming
// another transfer ID without a new download preparation. When run returns, the
// goroutine unregisters it and retries writer-waiting tasks after releasing
// dlMu. Eviction cancels it through the registered cancel. The caller holds
// n.dlMu and has checked that nothing is registered under key;
// startTransferLocked releases it.
func (n *NodeRuntime) startTransferLocked(ctx context.Context, key, target string, copyWriter bool, transferID string, run func(ctx context.Context)) {
	runCtx, cancel := context.WithCancel(ctx)
	active := &modelDownload{target: target, cancel: cancel, done: make(chan struct{}), copy: copyWriter, transferID: transferID}
	n.downloading[key] = active
	n.dlMu.Unlock()
	go func() {
		defer close(active.done)
		defer func() {
			if runCtx.Err() != nil {
				n.progress.discard(modelFileProgressKey(key))
			}
			n.dlMu.Lock()
			removed := false
			if n.downloading[key] == active {
				delete(n.downloading, key)
				removed = true
			}
			n.dlMu.Unlock()
			if removed {
				n.wakeModelWriters(ctx)
			}
		}()
		defer cancel()
		run(runCtx)
	}()
}

// downloadRepo reports nothing once ctx is canceled: whoever canceled it (agent
// shutdown, or an eviction that has already removed the files) owns the
// outcome, and a late "failed" or "present" would land on whatever the server
// recorded for this model afterwards.
func (n *NodeRuntime) downloadRepo(ctx context.Context, mf proto.ModelFileTask, absTarget, finalTarget string) {
	report := func(st proto.ModelFileStatus) {
		if ctx.Err() == nil {
			// The server names every download it prepares; echoing the name is
			// what lets it drop the reports of a download it has since replaced.
			st.TransferID = mf.TransferID
			n.reportModelFileContext(ctx, st)
		}
	}
	report(proto.ModelFileStatus{Key: mf.Key, State: proto.MFDownloading, Path: absTarget, Acquisition: proto.ModelAcquireDownload})
	fail := func(message, revision string, total int64) {
		state := proto.MFFailed
		if isModelDiskSpaceFailure(message) {
			state = proto.MFInsufficientDisk
		}
		report(proto.ModelFileStatus{
			Key: mf.Key, State: state, Path: absTarget, Revision: revision,
			BytesTotal: total, BytesDone: modelRepoBytesDone(absTarget),
			Message: message, Acquisition: proto.ModelAcquireDownload,
		})
	}

	net := n.network()
	// Where the files come from, as the console shows it while they arrive:
	// the address the fetch is at, and why it left the one before.
	var from atomic.Value
	from.Store("")
	say := func(message string) {
		from.Store(message)
		report(proto.ModelFileStatus{Key: mf.Key, State: proto.MFDownloading, Path: absTarget, Acquisition: proto.ModelAcquireDownload, Message: message})
	}
	var cooler modelfetch.Cooler
	if n.registries != nil {
		cooler = n.registries
	}
	fetcher, err := modelfetch.New(modelfetch.Config{
		Source: mf.Source, Repo: mf.SourceID, Candidates: fetchCandidates(mf, net),
		Client: func(base string) (*http.Client, error) {
			return modelSourceClient(mf.Source, base, net)
		},
		Cooler: cooler,
		Logins: n.modelLogins(),
		OnSwitch: func(s modelfetch.Switch) {
			n.log.Warn("model download moved to the next source", "key", mf.Key, "from", s.From, "to", s.To, "reason", s.Reason)
			say(switchedMessage(s))
		},
		OnRetry: func(file string, attempt int, delay time.Duration, err error) {
			n.log.Warn("model file download interrupted; retrying",
				"file", file, "attempt", attempt, "retry_in", delay.String(), "err", err)
		},
	})
	if err != nil {
		fail("model source: "+err.Error(), "", 0)
		return
	}
	say(downloadingFrom(fetcher.Host()))
	files, err := fetcher.List(ctx, mf.Revision)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fail(err.Error(), "", 0)
		return
	}
	files, err = selectRepoSubfolder(files, mf.Subfolder)
	if err != nil {
		fail("select repository subfolder: "+err.Error(), "", 0)
		return
	}
	pinnedRevision := manifestRevision(files)
	total, err := validateRepoManifest(files)
	if err != nil {
		fail("invalid repository manifest: "+err.Error(), "", 0)
		return
	}
	// A source whose listing prints sizes for people cannot be held to them:
	// disk admission, completeness and progress all have to allow for the
	// rounding until the transfer reports each file's real length.
	approximate := modelrepo.SizeIsApproximate(mf.Source)
	removeStaleRepoSidecars(n.log, mf.Key, absTarget, files, approximate)
	// A legacy cache may predate completion markers. Validate every repository
	// file and backfill the marker without downloading the model again.
	if present, _ := repoFilesPresent(absTarget, files, approximate); present {
		if _, err := validateModelDir(absTarget); err == nil {
			measured, err := writeCompletionMarker(absTarget, mf, files)
			if err != nil {
				fail("write completion marker: "+err.Error(), pinnedRevision, total)
				return
			}
			if err := installStagedModel(absTarget, finalTarget, mf); err != nil {
				fail(err.Error(), pinnedRevision, total)
				return
			}
			report(proto.ModelFileStatus{
				Key: mf.Key, State: proto.MFPresent, BytesTotal: measured, BytesDone: measured,
				Path: finalTarget, Revision: pinnedRevision, Acquisition: proto.ModelAcquireDownload,
			})
			return
		}
	}
	remaining := total
	for _, file := range files {
		dest, destErr := modelRepoDestination(absTarget, file.Path)
		if destErr != nil {
			continue
		}
		if _, complete := repoFileMeasured(dest, file.Size, approximate); complete {
			remaining -= file.Size
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	if err := n.checkDownloadSpace(mf.StorageID, absTarget, admissionBytes(remaining, approximate)); err != nil {
		fail(err.Error(), pinnedRevision, total)
		return
	}

	estimate := newRepoSizeEstimate(files, total)
	var done atomic.Int64 // completed-file bytes
	for index, f := range files {
		dest, err := modelRepoDestination(absTarget, f.Path)
		if err != nil {
			fail(err.Error(), pinnedRevision, estimate.Total())
			return
		}
		if size, complete := repoFileMeasured(dest, f.Size, approximate); complete {
			estimate.correct(index, size)
			done.Add(size)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			fail(err.Error(), pinnedRevision, estimate.Total())
			return
		}
		downloadRevision := mf.Revision
		if mf.Source == modelrepo.SourceMS && strings.TrimSpace(f.Revision) != "" {
			downloadRevision = f.Revision
		}
		base := done.Load()
		err = fetcher.Fetch(ctx, modelfetch.File{
			Path: f.Path, Revision: downloadRevision, Dest: dest,
			Concurrency: 4, RateLimitKBs: mf.RateLimitKBs,
		}, func(p download.Progress) {
			// The response carries this file's real length, which replaces the
			// listing's estimate for it as soon as the transfer starts. A
			// listing that already stated the size keeps its own number, so a
			// transfer that disagrees with it is still caught as a corrupt
			// download rather than redefining what was expected.
			if approximate {
				estimate.correct(index, p.Total)
			}
			report(proto.ModelFileStatus{
				Key: mf.Key, State: proto.MFDownloading,
				Revision:   pinnedRevision,
				BytesTotal: estimate.Total(), BytesDone: base + p.Done,
				RateBytesPerS: p.RateBytesPerS, Path: absTarget,
				Acquisition: proto.ModelAcquireDownload, Message: from.Load().(string),
			})
		})
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			fail(err.Error(), pinnedRevision, estimate.Total())
			return
		}
		// Only a listing that could not state the size gets corrected from
		// disk. A repository API states exact sizes, and a downloaded file
		// that does not match one is a corrupt transfer the verification below
		// must still catch — not a number to quietly adopt.
		if approximate {
			if size, complete := repoFileMeasured(dest, f.Size, true); complete {
				estimate.correct(index, size)
				done.Add(size)
				continue
			}
		}
		done.Add(f.Size)
	}
	manifest := estimate.Manifest()
	if present, message := repoFilesPresent(absTarget, manifest, false); !present {
		fail("download verification failed: "+message, pinnedRevision, estimate.Total())
		return
	}
	if _, err := validateModelDir(absTarget); err != nil {
		fail("downloaded repository is not a model: "+err.Error(), pinnedRevision, estimate.Total())
		return
	}
	measured, err := writeCompletionMarker(absTarget, mf, manifest)
	if err != nil {
		fail("write completion marker: "+err.Error(), pinnedRevision, estimate.Total())
		return
	}
	if err := installStagedModel(absTarget, finalTarget, mf); err != nil {
		fail(err.Error(), pinnedRevision, estimate.Total())
		return
	}
	report(proto.ModelFileStatus{
		Key: mf.Key, State: proto.MFPresent,
		BytesTotal: measured, BytesDone: measured, Path: finalTarget,
		Revision:    pinnedRevision,
		Acquisition: proto.ModelAcquireDownload,
	})
}

func isModelDiskSpaceFailure(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "insufficient disk space") || strings.Contains(message, "no space left on device")
}

// selectRepoSubfolder selects one immutable repository subtree while preserving
// every repository-relative path. Engines still receive the normal repository
// root and remain responsible for interpreting its internal layout.
func selectRepoSubfolder(files []modelrepo.File, subfolder string) ([]modelrepo.File, error) {
	subfolder, err := modelrepo.NormalizeSubfolder(subfolder)
	if err != nil {
		return nil, err
	}
	if subfolder == "" {
		return files, nil
	}
	selected := modelrepo.FilesUnderSubfolder(files, subfolder)
	if len(selected) == 0 {
		return nil, fmt.Errorf("repository contains no files under %q", subfolder)
	}
	return selected, nil
}

func normalizedRevision(source, revision string) string {
	if strings.TrimSpace(revision) == "" && source != modelrepo.SourceMS {
		return "main"
	}
	return strings.TrimSpace(revision)
}

func validateRepoManifest(files []modelrepo.File) (int64, error) {
	if len(files) == 0 {
		return 0, fmt.Errorf("repository contains no files")
	}
	seen := make(map[string]struct{}, len(files))
	var total int64
	pinnedRevision := ""
	for _, file := range files {
		clean := filepath.Clean(file.Path)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return 0, fmt.Errorf("unsafe file path %q", file.Path)
		}
		if clean == modelCompleteMarker {
			return 0, fmt.Errorf("repository file conflicts with reserved marker %q", modelCompleteMarker)
		}
		if file.Size < 0 {
			return 0, fmt.Errorf("file %q has a negative size", file.Path)
		}
		if _, ok := seen[clean]; ok {
			return 0, fmt.Errorf("duplicate file path %q", file.Path)
		}
		seen[clean] = struct{}{}
		if revision := strings.TrimSpace(file.Revision); revision != "" {
			if pinnedRevision != "" && pinnedRevision != revision {
				return 0, fmt.Errorf("repository files resolve to multiple revisions %q and %q", pinnedRevision, revision)
			}
			pinnedRevision = revision
		}
		total += file.Size
		if total < 0 {
			return 0, fmt.Errorf("repository size overflow")
		}
	}
	return total, nil
}

func modelRepoDestination(root, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe repository file path %q", name)
	}
	return filepath.Join(root, clean), nil
}

// modelRepoBytesDone reports bytes already materialized in a repository. It
// intentionally counts partial regular files too, so a failed/ENOSPC transfer
// can resume with truthful progress instead of jumping back to zero.
func modelRepoBytesDone(root string) int64 {
	var done int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // Progress is best effort; unreadable entries contribute no observed bytes.
		}
		info, statErr := entry.Info()
		if statErr == nil && info.Mode().IsRegular() {
			done += info.Size()
		}
		return nil
	})
	return done
}

// repoFilesPresent reports whether every file of a repository is on disk.
//
// approximate loosens only the size comparison, and only for a listing whose
// sizes are display values: such a size never equals the file's real length,
// so holding a cache to it would re-download a complete repository every time.
// A completion marker records measured sizes, so the marker check always
// compares exactly.
func repoFilesPresent(root string, files []modelrepo.File, approximate bool) (bool, string) {
	for _, file := range files {
		path, err := modelRepoDestination(root, file.Path)
		if err != nil {
			return false, err.Error()
		}
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Sprintf("%s is not a regular file", file.Path)
		}
		if approximate {
			if info.Size() <= 0 {
				return false, fmt.Sprintf("%s is empty", file.Path)
			}
			continue
		}
		if info.Size() != file.Size {
			return false, fmt.Sprintf("%s has %d bytes, expected %d", file.Path, info.Size(), file.Size)
		}
	}
	return true, ""
}

// failModelFile reports a task that cannot run at all.
func (n *NodeRuntime) failModelFile(mf proto.ModelFileTask, err error) {
	n.reportModelFile(proto.ModelFileStatus{Key: mf.Key, State: proto.MFFailed, Acquisition: mf.Acquisition, TransferID: mf.TransferID, Message: err.Error()})
}

func (n *NodeRuntime) isDownloading(key string) bool {
	n.dlMu.Lock()
	defer n.dlMu.Unlock()
	return n.downloading[key] != nil
}

var completionOnDiskForTask = (*NodeRuntime).completionOnDisk

// completionOnDisk reports whether dir holds a complete public download of the
// task's model, with the marker that says so. The read goes through the
// storage's guard, so a hung share costs one blocked goroutine per directory
// and surfaces as storagemount.ErrUnresponsive.
func (n *NodeRuntime) completionOnDisk(mf proto.ModelFileTask, dir string) (modelCompletion, int64, bool, error) {
	var (
		marker modelCompletion
		size   int64
		ok     bool
	)
	err := n.storageDo(mf.StorageID, "marker/"+dir, n.storageTimeouts().marker, func() error {
		marker, size, ok = validatedMarker(dir, mf)
		return nil
	})
	if err != nil {
		return modelCompletion{}, 0, false, err
	}
	return marker, size, ok, nil
}

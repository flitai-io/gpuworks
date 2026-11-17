package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// copyWorkers is how many files a same-node copy moves at once.
const copyWorkers = 4

// copyFile is one file of a model directory being copied.
type copyFile struct {
	rel     string
	size    int64
	modTime time.Time
}

// copySkipName reports whether a file directly under a model directory is task
// metadata rather than model content, so a copy must not carry it over.
//
// The ModelScope names are the same set internal/agent/modelscan/identity.go
// recognizes a ModelScope download by and pkg/model/modelmetadata/parser.go
// marks a repository by; modelmetadata owns it, and this function must not
// grow its own copy of those names. The completion marker and the copy's own
// source snapshot are written by this agent for this transfer.
func copySkipName(name string) bool {
	return name == modelCompleteMarker || name == copySourceMarker ||
		modelmetadata.IsSourceMarker(name) ||
		strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".part.state")
}

// listCopySource lists a model directory's regular files, skipping dot
// directories (tool metadata) and the names copySkipName classifies as task
// metadata.
//
// Both the listing that starts a copy and the one that verifies its source did
// not change come through here, so the two always see one file set: a resumed
// copy compares snapshot against listing, and a skip applied on only one side
// would make every resume decide the source had changed and start over.
func listCopySource(root string) ([]copyFile, error) {
	var files []copyFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if copySkipName(name) {
			return nil
		}
		info, err := os.Stat(path) // follow file symlinks
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, copyFile{rel: rel, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, err
}

func sameSource(a, b []copyFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].rel != b[i].rel || a[i].size != b[i].size || !a[i].modTime.Equal(b[i].modTime) {
			return false
		}
	}
	return true
}

// copyModelDir copies a model directory that is readable on this node (a
// local storage, or a shared storage mounted here) into workDir, then installs
// it at finalTarget. Each file is written to <name>.part and renamed when
// complete; an interrupted copy resumes from the bytes already there. When the
// source changes while it is being read, the copy starts over.
func (n *NodeRuntime) copyModelDir(ctx context.Context, mf proto.ModelFileTask, workDir, finalTarget string) {
	// The progress monitor reports on its own goroutine while the paths below
	// are resolved in place, so report reads the target through reportedTarget
	// rather than finalTarget itself.
	var reportedTarget atomic.Value
	reportedTarget.Store(finalTarget)
	report := func(st proto.ModelFileStatus) {
		st.Key, st.Path, st.Acquisition, st.TransferID = mf.Key, reportedTarget.Load().(string), proto.ModelAcquireCopy, mf.TransferID
		if ctx.Err() == nil {
			n.reportModelFileContext(ctx, st)
		}
	}
	if ctx.Err() != nil {
		return
	}
	p := newCopyProgress(report)
	p.begin(time.Now())
	stopMonitor := p.monitor(ctx)
	defer func() {
		if stopMonitor != nil {
			stopMonitor()
		}
	}()
	finishMonitor := func() {
		if stopMonitor != nil {
			stopMonitor()
			stopMonitor = nil
		}
	}
	fail := func(err error, done, total int64) {
		finishMonitor()
		state := proto.MFFailed
		if modelDiskFull(err) {
			state = proto.MFInsufficientDisk
		}
		report(proto.ModelFileStatus{Key: mf.Key, State: state, Path: finalTarget, BytesDone: done, BytesTotal: total,
			Message: err.Error(), Acquisition: proto.ModelAcquireCopy})
	}
	source := filepath.Clean(strings.TrimSpace(mf.SourcePath))
	if source == "." || !filepath.IsAbs(source) {
		fail(errors.New("copy has no absolute source directory"), 0, 0)
		return
	}
	for _, path := range []*string{&source, &workDir, &finalTarget} {
		resolved, err := copyCanonicalPath(*path)
		if err != nil {
			fail(fmt.Errorf("resolve copy path: %w", err), 0, 0)
			return
		}
		*path = resolved
	}
	reportedTarget.Store(finalTarget)
	if pathWithin(source, workDir) || pathWithin(workDir, source) || pathWithin(source, finalTarget) || pathWithin(finalTarget, source) {
		fail(errors.New("copy source and target directories overlap"), 0, 0)
		return
	}
	unlock, err := lockCopyTarget(finalTarget)
	if err != nil {
		fail(err, 0, 0)
		return
	}
	defer unlock()
	if _, err := validateModelDir(source); err != nil {
		fail(fmt.Errorf("source is not a complete model: %w", err), 0, 0)
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			p.begin(time.Now())
		}
		files, err := listCopySource(source)
		if err != nil {
			fail(fmt.Errorf("list source: %w", err), 0, 0)
			return
		}
		var total int64
		for _, f := range files {
			total += f.size
		}
		if err := prepareCopyResume(source, workDir, files); err != nil {
			fail(err, 0, total)
			return
		}
		remaining := total - modelRepoBytesDone(workDir)
		if remaining < 0 {
			remaining = 0
		}
		if err := n.checkDownloadSpace(mf.StorageID, workDir, remaining); err != nil {
			fail(err, 0, total)
			return
		}
		p.phase(proto.ModelPhaseCopying, total)
		if err := n.copyFiles(ctx, mf, source, workDir, finalTarget, files, total, p); err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(err, p.done.Load(), total)
			return
		}
		if ctx.Err() != nil {
			return
		}
		p.phase(proto.ModelPhaseVerifying, total)
		after, err := listCopySource(source)
		if err != nil {
			fail(fmt.Errorf("list source: %w", err), 0, total)
			return
		}
		if !sameSource(files, after) {
			n.log.Warn("model source changed while it was copied; starting over", "source", source)
			continue
		}
		manifest := make([]modelrepo.File, 0, len(files))
		for _, f := range files {
			manifest = append(manifest, modelrepo.File{Path: filepath.ToSlash(f.rel), Size: f.size})
		}
		if _, err := validateModelDir(workDir); err != nil {
			fail(fmt.Errorf("copied directory is not a model: %w", err), total, total)
			return
		}
		if ctx.Err() != nil {
			return
		}
		p.phase(proto.ModelPhaseInstalling, total)
		measured, err := writeCompletionMarker(workDir, mf, manifest)
		if err != nil {
			fail(fmt.Errorf("write completion marker: %w", err), total, total)
			return
		}
		if ctx.Err() != nil {
			_ = os.Remove(filepath.Join(workDir, modelCompleteMarker))
			return
		}
		if err := installStagedModelContext(ctx, workDir, finalTarget, mf); err != nil {
			fail(err, total, total)
			return
		}
		finishMonitor()
		report(proto.ModelFileStatus{Key: mf.Key, State: proto.MFPresent, Path: finalTarget, BytesTotal: measured, BytesDone: measured, Acquisition: proto.ModelAcquireCopy})
		return
	}
	fail(errors.New("the source kept changing while it was copied"), 0, 0)
}

func (n *NodeRuntime) copyFiles(ctx context.Context, mf proto.ModelFileTask, source, workDir, finalTarget string, files []copyFile, total int64, p *copyProgress) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	limiter := newCopyLimiter(mf.RateLimitKBs)
	limiter.progress = p
	jobs := make(chan copyFile)
	errs := make(chan error, copyWorkers)
	var wg sync.WaitGroup
	for i := 0; i < copyWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if err := copyOneFile(ctx, filepath.Join(source, f.rel), filepath.Join(workDir, f.rel), f.size, &p.done, limiter); err != nil {
					cancel()
					errs <- fmt.Errorf("copy %s: %w", f.rel, err)
					return
				}

			}
		}()
	}
	var firstErr error
feed:
	for _, f := range files {
		select {
		case jobs <- f:
		case err := <-errs:
			firstErr = err
			break feed
		case <-ctx.Done():
			firstErr = ctx.Err()
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if firstErr == nil || errors.Is(firstErr, context.Canceled) {
			firstErr = err
		}
	}
	return firstErr
}

// copyOneFile copies src to dst through dst.part, resuming a partial .part.
func copyOneFile(ctx context.Context, src, dst string, size int64, done *atomic.Int64, limiter *copyLimiter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() && info.Size() == size {
		done.Add(size)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + ".part"
	offset := int64(0)
	if info, err := os.Stat(part); err == nil && info.Size() <= size {
		offset = info.Size()
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := in.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if err := out.Truncate(offset); err != nil {
		out.Close()
		return err
	}
	if _, err := out.Seek(offset, io.SeekStart); err != nil {
		out.Close()
		return err
	}
	done.Add(offset)
	if err := copyModelBytes(ctx, in, out, done, limiter); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if info, err := os.Stat(part); err != nil || info.Size() != size {
		return fmt.Errorf("copied %s is incomplete", filepath.Base(dst))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(part, dst)
}

// copyLimiter paces a copy to a byte rate; zero means unlimited.
type copyLimiter struct {
	progress *copyProgress
	mu       sync.Mutex
	perByte  time.Duration
	next     time.Time
	disabled bool
}

func newCopyLimiter(kbps int) *copyLimiter {
	if kbps <= 0 {
		return &copyLimiter{disabled: true}
	}
	return &copyLimiter{perByte: time.Second / time.Duration(kbps*1024)}
}

func (l *copyLimiter) wait(ctx context.Context, n int) {
	if l.disabled {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(n) * l.perByte)
	delay := time.Until(l.next)
	l.mu.Unlock()
	if delay <= 0 {
		return
	}
	if l.progress != nil {
		l.progress.waiting.Add(1)
		defer l.progress.waiting.Add(-1)
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
}

// installStagedModel moves a complete staging directory to its final place.
// When workDir is the final place there is nothing to move. When another
// writer (another control plane on the same storage) got there first, its
// copy is kept if it is the same model and the staging copy is dropped; a
// different model at that path is an error, never overwritten.
func installStagedModel(workDir, finalTarget string, mf proto.ModelFileTask) error {
	return installStagedModelContext(context.Background(), workDir, finalTarget, mf)
}

func installStagedModelContext(ctx context.Context, workDir, finalTarget string, mf proto.ModelFileTask) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if filepath.Clean(workDir) == filepath.Clean(finalTarget) {
		return nil
	}
	if entries, err := os.ReadDir(finalTarget); err == nil && len(entries) > 0 {
		if _, ok := validCompletionMarker(finalTarget, mf); ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			_ = os.RemoveAll(workDir)
			return nil
		}
		if _, err := validateModelDir(finalTarget); err == nil && strings.TrimSpace(mf.Staging) != "" {
			return fmt.Errorf("%s already holds a different model; it was left untouched", finalTarget)
		}
		return fmt.Errorf("%s exists and is not empty", finalTarget)
	}
	if err := os.MkdirAll(filepath.Dir(finalTarget), 0o755); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = os.Remove(finalTarget) // an empty directory left by an earlier attempt
	defer removeEmptyParents(filepath.Dir(workDir), stagingRoot(workDir))
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(workDir, finalTarget); err != nil {
		if entries, readErr := os.ReadDir(finalTarget); readErr == nil && len(entries) > 0 {
			if _, ok := validCompletionMarker(finalTarget, mf); ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				_ = os.RemoveAll(workDir)
				return nil
			}
		}
		return fmt.Errorf("install %s: %w", finalTarget, err)
	}
	return nil
}

// stagingRoot is the per-control-plane incoming directory a staging path sits
// under (<root>/.gpuworks/incoming/<id>), or "" when workDir is not staged.
func stagingRoot(workDir string) string {
	marker := string(filepath.Separator) + filepath.Join(proto.StorageMetaDir, proto.StorageIncomingDir) + string(filepath.Separator)
	i := strings.Index(workDir, marker)
	if i < 0 {
		return ""
	}
	rest := workDir[i+len(marker):]
	id, _, _ := strings.Cut(rest, string(filepath.Separator))
	return filepath.Join(workDir[:i+len(marker)], id)
}

// removeEmptyParents removes the empty directories a staged model left
// between it and stop, never stop itself or anything above it.
func removeEmptyParents(dir, stop string) {
	if stop == "" {
		return
	}
	for dir != stop && pathWithin(stop, dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// copyModelBytes counts only bytes accepted by the destination. The I/O
// boundary lets fault tests block real writes without mocking task rules.
func copyModelBytes(ctx context.Context, in io.Reader, out io.Writer, done *atomic.Int64, limiter *copyLimiter) error {
	buf := make([]byte, 1<<20)
	emptyReads := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nr, readErr := in.Read(buf)
		if nr == 0 && readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
		if nr > 0 {
			limiter.wait(ctx, nr)
			if err := ctx.Err(); err != nil {
				return err
			}
			nw, err := out.Write(buf[:nr])
			done.Add(int64(nw))
			if limiter.progress != nil {
				limiter.progress.transferred.Add(int64(nw))
			}
			if err == nil && nw != nr {
				err = io.ErrShortWrite
			}
			if err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

package native

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	kgzip "github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// LayerSource is one image layer and how to open its (possibly gzip
// compressed) tar stream.
type LayerSource struct {
	Digest string
	Size   int64
	Open   func(context.Context) (io.ReadCloser, error)
}

// Unpacker materializes an ordered layer stack into a root directory.
//
// Layers are unpacked concurrently rather than one after another. The
// single-stream inflate rate is what bounds a large engine image -- a 4.6 GB
// layer takes about half a minute on one core no matter how fast the disk is
// -- so overlapping layers is the only way to spend that time once instead of
// once per layer. Cross-layer ordering is preserved by pathTable, and the two
// operations that genuinely need a total order (whiteouts and directory
// metadata) are deferred to a final pass.
type Unpacker struct {
	Root     string
	Workers  int
	Owner    *Owner
	Progress func(digest string, n int64)
	// OnLayerDone reports each layer's wall time once it finishes. A slow
	// pull in the field is almost always one oversized layer whose gzip
	// stream cannot be split, and this is what makes that visible.
	OnLayerDone func(digest string, elapsed time.Duration, size int64)

	umask int
	table *pathTable

	mu           sync.Mutex
	dirs         []dirMeta
	whiteouts    []pendingWhiteout
	pendingLinks []pendingLink
}

type pendingWhiteout struct {
	whiteout
	layer int32
}

type pendingLink struct {
	source string
	target string
}

var tempCounter atomic.Uint64

func nextTempID() uint64 { return tempCounter.Add(1) }

// Unpack applies layers in index order semantics into u.Root.
func (u *Unpacker) Unpack(ctx context.Context, layers []LayerSource) error {
	if err := os.MkdirAll(u.Root, 0o755); err != nil {
		return err
	}
	if u.Owner != nil {
		_ = os.Lchown(u.Root, u.Owner.UID, u.Owner.GID)
	}
	u.umask = currentUmask()
	u.table = newPathTable()

	// Start the largest layers first: the whole unpack finishes no sooner than
	// the longest single inflate, so that one must not be left until last.
	order := make([]int, len(layers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return layers[order[a]].Size > layers[order[b]].Size })

	workers := u.Workers
	if workers <= 0 {
		workers = goruntime.NumCPU()
	}
	if workers > len(layers) {
		workers = len(layers)
	}
	if workers < 1 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for _, index := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			if u.OnLayerDone != nil {
				defer func() { u.OnLayerDone(layers[index].Digest, time.Since(started), layers[index].Size) }()
			}
			if err := u.unpackLayer(ctx, layers[index], int32(index)); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("layer %s: %w", shortDigest(layers[index].Digest), err)
					cancel()
				}
				errMu.Unlock()
			}
		}(index)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tailStart := time.Now()
	if err := u.resolvePendingLinks(); err != nil {
		return err
	}
	if err := u.applyWhiteouts(); err != nil {
		return err
	}
	if err := u.applyDirMeta(); err != nil {
		return err
	}
	if u.OnLayerDone != nil {
		u.OnLayerDone("(whiteouts+metadata)", time.Since(tailStart), 0)
	}
	return nil
}

func (u *Unpacker) unpackLayer(ctx context.Context, layer LayerSource, index int32) error {
	stream, err := layer.Open(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	counted := io.Reader(stream)
	if u.Progress != nil {
		counted = &countingReader{inner: stream, report: func(n int64) { u.Progress(layer.Digest, n) }}
	}
	buffered := bufio.NewReaderSize(counted, 1<<20)
	var input io.Reader = buffered
	magic, _ := buffered.Peek(4)
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		zr, err := kgzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("open gzip: %w", err)
		}
		defer zr.Close()
		input = zr
	case len(magic) == 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		zr, err := zstd.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("open zstd: %w", err)
		}
		defer zr.Close()
		input = zr
	}

	buf := make([]byte, copyBufferSize)
	reader := tar.NewReader(input)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := u.handleEntry(header, reader, index, buf); err != nil {
			return err
		}
	}
}

func (u *Unpacker) handleEntry(header *tar.Header, reader io.Reader, index int32, buf []byte) error {
	name, err := cleanEntryPath(header.Name)
	if err != nil {
		if errors.Is(err, errSkipEntry) {
			return nil
		}
		return err
	}
	if excluded(name) {
		return nil
	}
	if mark, ok := parseWhiteout(name); ok {
		// The marker itself is never extracted; it is applied once every layer
		// has landed, so a higher layer restoring the path still wins.
		// The marker deliberately does not claim its target: applyWhiteouts
		// decides what to delete from the recorded content owners, and a claim
		// here would make the marker look like the path's author.
		u.mu.Lock()
		u.whiteouts = append(u.whiteouts, pendingWhiteout{whiteout: mark, layer: index})
		u.mu.Unlock()
		return nil
	}

	if header.Typeflag == tar.TypeDir {
		// Directories are shared by construction: every layer that touches a
		// subtree repeats its parents. They are never a conflict, only a
		// metadata race, which applyDirMeta settles in layer order.
		shadowed, releaseAncestors := u.table.holdAncestors(name, index)
		if shadowed {
			return nil
		}
		defer releaseAncestors()
		state := u.table.claim(name, index)
		if state == nil {
			return nil
		}
		state.isDir = true
		defer u.table.release(state)
		return u.mkdirEntry(header, u.Root, name)
	}

	shadowed, releaseAncestors := u.table.holdAncestors(name, index)
	if shadowed {
		return nil
	}
	defer releaseAncestors()
	state := u.table.claim(name, index)
	if state == nil {
		// A higher layer owns this path; skip it. The tar reader still has to
		// walk past the entry's bytes to reach the next header.
		return nil
	}
	defer u.table.release(state)
	state.isDir = false

	err = u.writeEntry(header, reader, u.Root, name, buf)
	var pending *pendingLinkError
	if errors.As(err, &pending) {
		u.mu.Lock()
		u.pendingLinks = append(u.pendingLinks, pendingLink{source: pending.source, target: pending.target})
		u.mu.Unlock()
		return nil
	}
	if errors.Is(err, errSkipEntry) {
		return nil
	}
	return err
}

func (u *Unpacker) deferDirMeta(path string, mode os.FileMode, modTime time.Time) {
	u.mu.Lock()
	u.dirs = append(u.dirs, dirMeta{path: path, mode: mode, modTime: modTime})
	u.mu.Unlock()
}

// resolvePendingLinks retries hard links whose target belonged to a layer that
// was still being unpacked. A link that still cannot be made is materialized
// as a copy so the rootfs stays complete.
func (u *Unpacker) resolvePendingLinks() error {
	u.mu.Lock()
	links := u.pendingLinks
	u.pendingLinks = nil
	u.mu.Unlock()

	for _, link := range links {
		if err := u.replace(link.target, func(tmp string) error { return os.Link(link.source, tmp) }); err == nil {
			_ = u.chown(link.target)
			continue
		}
		if err := copyFile(link.source, link.target); err != nil {
			return fmt.Errorf("hard link %s -> %s: %w", link.target, link.source, err)
		}
		_ = u.chown(link.target)
	}
	return nil
}

// applyWhiteouts deletes what each marker hides. Running after every layer
// means the pathTable already records which layer owns each surviving path, so
// content that a later layer restored on top of a whiteout is left alone --
// the case a strict sequential unpack gets right by construction.
func (u *Unpacker) applyWhiteouts() error {
	u.mu.Lock()
	marks := u.whiteouts
	u.mu.Unlock()
	for _, mark := range marks {
		target := filepath.Join(u.Root, filepath.FromSlash(mark.target))
		if mark.opaque {
			entries, err := os.ReadDir(target)
			if err != nil {
				if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
					continue // absent, or a higher layer replaced the directory with a file
				}
				return err
			}
			for _, entry := range entries {
				child := filepath.Join(target, entry.Name())
				if err := u.prune(child, mark.layer); err != nil {
					return err
				}
			}
			continue
		}
		if err := u.prune(target, mark.layer); err != nil {
			return err
		}
	}
	return nil
}

// prune removes path and its descendants that were written by a layer below
// the whiteout. A whiteout hides only what lower layers contributed, so
// content from the whiteout's own layer -- an opaque directory marker always
// travels with that layer's fresh contents for the directory -- and from any
// higher layer is kept.
func (u *Unpacker) prune(path string, layer int32) error {
	rel, err := filepath.Rel(u.Root, path)
	if err != nil {
		return err
	}
	owner, known := u.table.claimedLayer(filepath.ToSlash(rel))
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if known && owner >= layer {
			return nil
		}
		return os.Remove(path)
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	kept := 0
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		if err := u.prune(child, layer); err != nil {
			return err
		}
		if _, err := os.Lstat(child); err == nil {
			kept++
		}
	}
	if kept > 0 || (known && owner >= layer) {
		return nil
	}
	return os.Remove(path)
}

// applyDirMeta restores directory modes once nothing needs to write into them,
// the same reason udocker passes tar --delay-directory-restore. Deepest first,
// so a read-only parent cannot block a child's chmod.
func (u *Unpacker) applyDirMeta() error {
	u.mu.Lock()
	dirs := u.dirs
	u.dirs = nil
	u.mu.Unlock()

	sort.Slice(dirs, func(a, b int) bool {
		return strings.Count(dirs[a].path, string(os.PathSeparator)) > strings.Count(dirs[b].path, string(os.PathSeparator))
	})
	for _, dir := range dirs {
		// A directory that ends up read-only is normal in an image; ignore
		// failures on paths a whiteout has since removed.
		if _, err := os.Lstat(dir.path); err != nil {
			continue
		}
		_ = os.Chmod(dir.path, dir.mode)
		if !dir.modTime.IsZero() {
			_ = os.Chtimes(dir.path, dir.modTime, dir.modTime)
		}
	}
	return nil
}

type countingReader struct {
	inner  io.Reader
	report func(int64)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.report(int64(n))
	}
	return n, err
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

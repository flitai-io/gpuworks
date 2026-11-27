package storagemount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// Reconcile makes every spec usable on this node and returns one Status per spec, in order. Specs
// are handled concurrently and every filesystem call is time limited, so one stuck storage cannot
// stall the others. Mounts this agent created for storages that are no longer listed are
// unmounted unless Busy reports a container using them; Existing, shared_dir and local storages are
// never unmounted.
func (m *Manager) Reconcile(ctx context.Context, specs []Spec) []Status {
	m.recMu.Lock()
	defer m.recMu.Unlock()

	m.snapshotEligible()
	out := make([]Status, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = m.reconcileOne(ctx, sp)
		}()
	}
	wg.Wait()

	keep := make(map[string]bool, len(specs))
	listed := make(map[uint]bool, len(specs))
	for _, sp := range specs {
		listed[sp.ID] = true
	}
	m.forgetStuck(listed)
	for _, sp := range specs {
		p := filepath.Clean(sp.Path)
		keep[p] = true
		if sp.Existing || sp.Type == TypeSharedDir || sp.Type == TypeLocal {
			m.forgetMount(p) // not ours to unmount, whatever an earlier spec said
		}
	}
	m.unmountStale(ctx, keep)
	return out
}

// reconcileOne is reconcileSpec, plus what the agent does about a storage
// that stopped answering (recoverStuck).
func (m *Manager) reconcileOne(ctx context.Context, sp Spec) Status {
	st, sharePath := m.reconcileSpec(ctx, sp)
	if st.State != StateUnresponsive {
		m.clearStuck(sp.ID)
		return st
	}
	return m.recoverStuck(ctx, sp, st, sharePath)
}

// reconcileSpec makes one spec usable. sharePath reports that the status
// was decided on the storage's own directory, not on its node cache.
func (m *Manager) reconcileSpec(ctx context.Context, sp Spec) (st Status, sharePath bool) {
	st = m.reconcileShare(ctx, sp)
	if st.State == StateReady && sp.Type == TypeNFSCache {
		return m.reconcileCache(ctx, sp, st), false
	}
	return st, true
}

func (m *Manager) reconcileShare(ctx context.Context, sp Spec) Status {
	st := Status{ID: sp.ID, Path: sp.Path}
	// The server always sends explicit directories; this package has no default of its own.
	if strings.TrimSpace(sp.Path) == "" {
		return failWith(st, ReasonPathMissing, "the storage has no directory on this node")
	}
	if sp.Type == TypeNFSCache && strings.TrimSpace(sp.CachePath) == "" {
		return failWith(st, ReasonPathMissing, "the storage has no cache directory on this node")
	}
	if !filepath.IsAbs(sp.Path) {
		return failWith(st, ReasonOther, fmt.Sprintf("path %q must be absolute", sp.Path))
	}
	if err := m.call(ctx, storageKey(sp.ID), func() error {
		if err := checkCanonicalStoragePath(sp.Path); err != nil {
			return err
		}
		if sp.CachePath != "" {
			if err := checkCanonicalStoragePath(sp.CachePath); err != nil {
				return fmt.Errorf("cache directory: %w", err)
			}
		}
		return nil
	}); err != nil {
		return failErr(st, err, "validate storage directory")
	}
	switch {
	case sp.Type == TypeLocal:
		st = m.reconcileLocal(ctx, sp, st)
	case sp.Type == TypeSharedDir || ((sp.Type == TypeNFS || sp.Type == TypeNFSCache) && sp.Existing):
		st = m.reconcileDir(ctx, sp, st)
	case sp.Type == TypeNFS || sp.Type == TypeNFSCache:
		st = m.reconcileNFS(ctx, sp, st)
	default:
		st.State, st.Reason = StateUnsupported, ReasonOther
		st.Message = fmt.Sprintf("storage type %q is not supported by this agent", sp.Type)
		return st
	}
	return st
}

func (m *Manager) reconcileLocal(ctx context.Context, sp Spec, st Status) Status {
	st.SharedFSType, st.SharedFSSource = "", ""
	if err := m.call(ctx, storageKey(sp.ID), func() error { return os.MkdirAll(sp.Path, 0o755) }); err != nil {
		return failErr(st, err, "create directory")
	}
	st = m.finishReady(ctx, sp, st, sp.Path)
	if st.State != StateReady || !IsNetworkFS(st.fsMagic) {
		return st
	}
	// The directory is on a shared filesystem: name it as mountinfo does.
	// It stays ready; the platform warns about it.
	st.SharedFSType = "network"
	if entries, err := m.readMounts(); err == nil {
		if e, ok := ContainingMount(entries, sp.Path); ok {
			st.SharedFSType, st.SharedFSSource = e.FSType, e.Source
		}
	}
	return st
}

func (m *Manager) reconcileDir(ctx context.Context, sp Spec, st Status) Status {
	if sp.Type == TypeSharedDir {
		if reason, msg, err := m.createOnShare(ctx, sp); err != nil {
			return failErr(st, err, "create directory")
		} else if reason != "" {
			return failWith(st, reason, msg)
		}
	}
	err := m.call(ctx, storageKey(sp.ID), func() error { return checkReadableDir(sp.Path) })
	if err != nil {
		return failErr(st, err, "check directory")
	}
	reason, msg, err := m.checkBacking(ctx, sp, storageKey(sp.ID))
	if err != nil {
		return failErr(st, err, "check mount")
	}
	if reason != "" {
		return failWith(st, reason, msg)
	}
	return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
}

func (m *Manager) reconcileNFS(ctx context.Context, sp Spec, st Status) Status {
	if err := storagerules.ValidateExport(sp.Export); err != nil {
		return failWith(st, ReasonOther, err.Error())
	}
	entries, err := m.readMounts()
	if err != nil {
		return failWith(st, ReasonOther, "cannot read mountinfo: "+err.Error())
	}
	if e, ok := FindMount(entries, sp.Path); ok {
		if !strings.HasPrefix(e.FSType, "nfs") || normalizeSource(e.Source) != normalizeSource(sp.Export) {
			return failWith(st, ReasonSourceMismatch,
				fmt.Sprintf("%s is already mounted from %s (%s), expected %s", sp.Path, e.Source, e.FSType, sp.Export))
		}
		return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
	}
	if !m.o.IsRoot() {
		st.State, st.Reason = StateUnsupported, ReasonNotRoot
		st.Message = "the agent is not running as root and cannot mount NFS; mount the export yourself and use it as an existing shared directory"
		return st
	}
	opts, err := effectiveOptions(sp.MountOptions)
	if err != nil {
		return failWith(st, ReasonOther, err.Error())
	}
	if m.o.NodeRoot != "" {
		if failed, stop := m.unseenNodeMount(ctx, sp, st); stop {
			return failed
		}
	}
	st.State, st.Message = StateMounting, ""
	if err := m.call(ctx, storageKey(sp.ID), func() error { return os.MkdirAll(sp.Path, 0o755) }); err != nil {
		return failErr(st, err, "create mount point")
	}
	if m.o.NodeRoot != "" {
		if shared, err := m.sharedWithNode(ctx, sp); err != nil {
			return failErr(st, err, "check mount point on the node")
		} else if !shared {
			return failWith(st, ReasonNotSharedWithNode, fmt.Sprintf("%s is not a directory the agent shares with its node: "+
				"the node would mount the export where the agent cannot read it", sp.Path))
		}
	}
	res, err := m.runMount(ctx, storageKey(sp.ID), sp.Export, sp.Path, opts, true)
	if err != nil {
		st = failErr(st, err, "mount")
		if errors.Is(err, ErrUnresponsive) {
			st.Reason = ReasonUnreachable
			st.Message = fmt.Sprintf("the NFS server did not answer within %s", m.o.MountTimeout)
		}
		return st
	}
	if res.err != nil || res.timedOut {
		reason, msg := mountFailure(res, m.o.MountTimeout)
		return failWith(st, reason, msg)
	}
	if m.o.NodeRoot != "" {
		if _, seen := m.mountAt(sp.Path); !seen {
			m.takeOffUnseen(ctx, sp, 1)
			return failWith(st, ReasonNotSharedWithNode, fmt.Sprintf("the node mounted %s on %s, but the mount did not reach the agent", sp.Export, sp.Path))
		}
	}
	m.o.Logger.Info("storagemount: mounted", "storage", sp.Name, "export", sp.Export, "path", sp.Path, "options", opts)
	return m.finishReady(ctx, sp, st, filepath.Join(sp.Path, MetaDirName))
}

// finishReady fills capacity and writability once the storage path is usable. A read-only or
// unwritable storage is still ready.
func (m *Manager) finishReady(ctx context.Context, sp Spec, st Status, writeDir string) Status {
	key := storageKey(sp.ID)
	u, err := m.statfs(ctx, key, sp.Path)
	if err != nil {
		return failErr(st, err, "statfs")
	}
	st.State, st.Reason, st.Message = StateReady, "", ""
	st.TotalBytes, st.AvailableBytes, st.fsMagic = u.TotalBytes, u.AvailableBytes, u.FSType
	w, err := m.writable(ctx, key, writeDir, writeDir != sp.Path)
	if err != nil {
		return failErr(st, err, "write probe")
	}
	st.Writable = w
	return st
}

func (m *Manager) reconcileCache(ctx context.Context, sp Spec, st Status) Status {
	key := cacheKey(sp.ID)
	if err := m.call(ctx, key, func() error { return os.MkdirAll(sp.CachePath, 0o755) }); err != nil {
		st = failErr(st, err, "create cache directory")
		return st
	}
	u, err := m.statfs(ctx, key, sp.CachePath)
	if err != nil {
		return failErr(st, err, "cache statfs")
	}
	st.CachePath = sp.CachePath
	st.CacheTotalBytes, st.CacheAvailableBytes = u.TotalBytes, u.AvailableBytes
	return st
}

func (m *Manager) statfs(ctx context.Context, key, path string) (Usage, error) {
	var u Usage
	err := m.call(ctx, key, func() (err error) { u, err = m.o.Statfs(path); return })
	if err != nil {
		return Usage{}, err // u may still be written by a stuck call
	}
	return u, nil
}

// writable reports whether a temporary file can be created in dir (created first when mkdir is
// set). Failing to write is not an error; only a time out is.
func (m *Manager) writable(ctx context.Context, key, dir string, mkdir bool) (bool, error) {
	ok := false
	err := m.call(ctx, key, func() error {
		ok = writeProbe(dir, mkdir) == nil
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

func writeProbe(dir string, mkdir bool) error {
	if mkdir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	cerr := f.Close()
	rerr := os.Remove(name)
	return errors.Join(cerr, rerr)
}

func checkReadableDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", path, fs.ErrNotExist)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func failWith(st Status, reason, msg string) Status {
	st.State, st.Reason, st.Message = StateFailed, reason, msg
	st.Writable = false
	return st
}

// failErr maps a filesystem error to a status: a time out becomes unresponsive, others failed.
func failErr(st Status, err error, what string) Status {
	switch {
	case errors.Is(err, ErrUnresponsive):
		st.State, st.Reason = StateUnresponsive, ""
		st.Message = what + ": filesystem did not respond"
		st.Writable = false
		return st
	case errors.Is(err, ErrUnsupported):
		st.State, st.Reason, st.Message = StateUnsupported, ReasonOther, what+": "+err.Error()
		st.Writable = false
		return st
	case errors.Is(err, fs.ErrPermission):
		return failWith(st, ReasonPermission, what+": "+err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return failWith(st, ReasonPathMissing, what+": "+err.Error())
	}
	return failWith(st, ReasonOther, what+": "+err.Error())
}

// createOnShare creates a shared directory that does not exist yet (such as a
// models directory suggested on a cluster's NAS) when its nearest existing
// ancestor is on a network or FUSE filesystem. Any other ancestor (the root
// filesystem, or a local disk's mount point) means the share is not
// mounted, and creating the directory there would write models to the local
// disk, so it is answered as not mounted and nothing is created. A
// directory that exists is left to the checks that follow.
func (m *Manager) createOnShare(ctx context.Context, sp Spec) (reason, msg string, err error) {
	key := storageKey(sp.ID)
	var missing bool
	if err := m.call(ctx, key, func() error {
		_, statErr := os.Stat(sp.Path)
		missing = errors.Is(statErr, fs.ErrNotExist)
		return nil
	}); err != nil || !missing {
		return "", "", err
	}
	if reason, msg, err := m.ancestorOnShare(ctx, key, nearestExisting(sp.Path)); err != nil || reason != "" {
		return reason, msg, err
	}
	return "", "", m.call(ctx, key, func() error { return os.MkdirAll(sp.Path, 0o755) })
}

// ancestorOnShare says whether dir, the nearest existing ancestor of a
// shared directory that does not exist yet, is on a network or FUSE
// filesystem. Being a mount point is not enough here: the root filesystem
// is one.
func (m *Manager) ancestorOnShare(ctx context.Context, key, dir string) (reason, msg string, err error) {
	u, err := m.statfs(ctx, key, dir)
	if err != nil {
		return "", "", err
	}
	if IsNetworkFS(u.FSType) {
		return "", "", nil
	}
	return ReasonNotMounted, fmt.Sprintf("%s does not exist, and %s is not on a network filesystem; "+
		"the share is not mounted there, and creating the directory would write to the local disk", dir, dir), nil
}

// nearestExisting is path, or its nearest ancestor that exists.
func nearestExisting(path string) string {
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
}

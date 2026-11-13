package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// storageHealthInterval is how often the agent re-checks its storages. The
// check is local (mountinfo plus a time-limited statfs); only a change is
// reported to the server.
const storageHealthInterval = 30 * time.Second

// storageMountTimeout bounds one mount or umount command.
const storageMountTimeout = 20 * time.Second

// errStorageReadOnly is why a write task for a storage that is ready but
// read-only here fails instead of waiting: waiting would never end.
var errStorageReadOnly = errors.New("storage is read-only on this node")

// storageRuntime keeps the storages the server wants usable on this node and
// their last state. A storage is made usable in the background, so a slow or
// hanging NFS mount never holds up the control connection.
type storageRuntime struct {
	manager *storagemount.Manager
	// timeouts bound the calls into a share (storageDo); a test shortens its
	// own runtime's.
	timeouts storageTimeouts

	mu        sync.RWMutex
	tasks     map[uint]proto.StorageTask
	known     bool // the server sent a storage list at least once
	statuses  map[uint]storagemount.Status
	reported  []proto.StorageStatus
	heartbeat map[uint]time.Time
	// hbInvalid remembers the storages whose heartbeat the server asked for
	// without an interval, so the protocol error is logged once.
	hbInvalid map[uint]bool
	// waiting holds model-file tasks for storages that were not ready yet;
	// they are retried after every check. A task leaves it when it runs, is
	// withdrawn or evicted, or when its storage is removed. Like writerWaiting
	// it keeps one task per key: a newer task for the key replaces the parked
	// one (see modelfile_queue.go).
	waiting map[string]proto.ModelFileTask
	// writerWaiting holds tasks blocked by another writer or the copy limit.
	// Owner return retries them directly; non-zero storages are checked again.
	// A key is parked in at most one of the two queues.
	writerWaiting map[string]proto.ModelFileTask
	// incomingSwept holds the storages whose leftover upload staging this
	// process already swept once (sweepIncomingOnce).
	incomingSwept map[uint]bool

	trigger chan bool // true forces a status report
}

func newStorageRuntime(stateDir string, busy func(string) bool, runner storagemount.Runner) *storageRuntime {
	return &storageRuntime{
		timeouts: defaultStorageTimeouts(),
		manager: storagemount.New(storagemount.Options{
			StateFile: filepath.Join(stateDir, "storage-mounts.json"),
			// A probe answers through a control RPC with a time limit of its
			// own; a mount that is still trying after 20 seconds is reported
			// as an unreachable server inside that limit instead of a bare
			// timeout.
			MountTimeout: storageMountTimeout,
			ProbeDir:     filepath.Join(stateDir, "probe-mounts"),
			Busy:         busy,
			Runner:       runner,
		}),
		tasks: map[uint]proto.StorageTask{}, statuses: map[uint]storagemount.Status{},
		heartbeat: map[uint]time.Time{}, hbInvalid: map[uint]bool{}, waiting: map[string]proto.ModelFileTask{},
		writerWaiting: map[string]proto.ModelFileTask{}, incomingSwept: map[uint]bool{}, trigger: make(chan bool, 1),
	}
}

// mountBusy reports whether a running instance mounts something under path,
// so a storage is never unmounted from under a live container.
func (n *NodeRuntime) mountBusy(path string) bool {
	if n.engine == nil {
		return false
	}
	clean := filepath.Clean(path)
	for _, spec := range n.engine.RunningSpecs() {
		if spec.ModelPath != "" && pathWithin(clean, filepath.Clean(spec.ModelPath)) {
			return true
		}
		for _, mount := range spec.Mounts {
			if mount.Source != "" && pathWithin(clean, filepath.Clean(mount.Source)) {
				return true
			}
		}
	}
	return false
}

// applyStorages records the server's storage list and wakes the background
// loop. A nil list is a server that predates storages: nothing changes.
func (n *NodeRuntime) applyStorages(tasks []proto.StorageTask) {
	if tasks == nil || n.storage == nil {
		return
	}
	s := n.storage
	s.mu.Lock()
	s.known = true
	s.tasks = make(map[uint]proto.StorageTask, len(tasks))
	for _, task := range tasks {
		s.tasks[task.ID] = task
	}
	// A storage the server no longer lists takes its parked tasks with it:
	// they would otherwise start writing the day the id comes back.
	for key, task := range s.waiting {
		if _, ok := s.tasks[task.StorageID]; !ok && task.StorageID != 0 {
			delete(s.waiting, key)
		}
	}
	for key, task := range s.writerWaiting {
		if _, ok := s.tasks[task.StorageID]; !ok && task.StorageID != 0 {
			delete(s.writerWaiting, key)
		}
	}
	for id := range s.hbInvalid {
		if _, ok := s.tasks[id]; !ok {
			delete(s.hbInvalid, id)
		}
	}
	s.mu.Unlock()
	select {
	case s.trigger <- true:
	default:
		// A pending trigger already forces a report.
	}
}

// runStorages is the background loop: check on every trigger and every
// storageHealthInterval, report changes, keep heartbeats fresh and retry the
// model-file tasks that were waiting for a storage.
func (n *NodeRuntime) runStorages(ctx context.Context) {
	if n.storage == nil {
		return
	}
	ticker := time.NewTicker(storageHealthInterval)
	defer ticker.Stop()
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case force = <-n.storage.trigger:
		case <-ticker.C:
		}
		n.checkStorages(ctx, force)
	}
}

func (n *NodeRuntime) checkStorages(ctx context.Context, force bool) {
	s := n.storage
	s.mu.RLock()
	known := s.known
	specs := make([]storagemount.Spec, 0, len(s.tasks))
	for _, task := range s.tasks {
		specs = append(specs, n.storageSpec(task))
	}
	s.mu.RUnlock()
	if !known {
		return
	}
	statuses := s.manager.Reconcile(ctx, specs)
	s.mu.Lock()
	s.statuses = make(map[uint]storagemount.Status, len(statuses))
	for _, st := range statuses {
		s.statuses[st.ID] = st
	}
	report := make([]proto.StorageStatus, 0, len(statuses))
	for _, st := range statuses {
		report = append(report, proto.StorageStatus{
			ID: st.ID, State: st.State, Reason: st.Reason, Message: st.Message, Path: st.Path, Writable: st.Writable,
			TotalBytes: st.TotalBytes, AvailableBytes: st.AvailableBytes, CachePath: st.CachePath,
			CacheTotalBytes: st.CacheTotalBytes, CacheAvailableBytes: st.CacheAvailableBytes,
		})
	}
	changed := force || storageReportChanged(s.reported, report)
	if changed {
		s.reported = report
	}
	waiting := make([]proto.ModelFileTask, 0, len(s.waiting))
	for _, task := range s.waiting {
		if st, ok := s.statuses[task.StorageID]; ok && st.State == storagemount.StateReady {
			// The retry removes or re-parks the task. Until then it stays queued
			// so that eviction or a newer task for the key can fence this
			// snapshot (see retryParkedModelFile).
			waiting = append(waiting, task)
		}
	}
	s.mu.Unlock()
	if changed {
		n.emit(proto.TStatusStorage, proto.StorageStatusReport{Storages: report})
	}
	n.refreshHeartbeats()
	n.sweepIncomingOnce()
	for _, task := range waiting {
		n.retryParkedModelFile(ctx, task)
	}
}

// storageReportChanged ignores capacity drift under 1% so a busy disk does not
// turn into a report every check.
func storageReportChanged(prev, next []proto.StorageStatus) bool {
	if len(prev) != len(next) {
		return true
	}
	byID := make(map[uint]proto.StorageStatus, len(prev))
	for _, st := range prev {
		byID[st.ID] = st
	}
	moved := func(a, b, total int64) bool {
		d := a - b
		if d < 0 {
			d = -d
		}
		return total > 0 && d*100 > total
	}
	for _, st := range next {
		old, ok := byID[st.ID]
		if !ok || old.State != st.State || old.Reason != st.Reason || old.Message != st.Message ||
			old.Path != st.Path || old.Writable != st.Writable || old.CachePath != st.CachePath ||
			moved(old.AvailableBytes, st.AvailableBytes, st.TotalBytes) || moved(old.TotalBytes, st.TotalBytes, st.TotalBytes) ||
			moved(old.CacheAvailableBytes, st.CacheAvailableBytes, st.CacheTotalBytes) {
			return true
		}
	}
	return false
}

// storageSpec turns a server task into the mount manager's spec. The server
// always sends explicit directories (merged with the node's overrides), so
// there are no agent-side defaults: a missing one makes the storage fail with
// path_missing in the mount manager.
func (n *NodeRuntime) storageSpec(task proto.StorageTask) storagemount.Spec {
	return storagemount.Spec{ID: task.ID, Name: task.Name, Type: task.Type, Path: task.Path,
		Export: task.Export, MountOptions: task.MountOptions, Existing: task.Existing, CachePath: task.CachePath}
}

// storageRoot is the directory a model-file task's RelPath is relative to,
// and whether the storage is usable for it now. A storage that is ready but
// read-only here can never take the write, which is an error rather than a
// wait.
func (n *NodeRuntime) storageRoot(task proto.ModelFileTask) (string, bool, error) {
	if task.StorageID == 0 || n.storage == nil {
		return n.ModelsRoot(), true, nil
	}
	s := n.storage
	s.mu.RLock()
	storageTask, ok := s.tasks[task.StorageID]
	status, checked := s.statuses[task.StorageID]
	s.mu.RUnlock()
	if !ok {
		return "", false, fmt.Errorf("storage %d is not configured on this node", task.StorageID)
	}
	spec := n.storageSpec(storageTask)
	switch {
	case spec.Type == proto.StorageLocal:
		if strings.TrimSpace(spec.Path) == "" {
			return "", false, fmt.Errorf("storage %s has no directory on this node", spec.Name)
		}
		return spec.Path, true, nil
	case task.Cache:
		if strings.TrimSpace(spec.CachePath) == "" {
			return "", false, fmt.Errorf("storage %s has no cache directory on this node", spec.Name)
		}
		return spec.CachePath, checked && status.State == storagemount.StateReady, nil
	default:
		if !checked || status.State != storagemount.StateReady {
			return spec.Path, false, nil
		}
		if !status.Writable {
			return "", false, errStorageReadOnly
		}
		return spec.Path, true, nil
	}
}

// storageDo runs a filesystem access under a storage through the mount
// manager's guard: bounded by timeout, and at most one call in flight per
// (storage, op), so a hung NFS mount costs one blocked goroutine rather than
// one per caller. It returns storagemount.ErrUnresponsive when the call did not
// finish or an earlier one still has not.
func (n *NodeRuntime) storageDo(storageID uint, op string, timeout time.Duration, fn func() error) error {
	if n.storage == nil {
		return fn()
	}
	return n.storage.manager.Do(storageID, op, timeout, fn)
}

// waitForStorage parks a task until its storage is ready and asks for a check
// now; the check retries it.
func (n *NodeRuntime) waitForStorage(task proto.ModelFileTask, retry bool) {
	if n.parkStorageModelFile(task, retry) {
		select {
		case n.storage.trigger <- false:
		default:
		}
	}
}

// dropWaitingUnder forgets the parked tasks whose target is, or lies below or
// above, a directory that was evicted.
func (n *NodeRuntime) dropWaitingUnder(target string) {
	if n.storage == nil {
		return
	}
	for _, task := range n.parkedModelFiles() {
		root, _, err := n.storageRoot(task)
		if err != nil {
			continue
		}
		abs, err := resolveStorageTarget(root, taskTarget(task))
		if err == nil && (pathWithin(target, abs) || pathWithin(abs, target)) {
			// Only the task that was inspected: a newer task parked for the key
			// meanwhile is not the one that was found under the directory.
			n.dropParkedModelFile(task)
		}
	}
}

// pruneWaiting drops the parked tasks a full desired state no longer lists.
func (n *NodeRuntime) pruneWaiting(desired []proto.ModelFileTask) {
	listed := make(map[string]bool, len(desired))
	for _, task := range desired {
		listed[task.Key] = true
	}
	n.dlMu.Lock()
	for key := range n.stoppedCopies {
		if !listed[key] {
			delete(n.stoppedCopies, key)
		}
	}
	for key, active := range n.downloading {
		if active.copy && !listed[key] {
			active.cancel()
		}
	}
	n.dlMu.Unlock()
	if n.storage == nil {
		return
	}
	n.storage.mu.Lock()
	for key := range n.storage.waiting {
		if !listed[key] {
			delete(n.storage.waiting, key)
		}
	}
	for key := range n.storage.writerWaiting {
		if !listed[key] {
			delete(n.storage.writerWaiting, key)
		}
	}
	n.storage.mu.Unlock()
}

// storageRoots lists every directory model files may be written to or removed
// from on this node: the models root, local storages and node caches. A
// shared storage's own files are removed through storage.remove instead.
func (n *NodeRuntime) storageRoots() []string {
	roots := []string{filepath.Clean(n.ModelsRoot())}
	if n.storage == nil {
		return roots
	}
	n.storage.mu.RLock()
	defer n.storage.mu.RUnlock()
	for _, task := range n.storage.tasks {
		spec := n.storageSpec(task)
		if spec.Type == proto.StorageLocal && spec.Path != "" {
			roots = append(roots, filepath.Clean(spec.Path))
		}
		if spec.Type == proto.StorageNFSCache && spec.CachePath != "" {
			roots = append(roots, filepath.Clean(spec.CachePath))
		}
	}
	return roots
}

// readyMount is a shared storage's mount point when it is ready here.
func (n *NodeRuntime) readyMount(storageID uint) (string, storagemount.Status, error) {
	if n.storage == nil {
		return "", storagemount.Status{}, errors.New("storages are not available on this agent")
	}
	n.storage.mu.RLock()
	task, ok := n.storage.tasks[storageID]
	status, checked := n.storage.statuses[storageID]
	n.storage.mu.RUnlock()
	if !ok {
		return "", status, fmt.Errorf("storage %d is not configured on this node", storageID)
	}
	if !proto.StorageShared(task.Type) {
		return "", status, fmt.Errorf("storage %s is not shared", task.Name)
	}
	if !checked || status.State != storagemount.StateReady {
		return "", status, fmt.Errorf("storage %s is not ready on this node (%s)", task.Name, status.State)
	}
	return n.storageSpec(task).Path, status, nil
}

// storagePathUsage reports the last known capacity of each storage on this
// node without touching the filesystems again (a hung NFS mount must not stall
// the node status report).
func (n *NodeRuntime) storagePathUsage() []proto.PathUsage {
	if n.storage == nil {
		return nil
	}
	n.storage.mu.RLock()
	defer n.storage.mu.RUnlock()
	var out []proto.PathUsage
	for id, st := range n.storage.statuses {
		if st.State != storagemount.StateReady {
			continue
		}
		if st.TotalBytes > 0 {
			out = append(out, proto.PathUsage{Kind: proto.PathKindStorage, StorageID: id, Path: st.Path, TotalBytes: st.TotalBytes, AvailableBytes: st.AvailableBytes})
		}
		if st.CacheTotalBytes > 0 {
			out = append(out, proto.PathUsage{Kind: proto.PathKindStorageCache, StorageID: id, Path: st.CachePath, TotalBytes: st.CacheTotalBytes, AvailableBytes: st.CacheAvailableBytes})
		}
	}
	return out
}

// standardRootStateDir mirrors cmd/gpuworks-agent's defaultStateDir system
// path: the one root layout whose <state>/models is the default storage
// directory (/opt/gpuworks/models) already, so offering a directory there
// would state the same fact twice.
const standardRootStateDir = "/opt/gpuworks"

// OwnModelsDir is the directory the agent offers the server for its model
// files (see proto.Hello.OwnModelsDir). An agent without root cannot write
// the storages' own directories and always offers <state>/models. A root
// agent at the standard layout keeps using the storages' directories, which
// are <state>/models there; a root agent with any other state directory - a
// cloud install on a data disk, a CI container - offers its directory too,
// so the first desired state cannot silently relocate its models to the
// global default. The server records the offer once and never replaces a
// directory an operator set.
func (n *NodeRuntime) OwnModelsDir() string {
	return ownModelsDir(n.stateDir, os.Geteuid())
}

func ownModelsDir(stateDir string, euid int) string {
	if euid == 0 && filepath.Clean(stateDir) == standardRootStateDir {
		return ""
	}
	return filepath.Join(stateDir, "models")
}

// storageTimeouts are the time limits of the agent's calls into a share. A
// call that outlives its limit is left behind (a hung NFS mount cannot be
// interrupted) and its storage refuses a second one of the same kind until it
// returns.
type storageTimeouts struct {
	marker    time.Duration // reading a model's completion marker and checking its files
	space     time.Duration // one free-space check
	heartbeat time.Duration // writing this control plane's heartbeat
	sweep     time.Duration // emptying the storage's trash
	peers     time.Duration // reading the other control planes' heartbeats
	remove    time.Duration // removing a model directory
	scanGrace time.Duration // how long a scan page may overrun its deadline
}

func defaultStorageTimeouts() storageTimeouts {
	return storageTimeouts{
		marker: 30 * time.Second, space: 30 * time.Second, heartbeat: 30 * time.Second,
		sweep: 5 * time.Minute, peers: 15 * time.Second, remove: 30 * time.Second, scanGrace: 10 * time.Second,
	}
}

// storageTimeouts are the runtime's limits, the defaults when it has no
// storage support.
func (n *NodeRuntime) storageTimeouts() storageTimeouts {
	if n.storage == nil {
		return defaultStorageTimeouts()
	}
	return n.storage.timeouts
}

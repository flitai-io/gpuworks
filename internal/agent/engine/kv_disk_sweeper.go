// kv_disk_sweeper.go keeps the KV disk tier inside the capacity the deployment
// was given. vLLM's filesystem tier has no ceiling and never deletes a block:
// every eviction from the memory tier cascades to disk and the directory grows
// until the filesystem is full. Nothing in the engine can be asked to stop, so
// the node prunes the directory itself.

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/collector"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// kvDiskBlockSuffix is what vLLM names a finished block file. Its in-flight
	// writes and its O_DIRECT probe both end in .tmp and are never touched:
	// removing one would break a write that is about to be renamed into place.
	kvDiskBlockSuffix = ".bin"
	// kvDiskSweepInterval is how often the node re-measures every disk tier
	// directory. A full walk of a directory with millions of small files is not
	// free, and a shorter period buys nothing: the overshoot between two sweeps
	// is at most one round of memory-tier evictions.
	kvDiskSweepInterval = 30 * time.Second
	// kvDiskSweepTargetRatio is the hysteresis. Pruning back to exactly the
	// limit would delete a handful of blocks on every single sweep; pruning to
	// 90% makes deletion occasional and bounded.
	kvDiskSweepTargetRatio = 0.9
)

// KVDiskTarget is one deployment's disk tier directory on this node, with the
// ceiling the server sized for it and the instances that share it. Replicas of
// one deployment share a directory on purpose: vLLM addresses block files by
// content hash and publishes them with an atomic rename, so a shared root is a
// shared cache rather than a collision.
type KVDiskTarget struct {
	DeploymentID uint
	Instances    []string
	Engine       string
	Dir          string
	LimitBytes   int64
}

// kvDiskFS is the sweeper's view of the filesystem, so its policy can be tested
// on a temporary directory without a running engine.
type kvDiskFS interface {
	WalkDir(root string, fn fs.WalkDirFunc) error
	Remove(name string) error
}

type osKVDiskFS struct{}

func (osKVDiskFS) WalkDir(root string, fn fs.WalkDirFunc) error { return filepath.WalkDir(root, fn) }
func (osKVDiskFS) Remove(name string) error                     { return os.Remove(name) }

// KVDiskSweeper prunes every disk tier directory on the node from a single
// goroutine. One goroutine, not one per instance: the work is disk-bound, and
// two concurrent walks of the same device only make each other slower.
type KVDiskSweeper struct {
	node     string
	targets  func() []KVDiskTarget
	report   func([]proto.MetricSample)
	log      *slog.Logger
	files    kvDiskFS
	now      func() time.Time
	interval time.Duration
}

// NewKVDiskSweeper builds the node's sweeper. targets is asked once per round,
// so a deployment that stops between rounds simply disappears from the work
// list; report publishes the usage samples through the agent's metric channel.
func NewKVDiskSweeper(node string, targets func() []KVDiskTarget, report func([]proto.MetricSample), log *slog.Logger) *KVDiskSweeper {
	return &KVDiskSweeper{
		node:     node,
		targets:  targets,
		report:   report,
		log:      log,
		files:    osKVDiskFS{},
		now:      time.Now,
		interval: kvDiskSweepInterval,
	}
}

// Run sweeps every interval until ctx is cancelled.
func (s *KVDiskSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce()
		}
	}
}

// RunOnce measures, prunes and reports every directory once, in order. Exposed
// so tests drive a deterministic round instead of waiting on the ticker.
func (s *KVDiskSweeper) RunOnce() {
	var samples []proto.MetricSample
	at := s.now().UnixMilli()
	for _, target := range s.targets() {
		if target.Dir == "" || target.LimitBytes <= 0 {
			continue
		}
		used := s.sweep(target)
		for _, instance := range target.Instances {
			labels := map[string]string{"node": s.node, "instance": instance, "engine": target.Engine}
			collector.AddDeploymentLabel(labels, target.DeploymentID)
			samples = append(samples,
				proto.MetricSample{Metric: "gpuworks_kv_disk_used_bytes", Labels: labels, Value: float64(used), TimeMS: at},
				proto.MetricSample{Metric: "gpuworks_kv_disk_limit_bytes", Labels: labels, Value: float64(target.LimitBytes), TimeMS: at},
			)
		}
	}
	if len(samples) > 0 && s.report != nil {
		s.report(samples)
	}
}

// kvDiskBlock is one prunable block file.
type kvDiskBlock struct {
	path     string
	size     int64
	modified time.Time
}

// sweep measures one directory and deletes oldest-first until it is back under
// the hysteresis target. It answers with the bytes still held after pruning.
//
// Oldest-first is write order, not access order: reading a block does not touch
// its mtime, and atime is unusable under the relatime default. The cost is a
// slightly lower hit rate than a true LRU would give, which is the cheaper side
// of the trade against tracking every access ourselves.
func (s *KVDiskSweeper) sweep(target KVDiskTarget) int64 {
	blocks, used := s.collect(target)
	if used <= target.LimitBytes {
		return used
	}
	goal := int64(float64(target.LimitBytes) * kvDiskSweepTargetRatio)
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].modified.Before(blocks[j].modified) })
	removed, freed := 0, int64(0)
	for _, block := range blocks {
		if used <= goal {
			break
		}
		if err := s.files.Remove(block.path); err != nil {
			// A block that cannot be removed (a read-only file, a racing
			// engine) must not stop the rest of the directory from shrinking.
			s.log.Warn("KV disk sweep could not remove a block", "deployment", target.DeploymentID, "path", block.path, "err", err)
			continue
		}
		used -= block.size
		freed += block.size
		removed++
	}
	s.log.Info("KV disk sweep pruned the disk tier",
		"deployment", target.DeploymentID, "dir", target.Dir,
		"limit_bytes", target.LimitBytes, "used_bytes", used, "removed_blocks", removed, "freed_bytes", freed)
	return used
}

// collect walks the directory once and returns its block files with the total
// they occupy. Anything that is not a finished block - the tier's config.json,
// an in-flight .tmp - is invisible to the sweeper.
func (s *KVDiskSweeper) collect(target KVDiskTarget) ([]kvDiskBlock, int64) {
	var blocks []kvDiskBlock
	used := int64(0)
	err := s.files.WalkDir(target.Dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable subtree is reported once and skipped; the rest of
			// the directory is still measurable and still prunable.
			s.log.Warn("KV disk sweep could not read a path", "deployment", target.DeploymentID, "path", name, "err", walkErr)
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || filepath.Ext(name) != kvDiskBlockSuffix {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			// The block was removed between the readdir and the stat; it holds
			// no bytes any more, so there is nothing to count or prune.
			return nil //nolint:nilerr // The sweep tolerates entries disappearing or becoming unreadable during inspection.
		}
		blocks = append(blocks, kvDiskBlock{path: name, size: info.Size(), modified: info.ModTime()})
		used += info.Size()
		return nil
	})
	if err != nil {
		s.log.Warn("KV disk sweep could not walk the disk tier", "deployment", target.DeploymentID, "dir", target.Dir, "err", err)
	}
	return blocks, used
}

// KVDiskTargets is the node's current disk tier work list: one entry per
// deployment that has a running instance with a kv-disk mount, carrying every
// instance of that deployment on this node. It is derived from the desired
// state, so nothing has to be remembered separately.
func (e *Engine) KVDiskTargets() []KVDiskTarget {
	byDeployment := map[uint]*KVDiskTarget{}
	for _, spec := range e.RunningSpecs() {
		mount, ok := kvDiskMountOf(spec)
		if !ok {
			continue
		}
		limit, err := strconv.ParseInt(mount.SizeHint, 10, 64)
		if err != nil || limit <= 0 {
			// Without a ceiling there is nothing to enforce or report; the
			// directory is still mounted and the engine still uses it.
			continue
		}
		if target, known := byDeployment[spec.DeploymentID]; known {
			target.Instances = append(target.Instances, spec.UUID)
			continue
		}
		byDeployment[spec.DeploymentID] = &KVDiskTarget{
			DeploymentID: spec.DeploymentID, Engine: spec.Engine,
			Dir: e.kvDiskDir(spec.DeploymentID), LimitBytes: limit,
			Instances: []string{spec.UUID},
		}
	}
	out := make([]KVDiskTarget, 0, len(byDeployment))
	for _, target := range byDeployment {
		sort.Strings(target.Instances)
		out = append(out, *target)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}

// kvDiskMountOf returns the instance's disk tier mount, if it has one.
func kvDiskMountOf(spec proto.InstanceDesiredState) (proto.MountSpec, bool) {
	for _, mount := range spec.Mounts {
		if mount.Purpose == proto.MountPurposeKVDisk {
			return mount, true
		}
	}
	return proto.MountSpec{}, false
}

// kvDiskDir is the managed host directory backing a deployment's disk tier. The
// key is the deployment, not the instance: replicas on one node share the cache,
// and one deployment's directory is invisible to every other deployment.
func (e *Engine) kvDiskDir(deploymentID uint) string {
	return filepath.Join(e.runtimeMountManager().purposeRoot(proto.MountPurposeKVDisk), kvDiskKey(deploymentID))
}

func kvDiskKey(deploymentID uint) string { return strconv.FormatUint(uint64(deploymentID), 10) }

// applyKVDiskMount fills in the host source of the disk tier mount. The server
// states only the container path and the ceiling; which host directory backs it
// is a node decision, so the agent prepares one, writes it into the workload and
// asks the filesystem for a hard quota when it can enforce one.
func (e *Engine) applyKVDiskMount(ws *runtime.WorkloadSpec, spec proto.InstanceDesiredState) error {
	mount, ok := kvDiskMountOf(spec)
	if !ok || mount.Source != "" {
		return nil
	}
	dir, err := e.runtimeMountManager().PrepareInstanceDir(proto.MountPurposeKVDisk, kvDiskKey(spec.DeploymentID))
	if err != nil {
		return err
	}
	target := filepath.Clean(mount.Target)
	for index := range ws.Mounts {
		if filepath.Clean(ws.Mounts[index].Target) == target {
			ws.Mounts[index].Source = dir
		}
	}
	limit, parseErr := strconv.ParseInt(mount.SizeHint, 10, 64)
	if parseErr == nil && limit > 0 {
		applyKVDiskProjectQuota(e.log, dir, spec.DeploymentID, limit)
	}
	return nil
}

// releaseKVDiskDir drops a deployment's disk tier directory once this node runs
// no more of its instances. A replica going away must not throw away the cache
// the surviving replicas are still reading.
func (e *Engine) releaseKVDiskDir(spec proto.InstanceDesiredState) {
	if _, ok := kvDiskMountOf(spec); !ok {
		return
	}
	e.mu.Lock()
	for uuid, other := range e.desired {
		if uuid != spec.UUID && other.DeploymentID == spec.DeploymentID {
			e.mu.Unlock()
			return
		}
	}
	e.mu.Unlock()
	if err := os.RemoveAll(e.kvDiskDir(spec.DeploymentID)); err != nil {
		e.log.Warn("could not remove the KV disk tier directory", "deployment", spec.DeploymentID, "err", err)
	}
}

// reclaimOrphanKVDiskDirs removes the disk tier directory of every deployment
// this node has no desired instance of. An instance deleted while the agent was
// down never reaches releaseKVDiskDir, and the directory (up to twice the
// weights) would stay for good. Only a directory named like a deployment key is
// touched, and each one is checked against the desired set again right before
// it goes, so an instance that arrives mid-sweep keeps its cache. The tier is a
// cache: a wrong guess costs a cold start, not data.
func (e *Engine) reclaimOrphanKVDiskDirs() error {
	root := e.runtimeMountManager().purposeRoot(proto.MountPurposeKVDisk)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list KV disk tier directories: %w", err)
	}
	var failures []string
	for _, entry := range entries {
		id, perr := strconv.ParseUint(entry.Name(), 10, 32)
		if perr != nil || !entry.IsDir() || kvDiskKey(uint(id)) != entry.Name() || e.hasDesiredDeployment(uint(id)) {
			continue
		}
		e.log.Info("removing orphan KV disk tier directory", "deployment", id)
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entry.Name(), err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("orphan KV disk tier cleanup failed for %d directory(ies): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func (e *Engine) hasDesiredDeployment(deploymentID uint) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, spec := range e.desired {
		if spec.DeploymentID == deploymentID {
			return true
		}
	}
	return false
}

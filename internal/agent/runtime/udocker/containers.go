package udocker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// ListContainers lists udocker container names, including stopped ones.
//
// udocker has no daemon to query, so the listing is the container directory
// layout under UDOCKER_DIR/containers: one entry per container, named exactly
// as GPUWorks created it (`--name=<gpuworks-id>`, or the generated id for a
// bare `udocker create`). Reading the directory is deliberate — `udocker ps`
// would route the query through runuser/python, and it truncates names, which
// is precisely what an orphan sweep must not do.
//
// Only entries that actually carry a ROOT directory count as containers, and
// the `<name>.creating` staging directory of an in-flight native create is
// excluded explicitly even though it already has a ROOT: reporting it would
// make the orphan sweeper delete a rootfs that is still being built.
func (d *Driver) ListContainers(ctx context.Context) ([]string, error) {
	containersDir := filepath.Join(d.udockerDir, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing has been provisioned on this node yet.
			return nil, nil
		}
		return nil, fmt.Errorf("list udocker containers: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, native.CreateStagingSuffix) {
			continue
		}
		// Stat rather than trusting the entry type: a container name is a
		// symlink when udocker created it via --name, and udocker itself treats
		// those aliases as containers.
		root, err := os.Stat(filepath.Join(containersDir, name, "ROOT"))
		if err != nil || !root.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RunningContainers lists the workloads whose tracked process group is alive,
// from the same pid files Status reads; no CLI is involved.
func (d *Driver) RunningContainers(ctx context.Context) ([]string, error) {
	return d.procs().Running()
}

// Status reports whether the tracked process group is alive.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	exists, running, code := d.procs().Status(ctx, id)
	return runtime.Status{Exists: exists, Running: running, ExitCode: code}, nil
}

// Logs returns the redirected log stream: the last tail lines (all of it when
// tail <= 0), then, when following, whatever the container writes next.
func (d *Driver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	return procgroup.Logs(ctx, d.LogPath(id), follow, tail)
}

// Stop kills the process group.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if pid, ok := d.procs().PID(id); ok {
		slog.Info("stopping udocker workload process group", "container", id, "pid", pid)
	}
	return d.procs().Stop(ctx, id, 3*time.Second)
}

// procs is the tracker of this driver's workloads. Tests point stateDir
// elsewhere after construction, so the tracker follows it.
func (d *Driver) procs() *procgroup.Tracker {
	d.procMu.Lock()
	defer d.procMu.Unlock()
	if d.tracker.Dir != d.stateDir {
		d.tracker.Dir = d.stateDir
	}
	return &d.tracker
}

// Remove stops and deletes the udocker container.
//
// `udocker rm` fails on this deployment shape, and its failure is not an error
// condition this call should propagate - it is the expected outcome, so it
// costs a log line rather than the teardown. What it reports is:
//
//	Info: deleting container: <id>
//	Error: deleting directory:  <UDOCKER_DIR>/containers/<id>
//	Error: deleting container
//
// ROOT CAUSE (established 2026-09-14). The container ROOT holds entries owned
// by root - the agent's own uid - while `udocker rm` runs as the run user
// through runuser. rootfsFixupsWithNVIDIA() (helpers.go) runs IN the agent
// process and writes into the container ROOT *after* the unpack has already
// handed every entry to the run user:
//
//	ROOT/etc/hosts                 atomicWriteFile() = os.CreateTemp + os.Rename
//	ROOT/opt/nvidia-shim/          os.MkdirAll + four copyDriverLib() files
//	ROOT/<UDOCKER_DIR>/…/containers/  ensureHostRootLoopback()'s os.MkdirAll chain
//
// atomicWriteFile never chowns, so those entries stay root-owned. upstream
// FileUtil._removedir() chmods every file before unlinking it, and chmod on a
// root-owned file is EPERM, which its bare `except OSError` swallows into
// `return False`; the CLI then reports a directory it could not delete without
// saying why. ROOT/etc/hosts is written UNCONDITIONALLY, so every container is
// affected, not only the ones that need the NVIDIA shim.
//
// This supersedes two earlier readings of this comment: the first blamed a
// root-owned ROOT and was withdrawn, the second declared the cause unknown. The
// ownership problem is real, but it is INSIDE ROOT rather than at the container
// directory or at ROOT itself - both of which the unpack does hand to the run
// user (measured 2026-09-13: 1002:1002, mode 755) - which is why measuring only
// those two levels kept coming back clean. The failure was reproduced verbatim
// on the validation node by recreating the fixup artifacts and running the real
// CLI: `chmod` on ROOT/etc/hosts fails with EPERM (errno 1), then the walk
// aborts and the CLI prints the same three lines quoted above.
//
// The os.RemoveAll below therefore reclaims the rootfs on this shape because it
// runs as the agent's root rather than through runuser - root needs no
// permission to unlink a file it owns. Its result, not the CLI's, decides this
// call's return value. Fixing the ownership at the source - chown the fixup
// artifacts as they are written, or chown the tree once the fixups are done -
// would let the CLI succeed as well; until then this fallback is load-bearing
// rather than decorative, and it is the reason a workload that never runs as
// root cannot be torn down here.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." || strings.HasPrefix(id, "-") || strings.ContainsRune(id, 0) {
		return fmt.Errorf("invalid container name %q", id)
	}
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	// The failure of this command is expected and explained in the doc comment,
	// so it is logged once for the operator without the generic runtime-command
	// warning that would otherwise mark every single teardown as a problem.
	if d.runveil {
		// runveil owns the container directory outright; there is no CLI
		// bookkeeping to undo.
		if err := d.removeContainerDir(id); err != nil {
			return fmt.Errorf("remove container rootfs %s: %w", id, err)
		}
		d.forgetState(id)
		return nil
	}
	containerDir := d.containerDir(id)
	if parent, err := os.Lstat(filepath.Dir(containerDir)); err == nil && !parent.IsDir() {
		return fmt.Errorf("udocker containers path is not a real directory")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect udocker containers directory: %w", err)
	}
	before, statErr := removeContainerLstat(containerDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect udocker container %s: %w", id, statErr)
	}
	var aliasTarget, aliasRoot string
	if statErr == nil && before.Mode()&os.ModeSymlink != 0 {
		var err error
		aliasTarget, err = os.Readlink(containerDir)
		if err != nil {
			return fmt.Errorf("read udocker container alias %s: %w", id, err)
		}
		containers, err := filepath.EvalSymlinks(filepath.Dir(containerDir))
		if err != nil {
			return fmt.Errorf("resolve udocker containers directory: %w", err)
		}
		target := aliasTarget
		if !filepath.IsAbs(target) {
			if filepath.Base(target) != target || target == "." || target == ".." {
				return fmt.Errorf("udocker container alias %s does not name a direct managed container directory", id)
			}
			target = filepath.Join(filepath.Dir(containerDir), target)
		} else if filepath.Clean(target) != target {
			return fmt.Errorf("udocker container alias %s has an unsafe target", id)
		}
		targetParent, err := filepath.EvalSymlinks(filepath.Dir(target))
		if err != nil || targetParent != containers {
			return fmt.Errorf("udocker container alias %s does not name a direct managed container directory", id)
		}
		aliasRoot = filepath.Join(containers, filepath.Base(target))
		root, rootErr := removeContainerLstat(aliasRoot)
		if rootErr != nil && !os.IsNotExist(rootErr) {
			return fmt.Errorf("inspect udocker alias target %s: %w", id, rootErr)
		}
		if rootErr == nil && !root.IsDir() {
			return fmt.Errorf("udocker alias target %s is not a real directory; chained links are not followed", id)
		}
	}
	output, rmErr := d.udoExpected(ctx, "rm", id)
	if rmErr != nil {
		slog.Info("udocker rm did not delete the container; falling back to a privileged rootfs removal", "container", id, "err", rmErr)
	}
	// Older udocker releases can also report success while leaving the
	// materialized ROOT behind (and report "not found" when only metadata was
	// lost). Reap the exact managed container directory as a bounded fallback;
	// never broaden the target beyond UDOCKER_DIR/containers/<id>.
	info, statErr := removeContainerLstat(containerDir)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect udocker container %s: %w", id, statErr)
	}
	if aliasRoot != "" {
		_, rootErr := removeContainerLstat(aliasRoot)
		if !os.IsNotExist(rootErr) {
			// Some CLI failures unlink the name before removing its ROOT. Put
			// back only that captured alias so the next retry cannot mistake the
			// remaining rootfs for an already absent container. Never remove
			// through the alias or overwrite a replacement name.
			if os.IsNotExist(statErr) {
				if err := os.Symlink(aliasTarget, containerDir); err != nil {
					return fmt.Errorf("restore failed udocker container alias %s: %w", id, err)
				}
			}
			if cause := errors.Join(rmErr, rootErr); cause != nil {
				return fmt.Errorf("udocker container rootfs behind alias %s is not confirmed absent: %w", id, cause)
			}
			return fmt.Errorf("udocker container rootfs behind alias %s remains after removal", id)
		}
	}
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if aliasRoot == "" || !os.SameFile(before, info) {
				return fmt.Errorf("udocker container alias %s changed during removal", id)
			}
			if target, err := os.Readlink(containerDir); err != nil || target != aliasTarget {
				return fmt.Errorf("udocker container alias %s target changed during removal", id)
			}
			if rmErr != nil && !missingContainerOutput(output) {
				return fmt.Errorf("remove udocker container %s: %w", id, rmErr)
			}
			// The direct managed target was confirmed absent above. Remove only
			// this unchanged leftover link, never the path it used to reference.
			if err := os.Remove(containerDir); err != nil {
				return fmt.Errorf("remove verified dangling udocker alias %s: %w", id, err)
			}
			statErr = os.ErrNotExist
		}
	}
	if statErr == nil {
		if aliasRoot != "" {
			return fmt.Errorf("udocker container alias %s was replaced during removal", id)
		}
		if before == nil || !os.SameFile(before, info) {
			return fmt.Errorf("udocker container %s appeared or changed during removal", id)
		}
		slog.Info("removing udocker container rootfs", "container", id, "dir", containerDir)
		if removeErr := os.RemoveAll(containerDir); removeErr != nil {
			return fmt.Errorf("remove udocker container rootfs %s: %w", id, removeErr)
		}
		// The exact managed directory was reclaimed without following an
		// alias, so its state no longer depends on the failed CLI removal.
		rmErr = nil
	}
	if rmErr != nil {
		// udocker reports an error for an already removed container. Treat that
		// case as idempotent, but propagate real cleanup failures so reconcile
		// does not report a healthy deletion when rootfs cleanup failed.
		if !missingContainerOutput(output) {
			return fmt.Errorf("remove udocker container %s: %w", id, rmErr)
		}
	}
	d.removeWorkloadTmp(id)
	d.forgetState(id)
	return nil
}

// removeContainerLstat lets tests exercise permission errors even as root.
var removeContainerLstat = os.Lstat

func missingContainerOutput(output string) bool {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "error: invalid container id", "error: container not found", "error: no such container":
		return true
	default:
		return false
	}
}

// forgetState drops the tracking files of a removed container.
func (d *Driver) forgetState(id string) {
	// The process was stopped before this; Forget lets its reaper write the
	// status first so the file is not left behind for the next container.
	d.procs().Forget(id)
	_ = os.Remove(d.LogPath(id))
	_ = os.Remove(d.SpecPath(id))
	d.specMu.Lock()
	delete(d.specs, id)
	d.specMu.Unlock()
}

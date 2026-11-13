package udocker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// RuntimeDeviceGroup owns accelerator device nodes that the host leaves
// root-only (root:root with no group or other access, as some Ascend hosts
// ship davinci_manager, devmm_svm and hisi_hdc). A root agent hands such a
// node to this group with group read/write so the non-root workload user can
// reach it through a supplementary group; nothing else on the host changes.
const RuntimeDeviceGroup = "gpuworks-runtime"

// rootOnlyDevice reports whether a device node is owned by root:root and
// grants neither group nor other any access. Only such nodes are adopted into
// RuntimeDeviceGroup: a node with a real owning group, or with any group or
// other bits already set, keeps the host's policy.
func rootOnlyDevice(info os.FileInfo) bool {
	if info.Mode()&os.ModeDevice == 0 {
		return false
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata == nil {
		return false
	}
	return metadata.Uid == 0 && metadata.Gid == 0 && info.Mode().Perm()&0o066 == 0
}

// deviceGroupOps are the host operations adoptRootOnlyDevices performs,
// injectable so tests exercise failures without touching real device nodes.
type deviceGroupOps struct {
	stat        func(string) (os.FileInfo, error)
	lookupGroup func(string) (*user.Group, error)
	addGroup    func(context.Context, string) error
	chown       func(string, int, int) error
	chmod       func(string, os.FileMode) error
}

func hostDeviceGroupOps() deviceGroupOps {
	return deviceGroupOps{
		stat:        os.Stat,
		lookupGroup: user.LookupGroup,
		addGroup: func(ctx context.Context, name string) error {
			slog.Info("runtime command", "runtime", "udocker", "args", []string{"groupadd", "--system", name})
			if out, err := exec.CommandContext(ctx, "groupadd", "--system", name).CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		chown: os.Chown,
		chmod: os.Chmod,
	}
}

// adoptRootOnlyDevices gives every root-only device node among mounts to
// RuntimeDeviceGroup with group read/write, creating the group on first use.
// It must run as root, and runs before every workload start because device
// nodes are recreated with their original modes when the host reboots or the
// driver reloads. It returns the adopted paths.
func adoptRootOnlyDevices(ctx context.Context, mounts []runtime.Mount, ops deviceGroupOps) ([]string, error) {
	var adopted []string
	gid := -1
	for _, mount := range mounts {
		info, err := ops.stat(mount.Source)
		if err != nil || !rootOnlyDevice(info) {
			continue
		}
		if gid < 0 {
			if gid, err = ensureGroup(ctx, RuntimeDeviceGroup, ops); err != nil {
				return adopted, err
			}
		}
		if err := ops.chown(mount.Source, -1, gid); err != nil {
			return adopted, fmt.Errorf("give root-only accelerator device %s to group %s: %w", mount.Source, RuntimeDeviceGroup, err)
		}
		if err := ops.chmod(mount.Source, info.Mode().Perm()|0o060); err != nil {
			return adopted, fmt.Errorf("grant group %s read/write on accelerator device %s: %w", RuntimeDeviceGroup, mount.Source, err)
		}
		adopted = append(adopted, mount.Source)
	}
	if len(adopted) != 0 {
		slog.Info("accelerator devices adopted into runtime group", "group", RuntimeDeviceGroup, "devices", adopted)
	}
	return adopted, nil
}

// ensureGroup returns the id of name, creating it as a system group when it
// does not exist. A concurrent creation is tolerated by looking it up again.
func ensureGroup(ctx context.Context, name string, ops deviceGroupOps) (int, error) {
	group, err := ops.lookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if !errors.As(err, &unknown) {
			return -1, fmt.Errorf("look up group %s: %w", name, err)
		}
		addErr := ops.addGroup(ctx, name)
		if group, err = ops.lookupGroup(name); err != nil {
			if addErr != nil {
				return -1, fmt.Errorf("create group %s: %w", name, addErr)
			}
			return -1, fmt.Errorf("look up group %s after creating it: %w", name, err)
		}
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return -1, fmt.Errorf("parse group %s id %q: %w", name, group.Gid, err)
	}
	return gid, nil
}

// prepareDeviceAccess adopts root-only device nodes before a workload starts
// when this root agent runs workloads as a dedicated user. A non-root agent
// cannot change device ownership and leaves the decision to
// requiredDeviceGroups, which names the fix.
func (d *Driver) prepareDeviceAccess(ctx context.Context, spec runtime.WorkloadSpec) error {
	if d.runUser == "" || os.Geteuid() != 0 {
		return nil
	}
	_, err := adoptRootOnlyDevices(ctx, workloadDeviceMounts(spec), hostDeviceGroupOps())
	return err
}

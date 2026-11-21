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
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// RuntimeDeviceGroup owns accelerator device nodes that the host leaves
// root-only (root:root with no group or other access, as some Ascend hosts
// ship davinci_manager, devmm_svm and hisi_hdc). A root agent hands such a
// node to this group with group read/write so the non-root workload user can
// reach it through a supplementary group; nothing else on the host changes.
const RuntimeDeviceGroup = "gpuworks-runtime"

// adoptedDeviceGroupID stands in for the id of RuntimeDeviceGroup while it is
// only predicted: diagnostics run before adoption has created the group, so
// there is no id to report yet. No real group has a negative id.
const adoptedDeviceGroupID = -1

// suggestedDeviceGroupName is the name an operator is told to give a device
// gid the host leaves out of its group database, so that the launchers which
// can only be given names have one.
func suggestedDeviceGroupName(gid int) string {
	return "gpuworks-device-" + strconv.Itoa(gid)
}

// lookupGroupID resolves a numeric group id through the host's group database.
// The agent is built without cgo, so os/user only parses /etc/group; a host
// that serves groups from LDAP or SSSD answers through getent instead, and
// without this fallback such a group would look nameless.
func lookupGroupID(gid string) (*user.Group, error) {
	group, err := user.LookupGroupId(gid)
	if err == nil {
		return group, nil
	}
	// Every failure gets the second opinion, not only "unknown id": a host
	// whose /etc/group is missing or unreadable still answers through getent,
	// and os/user reports that one as a path error.
	if name := getentGroupName(gid); name != "" {
		return &user.Group{Name: name, Gid: gid}, nil
	}
	return nil, err
}

// getentGroupName returns the name NSS has for gid, or "" when it has none.
// Any failure is reported as "no name": the caller's own lookup already said
// the same, and a nameless group is a condition the launchers handle rather
// than an error to propagate.
func getentGroupName(gid string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getent", "group", gid).Output()
	if err != nil {
		return ""
	}
	// name:password:gid:member,member
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 3 || fields[2] != gid {
		return ""
	}
	return fields[0]
}

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

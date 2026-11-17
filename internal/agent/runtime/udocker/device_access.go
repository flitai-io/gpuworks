package udocker

import (
	"context"
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

// DeviceAccessDiagnostics checks the identity that will execute udocker
// workloads against the actual accelerator device nodes selected by the GPU
// prober. It does not change the host.
func (d *Driver) DeviceAccessDiagnostics(spec runtime.WorkloadSpec) string {
	d.provisionMu.Lock()
	provisioned := d.provisioned
	runUser, runUID, runGID := d.runUser, d.runUID, d.runGID
	d.provisionMu.Unlock()

	canInjectGroups := runUser != ""
	if !provisioned && os.Geteuid() == 0 {
		account, err := user.Lookup("udocker")
		if err != nil {
			return "device access: ready (root agent will create the non-root udocker user and supply device groups)"
		}
		runUser = account.Username
		runUID, err = strconv.Atoi(account.Uid)
		if err != nil {
			return "device access: FAIL: parse udocker uid: " + err.Error()
		}
		runGID, err = strconv.Atoi(account.Gid)
		if err != nil {
			return "device access: FAIL: parse udocker gid: " + err.Error()
		}
		canInjectGroups = true
	}
	groupIDs, err := identityGroupIDs(runUser, runGID)
	if err != nil {
		return "device access: FAIL: " + err.Error()
	}
	willAdopt := canInjectGroups && os.Geteuid() == 0
	groups, devices, err := requiredDeviceGroups(workloadDeviceMounts(spec), runUID, runGID, groupIDs, os.Stat, user.LookupGroupId, willAdopt)
	if err != nil {
		return "device access: FAIL: " + err.Error()
	}
	if devices == 0 {
		return "device access: no accelerator device nodes found for the selected workload"
	}
	identity := runUser
	if identity == "" {
		identity = strconv.Itoa(runUID)
	}
	if len(groups) == 0 {
		return fmt.Sprintf("device access: PASS (%s can read and write %d required device nodes)", identity, devices)
	}
	if canInjectGroups {
		return fmt.Sprintf("device access: PASS (%s will receive device group(s) %s for %d required device nodes)", identity, strings.Join(groups, ","), devices)
	}
	return fmt.Sprintf("device access: FAIL (%s lacks group(s) %s; run sudo usermod -aG %s %s, then restart the agent)", identity, strings.Join(groups, ","), strings.Join(groups, ","), identity)
}

// GrantDeviceAccess adds username to the groups that own the selected
// accelerator device nodes, first handing root-only nodes to
// RuntimeDeviceGroup. The caller must run this method with root privileges;
// the workload itself continues to run as username. Device nodes regain their
// original modes on reboot: a root agent re-adopts them before every workload,
// while a non-root agent needs this command again.
func (d *Driver) GrantDeviceAccess(ctx context.Context, spec runtime.WorkloadSpec, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", fmt.Errorf("runtime username is required")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return "", fmt.Errorf("resolve runtime user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return "", fmt.Errorf("parse uid for %s: %w", username, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return "", fmt.Errorf("parse gid for %s: %w", username, err)
	}
	if os.Geteuid() == 0 {
		if _, err := adoptRootOnlyDevices(ctx, workloadDeviceMounts(spec), hostDeviceGroupOps()); err != nil {
			return "", err
		}
	}
	groupIDs, err := identityGroupIDs(username, gid)
	if err != nil {
		return "", err
	}
	groups, devices, err := requiredDeviceGroups(workloadDeviceMounts(spec), uid, gid, groupIDs, os.Stat, user.LookupGroupId, false)
	if err != nil {
		return "", err
	}
	if devices == 0 {
		return "no accelerator device nodes require group access", nil
	}
	if len(groups) == 0 {
		return fmt.Sprintf("%s already has read/write access to %d accelerator device nodes", username, devices), nil
	}
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("root is required to add %s to accelerator device group(s) %s", username, strings.Join(groups, ","))
	}
	slog.Info("runtime command", "runtime", "udocker", "args", []string{"usermod", "-aG", strings.Join(groups, ","), username})
	if out, err := exec.CommandContext(ctx, "usermod", "-aG", strings.Join(groups, ","), username).CombinedOutput(); err != nil {
		return "", fmt.Errorf("add %s to accelerator device group(s) %s: %w: %s", username, strings.Join(groups, ","), err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("added %s to accelerator device group(s) %s; workloads remain non-root", username, strings.Join(groups, ",")), nil
}

// workloadInvocation preserves the dedicated udocker user's primary group and
// adds the groups that own accelerator device nodes. PRoot bind mounts expose
// the host device inode unchanged, so a mount alone is insufficient when the
// workload user is not a member of the device's group.
func (d *Driver) workloadInvocation(args []string, spec runtime.WorkloadSpec) ([]string, error) {
	if err := d.prepareDeviceAccess(context.Background(), spec); err != nil {
		return nil, err
	}
	groupIDs, err := identityGroupIDs(d.runUser, d.runGID)
	if err != nil {
		return nil, err
	}
	groups, _, err := requiredDeviceGroups(workloadDeviceMounts(spec), d.runUID, d.runGID, groupIDs, os.Stat, user.LookupGroupId, false)
	if err != nil {
		return nil, err
	}
	if d.runUser == "" {
		if len(groups) != 0 {
			current, _ := user.Current()
			name := strconv.Itoa(d.runUID)
			if current != nil && current.Username != "" {
				name = current.Username
			}
			return nil, fmt.Errorf("runtime user %s lacks accelerator device group(s) %s; run sudo usermod -aG %s %s, then restart the agent", name, strings.Join(groups, ","), strings.Join(groups, ","), name)
		}
		return d.udockerInvocation(args), nil
	}
	if len(groups) == 0 {
		return d.udockerInvocation(args), nil
	}
	primary, err := user.LookupGroupId(strconv.Itoa(d.runGID))
	if err != nil {
		return nil, fmt.Errorf("resolve udocker primary group %d: %w", d.runGID, err)
	}
	invocation := []string{"runuser", "-u", d.runUser, "-g", primary.Name}
	for _, group := range groups {
		invocation = append(invocation, "-G", group)
	}
	full := d.udockerArgs(args...)
	return append(invocation, "--", "bash", "-lc", d.shellInvocation(full)), nil
}

// requiredDeviceGroups returns the device groups runUID still needs for the
// accelerator device nodes among mounts. willAdopt predicts that a root agent
// will hand root-only nodes to RuntimeDeviceGroup before the workload starts
// (see adoptRootOnlyDevices), so they count as needing that group instead of
// being unreachable.
func requiredDeviceGroups(
	mounts []runtime.Mount,
	runUID, runGID int,
	supplementaryGroupIDs map[int]bool,
	stat func(string) (os.FileInfo, error),
	lookupGroup func(string) (*user.Group, error),
	willAdopt bool,
) ([]string, int, error) {
	seen := map[string]bool{}
	groups := make([]string, 0, 1)
	devices := 0
	for _, mount := range mounts {
		info, err := stat(mount.Source)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			continue
		}
		devices++
		metadata, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, devices, fmt.Errorf("inspect accelerator device ownership %s", mount.Source)
		}
		permissions := info.Mode().Perm()
		ownerAccess := int(metadata.Uid) == runUID && permissions&0o600 == 0o600
		groupAccess := (int(metadata.Gid) == runGID || supplementaryGroupIDs[int(metadata.Gid)]) && permissions&0o060 == 0o060
		worldAccess := permissions&0o006 == 0o006
		if ownerAccess || groupAccess || worldAccess {
			continue
		}
		if rootOnlyDevice(info) {
			if !willAdopt {
				return nil, devices, fmt.Errorf("runtime user %s cannot read and write root-only accelerator device %s; run sudo gpuworks-agent grant-device-access --user <runtime user> to give it to group %s (repeat after a reboot)", strconv.Itoa(runUID), mount.Source, RuntimeDeviceGroup)
			}
			if !seen[RuntimeDeviceGroup] {
				seen[RuntimeDeviceGroup] = true
				groups = append(groups, RuntimeDeviceGroup)
			}
			continue
		}
		if permissions&0o060 != 0o060 {
			return nil, devices, fmt.Errorf("runtime user %s cannot read and write accelerator device %s", strconv.Itoa(runUID), mount.Source)
		}
		gid := strconv.FormatUint(uint64(metadata.Gid), 10)
		group, err := lookupGroup(gid)
		if err != nil {
			return nil, devices, fmt.Errorf("resolve accelerator device group %s for %s: %w", gid, mount.Source, err)
		}
		if !seen[group.Name] {
			seen[group.Name] = true
			groups = append(groups, group.Name)
		}
	}
	return groups, devices, nil
}

func identityGroupIDs(username string, primaryGID int) (map[int]bool, error) {
	ids := map[int]bool{primaryGID: true}
	if username == "" {
		groups, err := os.Getgroups()
		if err != nil {
			return nil, fmt.Errorf("resolve current supplementary groups: %w", err)
		}
		for _, gid := range groups {
			ids[gid] = true
		}
		return ids, nil
	}
	account, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime user %s: %w", username, err)
	}
	groups, err := account.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("resolve supplementary groups for %s: %w", username, err)
	}
	for _, raw := range groups {
		gid, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return nil, fmt.Errorf("parse group id %q for %s: %w", raw, username, convErr)
		}
		ids[gid] = true
	}
	return ids, nil
}

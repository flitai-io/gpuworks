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
	needs, err := requiredDeviceGroups(workloadDeviceMounts(spec), runUID, runGID, groupIDs, os.Stat, lookupGroupID, willAdopt)
	if err != nil {
		return "device access: FAIL: " + err.Error()
	}
	if needs.devices == 0 {
		return "device access: no accelerator device nodes found for the selected workload"
	}
	identity := runUser
	if identity == "" {
		identity = strconv.Itoa(runUID)
	}
	if needs.empty() {
		return fmt.Sprintf("device access: PASS (%s can read and write %d required device nodes)", identity, needs.devices)
	}
	labels := needs.labels()
	if !canInjectGroups {
		return fmt.Sprintf("device access: FAIL (%s lacks group(s) %s; run %s, then restart the agent)", identity, strings.Join(labels, ","), needs.membershipAdvice(identity))
	}
	// The runveil launcher sets supplementary groups numerically, so a group
	// the host cannot name is no obstacle there. The udocker CLI launcher
	// goes through runuser and needs every name.
	if !d.runveil {
		if _, err := needs.names(identity); err != nil {
			return "device access: FAIL: " + err.Error()
		}
	}
	return fmt.Sprintf("device access: PASS (%s will receive device group(s) %s for %d required device nodes)", identity, strings.Join(labels, ","), needs.devices)
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
	needs, err := requiredDeviceGroups(workloadDeviceMounts(spec), uid, gid, groupIDs, os.Stat, lookupGroupID, false)
	if err != nil {
		return "", err
	}
	if needs.devices == 0 {
		return "no accelerator device nodes require group access", nil
	}
	if needs.empty() {
		return fmt.Sprintf("%s already has read/write access to %d accelerator device nodes", username, needs.devices), nil
	}
	groups, err := needs.names(username)
	if err != nil {
		return "", err
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
	needs, err := requiredDeviceGroups(workloadDeviceMounts(spec), d.runUID, d.runGID, groupIDs, os.Stat, lookupGroupID, false)
	if err != nil {
		return nil, err
	}
	if d.runUser == "" {
		if !needs.empty() {
			current, _ := user.Current()
			name := strconv.Itoa(d.runUID)
			if current != nil && current.Username != "" {
				name = current.Username
			}
			groups, nameErr := needs.names(name)
			if nameErr != nil {
				return nil, nameErr
			}
			return nil, fmt.Errorf("runtime user %s lacks accelerator device group(s) %s; run sudo usermod -aG %s %s, then restart the agent", name, strings.Join(groups, ","), strings.Join(groups, ","), name)
		}
		return d.udockerInvocation(args), nil
	}
	if needs.empty() {
		return d.udockerInvocation(args), nil
	}
	groups, err := needs.names(d.runUser)
	if err != nil {
		return nil, err
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

// deviceGroup is one group that owns an accelerator device node the runtime
// identity cannot reach on its own. gid is negative for a group a root agent
// is predicted to create (RuntimeDeviceGroup, whose id is not known until
// adoption runs). name is empty when the host's group database has no entry
// for gid at all: a node that receives its device nodes with a numeric owner
// only -- an Ascend host passing /dev/davinci* into a container, say -- has
// group-writable devices whose gid nothing can name.
type deviceGroup struct {
	gid  int
	name string
}

// deviceGroupNeeds is the groups one workload needs, in mount order, plus the
// number of device nodes inspected.
type deviceGroupNeeds struct {
	groups  []deviceGroup
	devices int
}

func (n deviceGroupNeeds) empty() bool { return len(n.groups) == 0 }

// labels describes the groups for a human: a name where the host has one, the
// numeric id where it does not.
func (n deviceGroupNeeds) labels() []string {
	out := make([]string, 0, len(n.groups))
	for _, group := range n.groups {
		if group.name != "" {
			out = append(out, group.name)
			continue
		}
		out = append(out, strconv.Itoa(group.gid))
	}
	return out
}

// names returns every group name for a launcher that can only be given names
// (`runuser -G`, `usermod -aG`). A nameless group stops those paths, so the
// error names the one command that gives the gid a name; the runveil launcher
// needs no name and never calls this.
// membershipAdvice reports the commands that make identity a member of every
// group it is missing, for the launcher that cannot inject supplementary
// groups and therefore needs the membership to exist beforehand. A group the
// host cannot name has to be given one first: usermod only takes names.
func (n deviceGroupNeeds) membershipAdvice(identity string) string {
	commands := make([]string, 0, len(n.groups)+1)
	names := make([]string, 0, len(n.groups))
	for _, group := range n.groups {
		name := group.name
		if name == "" {
			name = suggestedDeviceGroupName(group.gid)
			commands = append(commands, fmt.Sprintf("sudo groupadd --system --gid %d %s", group.gid, name))
		}
		names = append(names, name)
	}
	commands = append(commands, fmt.Sprintf("sudo usermod -aG %s %s", strings.Join(names, ","), identity))
	return strings.Join(commands, " && ")
}

func (n deviceGroupNeeds) names(identity string) ([]string, error) {
	out := make([]string, 0, len(n.groups))
	for _, group := range n.groups {
		if group.name == "" {
			suggested := suggestedDeviceGroupName(group.gid)
			return nil, fmt.Errorf("accelerator device group %d has no name on this host and this runtime can only be given named groups; run sudo groupadd --system --gid %d %s && sudo usermod -aG %s %s, then restart the agent", group.gid, group.gid, suggested, suggested, identity)
		}
		out = append(out, group.name)
	}
	return out, nil
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
) (deviceGroupNeeds, error) {
	seen := map[int]bool{}
	needs := deviceGroupNeeds{}
	for _, mount := range mounts {
		info, err := stat(mount.Source)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			continue
		}
		needs.devices++
		metadata, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return needs, fmt.Errorf("inspect accelerator device ownership %s", mount.Source)
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
				return needs, fmt.Errorf("runtime user %s cannot read and write root-only accelerator device %s; run sudo gpuworks-agent grant-device-access --user <runtime user> to give it to group %s (repeat after a reboot)", strconv.Itoa(runUID), mount.Source, RuntimeDeviceGroup)
			}
			if !seen[adoptedDeviceGroupID] {
				seen[adoptedDeviceGroupID] = true
				needs.groups = append(needs.groups, deviceGroup{gid: adoptedDeviceGroupID, name: RuntimeDeviceGroup})
			}
			continue
		}
		if permissions&0o060 != 0o060 {
			return needs, fmt.Errorf("runtime user %s cannot read and write accelerator device %s", strconv.Itoa(runUID), mount.Source)
		}
		gid := int(metadata.Gid)
		if seen[gid] {
			continue
		}
		seen[gid] = true
		group, err := lookupGroup(strconv.Itoa(gid))
		if err != nil {
			// A gid the group database cannot name is still a usable
			// supplementary group: the launcher that sets it numerically
			// reaches the device, and only a name-taking launcher has to
			// stop. Any other lookup failure is the host misbehaving.
			var unknown user.UnknownGroupIdError
			if !errors.As(err, &unknown) {
				return needs, fmt.Errorf("resolve accelerator device group %d for %s: %w", gid, mount.Source, err)
			}
			needs.groups = append(needs.groups, deviceGroup{gid: gid})
			continue
		}
		needs.groups = append(needs.groups, deviceGroup{gid: gid, name: group.Name})
	}
	return needs, nil
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

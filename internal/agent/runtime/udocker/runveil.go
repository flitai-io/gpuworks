package udocker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/runveil"
	"gpuworks.ai/gpuworks/internal/proto"
)

// launcherEnv selects who executes containers. runveil, the default, launches
// the patched PRoot directly from Go and needs no Python; "udocker" restores
// the udocker CLI launcher, which does. Image handling is native either way.
const launcherEnv = "GPUWORKS_UDOCKER_LAUNCHER"

func runveilSelected() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(launcherEnv)), "udocker")
}

func (d *Driver) launcherName() string { return launcherNameOf(d.runveil) }

// LauncherName is this runtime as operators know it: runveil, or udocker
// under the udocker CLI launcher. The driver's wire name stays "udocker" for
// existing nodes and API callers.
func LauncherName() string { return launcherNameOf(runveilSelected()) }

func launcherNameOf(runveil bool) string {
	if runveil {
		return "runveil"
	}
	return "udocker"
}

func (d *Driver) containerDir(name string) string {
	return filepath.Join(d.udockerDir, "containers", name)
}

// runveilPrepare is prepareWithProgress without the udocker CLI on the
// container path: existence is the ROOT directory, `setup --nvidia` is
// runveil.SetupNVIDIA, and `setup --execmode=P1` is unnecessary because P1 is
// the only mode runveil implements. Only a failed native create still falls
// back to `udocker create`.
func (d *Driver) runveilPrepare(ctx context.Context, spec runtime.WorkloadSpec, report func(phase, message string)) error {
	name := spec.Name
	created := false
	cleanupPartial := func() {
		if created {
			if err := d.removeContainerDir(name); err != nil {
				slog.Warn("remove partial container after preparation failure", "container", name, "err", err)
			}
		}
	}
	if !isDir(filepath.Join(d.containerDir(name), "ROOT")) {
		report(proto.PhaseContainerUnpack, "unpacking image layers into container root filesystem")
		created = true
		handled, err := d.nativeCreate(ctx, name, spec.Image, func(line string) { report(proto.PhaseContainerUnpack, line) })
		switch {
		case handled && err != nil:
			cleanupPartial()
			return err
		case handled:
			report(proto.PhaseContainerUnpack, "container root filesystem ready")
		default:
			cliImage, _ := udockerCLIImage(spec.Image)
			if _, err := d.udo(ctx, "create", "--name="+name, cliImage); err != nil {
				cleanupPartial()
				return fmt.Errorf("udocker create: %w", err)
			}
		}
	}
	if needsNVIDIASetup(spec) {
		report("container_gpu_setup", "configuring NVIDIA runtime support")
		ldconfig, _ := exec.CommandContext(ctx, "ldconfig", "-p").Output()
		note, err := runveil.SetupNVIDIA(d.containerDir(name), runveil.HostNVIDIALibDirs(string(ldconfig), os.Getenv("LD_LIBRARY_PATH")))
		if err != nil {
			// udocker ignores a failed NVIDIA setup as well; the /opt/nvidia-shim
			// fixup below still provides the libraries CUDA needs to start.
			slog.Warn("runveil nvidia setup failed", "container", name, "err", err)
		} else {
			slog.Info("runveil nvidia setup", "container", name, "result", note)
		}
	}
	report("container_fixup", "applying GPU runtime compatibility fixes")
	if err := d.rootfsFixupsWithNVIDIA(ctx, name, needsNVIDIASetup(spec)); err != nil {
		cleanupPartial()
		return err
	}
	return nil
}

// runveilCommand builds the launch for spec. entrypoint nil keeps the image
// entrypoint; workdir "" keeps the image working directory.
func (d *Driver) runveilCommand(spec runtime.WorkloadSpec, entrypoint, cmd []string, workdir string) (*runveil.Command, []uint32, error) {
	identity, err := d.runveilIdentity(spec)
	if err != nil {
		return nil, nil, err
	}
	binds := make([]runveil.Bind, 0, len(spec.Mounts)+4)
	for _, m := range d.workloadMounts(spec) {
		binds = append(binds, runveil.Bind{Host: m.Source, Guest: m.Target})
	}
	env := d.runEnv(spec)
	binds, err = d.ascendRunveilCompatibility(spec, env, binds)
	if err != nil {
		return nil, nil, err
	}
	command, err := runveil.Prepare(runveil.Spec{
		Proot:        d.prootPath,
		ContainerDir: d.containerDir(spec.Name),
		Name:         spec.Name,
		Env:          env,
		Binds:        binds,
		Entrypoint:   entrypoint,
		Cmd:          cmd,
		Workdir:      workdir,
		HostEnv:      os.Environ(),
		Identity:     identity,
	})
	if err != nil {
		return nil, nil, err
	}
	groups := make([]uint32, len(identity.Groups))
	for i, gid := range identity.Groups {
		groups[i] = uint32(gid)
	}
	return command, groups, nil
}

// runveilIdentity is the account PRoot runs as and its supplementary groups:
// the run user's own groups plus every group that owns a bound accelerator
// device node. The udocker CLI path gets the same set from `runuser -G`.
func (d *Driver) runveilIdentity(spec runtime.WorkloadSpec) (runveil.Identity, error) {
	if err := d.prepareDeviceAccess(context.Background(), spec); err != nil {
		return runveil.Identity{}, err
	}
	owned, err := identityGroupIDs(d.runUser, d.runGID)
	if err != nil {
		return runveil.Identity{}, err
	}
	missing, _, err := requiredDeviceGroups(workloadDeviceMounts(spec), d.runUID, d.runGID, owned, os.Stat, user.LookupGroupId, false)
	if err != nil {
		return runveil.Identity{}, err
	}
	name := d.runUser
	if name == "" {
		name = strconv.Itoa(d.runUID)
		if current, err := user.Current(); err == nil && current.Username != "" {
			name = current.Username
		}
		if len(missing) != 0 {
			return runveil.Identity{}, fmt.Errorf("runtime user %s lacks accelerator device group(s) %s; run sudo usermod -aG %s %s, then restart the agent", name, strings.Join(missing, ","), strings.Join(missing, ","), name)
		}
	}
	for _, group := range missing {
		resolved, err := user.LookupGroup(group)
		if err != nil {
			return runveil.Identity{}, fmt.Errorf("resolve device group %s: %w", group, err)
		}
		gid, err := strconv.Atoi(resolved.Gid)
		if err != nil {
			return runveil.Identity{}, fmt.Errorf("parse device group %s id %q: %w", group, resolved.Gid, err)
		}
		owned[gid] = true
	}
	delete(owned, d.runGID)
	groups := make([]int, 0, len(owned))
	for gid := range owned {
		groups = append(groups, gid)
	}
	sort.Ints(groups)
	return runveil.Identity{UID: d.runUID, GID: d.runGID, Groups: groups, Username: name}, nil
}

// processDir is the working directory of every PRoot and udocker process the
// agent starts. Without it they inherit the agent's own cwd, and an agent that
// was started in /root (mode 700) leaves a PRoot running as the dedicated
// udocker user unable to chdir back there ("can't chdir to '/root'"). The
// container's own working directory is set separately (PRoot -w).
const processDir = "/"

// runveilExec turns a command into an exec.Cmd running as the run user, in
// its own process group so Stop can signal the whole tree.
func (d *Driver) runveilExec(ctx context.Context, command *runveil.Command, groups []uint32) *exec.Cmd {
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(command.Argv[0], command.Argv[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	}
	cmd.Env = command.Env
	cmd.Dir = processDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if d.runUser != "" {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(d.runUID), Gid: uint32(d.runGID), Groups: groups}
	}
	return cmd
}

// runveilPTYArgv is the terminal argv: the terminal package owns the process
// and its environment, so identity and environment travel in the argv itself
// (runuser for the account, `env -i` for an exact environment).
func (d *Driver) runveilPTYArgv(spec runtime.WorkloadSpec, cmd []string) ([]string, error) {
	entrypoint := []string{"/bin/sh", "-l"}
	if len(cmd) > 0 {
		entrypoint = cmd
	}
	// The browser terminal is xterm-compatible whatever TERM the agent itself
	// inherited; container env overrides the host TERM that PRoot passes on.
	spec = cloneWorkloadSpec(spec)
	spec.Env["TERM"] = "xterm-256color"
	command, groups, err := d.runveilCommand(spec, entrypoint, nil, "/")
	if err != nil {
		return nil, err
	}
	var argv []string
	if d.runUser != "" {
		primary, err := user.LookupGroupId(strconv.Itoa(d.runGID))
		if err != nil {
			return nil, fmt.Errorf("resolve runtime primary group %d: %w", d.runGID, err)
		}
		// runuser takes the first -G as the primary group unless -g is given.
		argv = append(argv, "runuser", "-u", d.runUser, "-g", primary.Name)
		for _, gid := range groups {
			if group, err := user.LookupGroupId(strconv.Itoa(int(gid))); err == nil {
				argv = append(argv, "-G", group.Name)
			}
		}
		argv = append(argv, "--")
	}
	// A UTF-8 default locale for the shell; an image or workload LANG in
	// command.Env comes later and wins.
	argv = append(argv, "env", "-i", "LANG=C.UTF-8")
	argv = append(argv, command.Env...)
	return append(argv, command.Argv...), nil
}

// removeContainerDir deletes UDOCKER_DIR/containers/<name> together with the
// container's short-path /tmp source. A name created
// by `udocker create --name=` is a symlink to the real container directory;
// its target is removed too, but only when it is itself a direct child of the
// containers directory, so a hostile link can never widen the deletion.
func (d *Driver) removeContainerDir(name string) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("invalid container name %q", name)
	}
	// Best-effort: the tmp dir holds nothing worth failing a teardown over.
	d.removeWorkloadTmp(name)
	dir := d.containerDir(name)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(dir)
		if err == nil {
			containers, cerr := filepath.EvalSymlinks(filepath.Join(d.udockerDir, "containers"))
			if cerr == nil && filepath.Dir(target) == containers && target != containers {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
		}
		return os.Remove(dir)
	}
	return os.RemoveAll(dir)
}

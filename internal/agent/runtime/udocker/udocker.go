// Package udocker implements runtime.Driver for no-root / no-daemon nodes
// (autodl, compshare containers, shared clusters) using udocker + a patched
// proot. It encodes the pitfalls from the runpod evaluation as a deterministic
// pipeline: non-root user, UDOCKER_DIR on local disk, patched proot (socket-free
// seccomp), P1 exec mode, an nvidia driver shim, host-path loopback symlinks,
// and baked NCCL/loader env. udocker has no daemon, so instances are tracked as
// background process groups with a pidfile + redirected log.
package udocker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Driver runs containers via udocker.
type Driver struct {
	provisionMu        sync.Mutex
	provisioned        bool
	track              provisionTracker
	udockerDir         string // UDOCKER_DIR (forced onto local disk)
	stateDir           string // pidfiles + logs for tracked instances
	runUser            string // non-root user to run udocker as ("" = current user)
	runUserHome        string // home used when invoking udocker through runuser
	runUID             int    // resolved uid for runUser, or the current process
	runGID             int    // resolved gid for runUser, or the current process
	prootPath          string // extracted patched proot
	pyBin              string // python3 binary
	pyPath             string // optional offline udocker python module path
	udockerExe         string // optional offline udocker executable
	udockerScript      string // offline Python entry script, invoked with pyBin
	bundleDir          string // optional offline bundle root
	embeddedBundle     bool   // extract the bundle embedded in the agent into bundleDir
	allowOnlineInstall bool   // explicit escape hatch for pip-based provisioning
	offline            bool   // true when udocker came from a local bundle
	nvidiaSMIPath      string // host utility exposed to engines that probe GPU memory through nvidia-smi
	specMu             sync.Mutex
	specs              map[string]runtime.WorkloadSpec // tracked specs for controlled terminal shells
	hostTmpRoot        string                          // short-path root for guest /tmp binds; "" = os.TempDir()
	nativeOnce         sync.Once
	native             *native.Engine
	runveil            bool       // launch containers with runveil instead of the udocker CLI
	cliMu              sync.Mutex // serializes lazy udocker CLI provisioning under runveil
	procMu             sync.Mutex
	tracker            procgroup.Tracker       // pid and exit files of launched workloads, under stateDir
	freeSpace          func(path string) int64 // free bytes for the run user; nil measures the disk (tests stand in a full one)
}

// Option configures the driver.
type Option func(*Driver)

// New builds a udocker driver rooted at baseDir (its UDOCKER_DIR and state live
// under baseDir).
func New(baseDir string) *Driver {
	bundleDir := os.Getenv("GPUWORKS_UDOCKER_BUNDLE")
	embeddedBundle := false
	if bundleDir == "" {
		bundleDir = filepath.Join(baseDir, "offline", "udocker")
		embeddedBundle = true
	}
	driver := &Driver{
		udockerDir:         filepath.Join(baseDir, "udocker"),
		stateDir:           filepath.Join(baseDir, "udocker-state"),
		pyBin:              "python3",
		bundleDir:          bundleDir,
		embeddedBundle:     embeddedBundle,
		allowOnlineInstall: envTrue("GPUWORKS_ALLOW_ONLINE_UDOCKER_INSTALL"),
		specs:              make(map[string]runtime.WorkloadSpec),
		runUID:             os.Geteuid(),
		runGID:             os.Getegid(),
		runveil:            runveilSelected(),
	}
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = resolved
		}
		driver.nvidiaSMIPath = path
	}
	return driver
}

func (d *Driver) Name() string { return "udocker" }

// MountOwner returns the identity that will resolve udocker bind-mount source
// paths. Provisioning chooses between the dedicated user and --allow-root, so
// callers must not guess the username before that decision has completed.
func (d *Driver) MountOwner() (runtime.MountOwner, error) {
	d.provisionMu.Lock()
	defer d.provisionMu.Unlock()
	if !d.provisioned {
		return runtime.MountOwner{}, fmt.Errorf("udocker mount owner is unavailable before provisioning")
	}
	if d.runUser == "" && d.runUID == 0 && d.runGID == 0 && os.Geteuid() != 0 {
		return runtime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}, nil
	}
	return runtime.MountOwner{UID: d.runUID, GID: d.runGID, Username: d.runUser}, nil
}

var _ runtime.MountOwnerProvider = (*Driver)(nil)
var _ runtime.DeviceAccessDiagnoser = (*Driver)(nil)
var _ runtime.DeviceAccessGranter = (*Driver)(nil)

// Diagnostics reports the selected bundle and entrypoint without provisioning
// or touching the node. It is used by the agent Doctor RPC.
func (d *Driver) Diagnostics() string {
	source := "external bundle"
	if d.embeddedBundle {
		source = "embedded offline bundle " + assets.UdockerBundleVersion
	}
	entry := d.udockerExe
	if entry == "" {
		entry = d.udockerScript
	}
	if entry == "" && d.pyPath != "" {
		entry = "python module at " + d.pyPath
	}
	if entry == "" {
		entry = "not resolved until provisioning"
	}
	return fmt.Sprintf("source:   %s\nbundle:   %s\nentry:    %s\nonline:   %t\nlauncher: %s", source, d.bundleDir, entry, d.allowOnlineInstall, d.launcherName())
}

// engineArchName maps a Go architecture to the arch suffix udocker's own
// englib/proot binaries use (proot-<arch>[-4_8_0]), and reports whether
// gpuworks ships a validated patched proot for that architecture.
func engineArchName(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x86_64", true
	case "arm64":
		return "arm64", true
	default:
		return "", false
	}
}

// Detect reports viability. runveil needs only Linux on an architecture with
// an embedded patched PRoot. The udocker CLI launcher additionally needs
// Python >= 3.6; some accelerator base images keep their only Python in a
// Conda directory that is not on the non-interactive agent's PATH, so the same
// well-known locations as the SSH precheck are tried.
func (d *Driver) Detect(ctx context.Context) (bool, error) {
	if goruntime.GOOS != "linux" {
		return false, nil
	}
	if _, ok := engineArchName(goruntime.GOARCH); !ok {
		return false, nil
	}
	if d.runveil {
		return true, nil
	}
	return d.detectPython(ctx), nil
}

// detectPython finds a Python able to run the udocker CLI and remembers it.
func (d *Driver) detectPython(ctx context.Context) bool {
	py := findPython3()
	if py == "" {
		return false
	}
	d.pyBin = py
	out, err := exec.CommandContext(ctx, py, "-c", "import sys;print(sys.version_info[0]*100+sys.version_info[1])").Output()
	if err != nil {
		return false
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v >= 306 // udocker supports python >= 3.6
}

func findPython3() string {
	if py, err := exec.LookPath("python3"); err == nil {
		return py
	}
	for _, candidate := range []string{
		"/root/miniconda3/bin/python3",
		"/opt/conda/bin/python3",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// provisionRuntime prepares the runtime: the run user, the managed
// directories and the patched PRoot; the udocker CLI too, but only under the
// udocker CLI launcher. The caller holds provisionMu (ensureProvisioned).
func (d *Driver) provisionRuntime(ctx context.Context) error {
	slog.Info("provisioning udocker runtime", "dir", d.udockerDir, "state_dir", d.stateDir, "bundle", d.bundleDir, "embedded", d.embeddedBundle)
	if err := os.MkdirAll(d.udockerDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		return err
	}
	if err := colonNamesAllowed(d.udockerDir); err != nil {
		return err
	}
	// Some container images expose their only Python under /root/conda with a
	// non-traversable /root; the packaged udocker's explicit --allow-root mode
	// is then the only viable path, which decideRunUser falls back to.
	if err := d.decideRunUser(ctx); err != nil {
		return err
	}

	if err := d.ensureStateTraversal(); err != nil {
		return err
	}
	d.warnUntraversableAncestors()

	if d.runveil {
		// runveil needs nothing but the patched PRoot. The udocker CLI is
		// provisioned on first use only (ensureCLI).
		if err := d.extractProot(); err != nil {
			return err
		}
		if err := d.ensureOwned(ctx, d.udockerDir); err != nil {
			return fmt.Errorf("prepare runtime ownership: %w", err)
		}
		if err := d.ensureOwned(ctx, d.stateDir); err != nil {
			return fmt.Errorf("prepare runtime state ownership: %w", err)
		}
		d.provisioned = true
		return nil
	}
	return d.provisionCLI(ctx)
}

// extractProot places the embedded patched PRoot under both names udocker
// looks for, and records it as the executable every launch uses.
func (d *Driver) extractProot() error {
	archName, _ := engineArchName(goruntime.GOARCH)
	binDir := filepath.Join(d.udockerDir, "bin")
	src, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName))
	if err != nil {
		return fmt.Errorf("extract proot: %w", err)
	}
	// udocker also probes a versioned name.
	if _, err := assets.ExtractProot(assets.ProotStaticNoFD, filepath.Join(binDir, "proot-"+archName+"-4_8_0")); err != nil {
		return fmt.Errorf("extract versioned proot: %w", err)
	}
	d.prootPath = src
	return nil
}

// ensureOwned performs the expensive recursive ownership migration only for
// legacy roots. Once the root and bin directory belong to the selected user,
// a small marker makes the decision explicit and subsequent agent restarts do
// not walk every rootfs again.
func (d *Driver) ensureOwned(ctx context.Context, root string) error {
	if d.runUser == "" || root == "" {
		return nil
	}
	marker := filepath.Join(root, ".gpuworks-owner")
	want := fmt.Sprintf("%d:%d\n", d.runUID, d.runGID)
	if data, err := os.ReadFile(marker); err == nil && string(data) == want {
		if owned, _ := pathOwnedBy(root, d.runUID, d.runGID); owned {
			return nil
		}
	}
	for _, target := range []string{root, filepath.Join(root, "bin")} {
		if _, err := os.Lstat(target); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		owned, err := pathOwnedBy(target, d.runUID, d.runGID)
		if err != nil {
			return err
		}
		if !owned {
			slog.Info("runtime command", "runtime", "udocker", "args", []string{"chown", "-R", d.runUser + ":" + d.runUser, target})
			if out, err := exec.CommandContext(ctx, "chown", "-R", d.runUser+":"+d.runUser, target).CombinedOutput(); err != nil {
				return fmt.Errorf("chown -R %s %s: %w: %s", d.runUser, target, err, strings.TrimSpace(string(out)))
			}
		}
	}
	return os.WriteFile(marker, []byte(want), 0o644)
}

func pathOwnedBy(path string, uid, gid int) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return int(stat.Uid) == uid && int(stat.Gid) == gid, nil
}

func shouldUseDedicatedUdockerUser(python string, probeErr error) bool {
	return strings.TrimSpace(python) != "" && probeErr == nil
}

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Prepare creates + sets up the container (P1 + nvidia) and applies rootfs
// fixups. Idempotent.
func (d *Driver) Prepare(ctx context.Context, spec runtime.WorkloadSpec) error {
	return d.prepareWithProgress(ctx, spec, func(string, string) {})
}

// Run starts the container as a tracked background process group.
func (d *Driver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	return d.RunWithProgress(ctx, spec, nil)
}

// RunWithProgress exposes the expensive first-run rootfs preparation stages.
func (d *Driver) RunWithProgress(ctx context.Context, spec runtime.WorkloadSpec, onProgress func(phase, message string)) (string, error) {
	report := func(phase, message string) {
		if onProgress != nil {
			onProgress(phase, message)
		}
	}
	report(proto.PhaseContainerPrepare, "preparing container root filesystem")
	if err := d.prepareWithProgress(ctx, spec, report); err != nil {
		return "", err
	}
	if err := d.checkLaunchSpace(); err != nil {
		return "", err
	}
	report(proto.PhaseContainerLaunch, "launching container process")
	name := spec.Name

	if _, err := d.ensureWorkloadTmp(spec.Name); err != nil {
		slog.Warn("short-path /tmp bind unavailable; guest /tmp stays on the container rootfs", "container", name, "err", err)
	}
	args := d.containerRunArgs(spec)

	// Start detached; capture logs and pgid.
	logPath := d.LogPath(name)
	logf, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	var cmd *exec.Cmd
	if d.runveil {
		command, groups, err := d.runveilCommand(spec, spec.Entrypoint, spec.Cmd, "")
		if err != nil {
			logf.Close()
			return "", err
		}
		args = command.Argv
		cmd = d.runveilExec(context.Background(), command, groups)
	} else {
		full, err := d.workloadInvocation(args, spec)
		if err != nil {
			logf.Close()
			return "", err
		}
		cmd = exec.Command(full[0], full[1:]...)
		// The run-user path embeds these variables in the shell command.  The
		// direct (non-root) path must set them on exec.Cmd as well; otherwise
		// detached workloads look in the default ~/.udocker tree and may select
		// an unpatched proot executable instead of the provisioned local runtime.
		if d.runUser == "" {
			cmd.Env = d.cliEnv()
		}
		cmd.Dir = processDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group
	}
	slog.Info("runtime command", "runtime", d.launcherName(), "args", args)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		slog.Warn("runtime command failed", "runtime", "udocker", "args", args, "err", err)
		logf.Close()
		return "", err
	}
	if err := d.rememberSpec(name, spec); err != nil {
		slog.Warn("persist udocker workload spec for terminal shell failed", "container", name, "err", err)
	}
	// Reap asynchronously so the process isn't left a zombie.
	if err := d.procs().Track(name, cmd, func() { logf.Close() }); err != nil {
		slog.Warn("record udocker workload pid failed", "container", name, "err", err)
	}
	return name, nil
}

func (d *Driver) prepareWithProgress(ctx context.Context, spec runtime.WorkloadSpec, report func(phase, message string)) error {
	if err := d.ensureProvisioned(ctx); err != nil {
		return err
	}
	name := spec.Name
	cliImage, pinned := udockerCLIImage(spec.Image)
	if pinned {
		verified, verifyErr := d.pinnedImageVerified(spec.Image)
		if verifyErr != nil {
			return verifyErr
		}
		if !verified {
			return fmt.Errorf("udocker image %q has not been verified against its pinned digest", spec.Image)
		}
	}
	if d.runveil {
		return d.runveilPrepare(ctx, spec, report)
	}
	created := false
	cleanupPartial := func() {
		if !created {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.udo(cleanupCtx, "rm", name); err != nil {
			slog.Warn("remove partial udocker container after preparation failure", "container", name, "err", err)
		}
	}
	if _, err := d.udo(ctx, "inspect", name); err != nil {
		report(proto.PhaseContainerUnpack, "unpacking image layers into container root filesystem")
		created = true
		if handled, err := d.nativeCreate(ctx, name, spec.Image, func(line string) { report(proto.PhaseContainerUnpack, line) }); handled && err != nil {
			cleanupPartial()
			return err
		} else if handled {
			report(proto.PhaseContainerUnpack, "container root filesystem ready")
		} else if _, err := d.udo(ctx, "create", "--name="+name, cliImage); err != nil {
			// udocker may create the container directory before an individual
			// layer extraction fails. Remove that partial rootfs so controller
			// retries do not progressively exhaust the pod's disk.
			cleanupPartial()
			return fmt.Errorf("udocker create: %w", err)
		}
	}
	if needsNVIDIASetup(spec) {
		report("container_gpu_setup", "configuring NVIDIA runtime support")
		if _, err := d.udo(ctx, "setup", "--nvidia", name); err != nil {
			cleanupPartial()
			return fmt.Errorf("udocker setup --nvidia: %w", err)
		}
	}
	report("container_exec_setup", "configuring PRoot execution mode")
	if _, err := d.udo(ctx, "setup", "--execmode=P1", name); err != nil {
		cleanupPartial()
		return fmt.Errorf("udocker setup P1: %w", err)
	}
	report("container_fixup", "applying GPU runtime compatibility fixes")
	if err := d.rootfsFixupsWithNVIDIA(ctx, name, needsNVIDIASetup(spec)); err != nil {
		cleanupPartial()
		return err
	}
	return nil
}

// cliEnv is the environment contract shared by foreground and detached
// udocker invocations. Keep the caller environment intact so credentials and
// proxy configuration continue to work.
func (d *Driver) cliEnv() []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"UDOCKER_DIR="+d.udockerDir,
		"UDOCKER_USE_PROOT_EXECUTABLE="+d.prootPath,
	)
	if d.pyPath != "" {
		env = append(env, "PYTHONPATH="+d.pyPath)
	}
	return env
}

// containerRunArgs assembles the udocker run command. Host network (P1 shares
// host net), GPU selection is passed via CUDA_VISIBLE_DEVICES, and volumes use
// --volume. Unlike Docker, udocker's --rm is a boolean flag and rejects
// --rm=false; omitting it is the supported way to preserve the container.
func (d *Driver) containerRunArgs(spec runtime.WorkloadSpec) []string {
	args := []string{"run"}
	env := d.runEnv(spec)
	for k, v := range env {
		args = append(args, "--env="+k+"="+v)
	}
	for _, m := range d.workloadMounts(spec) {
		// udocker's --volume syntax has no portable read-only qualifier across
		// supported releases. Keep the bind target stable; callers that require
		// strict immutability must enforce it in the guest/runtime-specific path.
		args = append(args, "--volume="+m.Source+":"+m.Target)
	}
	// udocker treats command arguments as arguments to the image metadata
	// entrypoint.  WorkloadSpec.Entrypoint, however, is an explicit override
	// (the same contract used by the Docker driver).  Pass its executable via
	// udocker's --entrypoint option, then append the remaining entrypoint tokens
	// and Cmd as arguments.  Without this, an image whose metadata entrypoint is
	// e.g. ["vllm", "serve"] would receive another "vllm serve" prefix and
	// execute "vllm serve vllm serve ...".
	if len(spec.Entrypoint) > 0 {
		args = append(args, "--entrypoint="+spec.Entrypoint[0])
	}
	args = append(args, spec.Name)
	// The first entrypoint token was supplied as --entrypoint above; the rest
	// are the explicit entrypoint arguments and must precede the workload args.
	if len(spec.Entrypoint) > 1 {
		args = append(args, spec.Entrypoint[1:]...)
	}
	args = append(args, spec.Cmd...)
	return args
}

// workloadMounts is every bind a workload gets: the caller's mounts, the host
// nvidia-smi, the accelerator device nodes, and a short-path /tmp.
func (d *Driver) workloadMounts(spec runtime.WorkloadSpec) []runtime.Mount {
	mounts := append([]runtime.Mount(nil), spec.Mounts...)
	if d.nvidiaSMIPath != "" && !isNonNVIDIASpec(spec) && !hasMountTarget(mounts, "/usr/bin/nvidia-smi") {
		// The NVIDIA setup exposes devices and driver libraries, but not the
		// host utility. SGLang invokes nvidia-smi during argument validation
		// to determine memory capacity, before CUDA initialization starts.
		mounts = append(mounts, runtime.Mount{Source: d.nvidiaSMIPath, Target: "/usr/bin/nvidia-smi", ReadOnly: true})
	}
	mounts = append(mounts, acceleratorMounts(spec)...)
	if !hasMountTarget(mounts, "/tmp") {
		// A caller-provided /tmp keeps winning; see workload_tmp.go for why the
		// guest /tmp needs a short host path at all.
		if source, ok := d.preparedWorkloadTmp(spec.Name); ok {
			mounts = append(mounts, runtime.Mount{Source: source, Target: "/tmp"})
		}
	}
	return mounts
}

func hasMountTarget(mounts []runtime.Mount, target string) bool {
	for _, mount := range mounts {
		if filepath.Clean(mount.Target) == target {
			return true
		}
	}
	return false
}

var _ runtime.Driver = (*Driver)(nil)
var _ runtime.RunProgressDriver = (*Driver)(nil)
var _ runtime.PTYExecer = (*Driver)(nil)

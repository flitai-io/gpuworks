package udocker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
)

func isAscendSpec(spec runtime.WorkloadSpec) bool {
	return strings.TrimSpace(spec.Env["ASCEND_VISIBLE_DEVICES"]) != ""
}

func isPPUSpec(spec runtime.WorkloadSpec) bool {
	for i, arg := range spec.DockerArgs {
		if arg == "--device" && i+1 < len(spec.DockerArgs) && strings.Contains(spec.DockerArgs[i+1], "/dev/alixpu") {
			return true
		}
	}
	return false
}

func isHygonSpec(spec runtime.WorkloadSpec) bool {
	return strings.TrimSpace(spec.Env["HYGON_VISIBLE_DEVICES"]) != ""
}

func isNVIDIASpec(spec runtime.WorkloadSpec) bool {
	return strings.TrimSpace(spec.Env["CUDA_VISIBLE_DEVICES"]) != ""
}

// isAMDSpec marks a ROCm workload: the AMD prober pins its cards through
// ROCR_VISIBLE_DEVICES (GPU-<unique id>), which PRoot cannot do with device
// cgroups the way Docker's --device does.
func isAMDSpec(spec runtime.WorkloadSpec) bool {
	return strings.HasPrefix(strings.TrimSpace(spec.Env["ROCR_VISIBLE_DEVICES"]), "GPU-") && !isHygonSpec(spec)
}

// isNonNVIDIASpec reports a workload whose accelerator needs none of the
// NVIDIA shim setup (libraries, CUDA_VISIBLE_DEVICES, nvidia-smi).
func isNonNVIDIASpec(spec runtime.WorkloadSpec) bool {
	return isAscendSpec(spec) || isPPUSpec(spec) || isHygonSpec(spec) || isAMDSpec(spec)
}

func needsNVIDIASetup(spec runtime.WorkloadSpec) bool { return !isNonNVIDIASpec(spec) }

var ascendSupportPaths = []string{
	"/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc", "/dev/ummu", "/dev/uburma",
	"/usr/local/Ascend/driver", "/usr/local/dcmi", "/usr/local/bin/npu-smi",
	"/etc/ascend_install.info", "/etc/hccn.conf", "/etc/vnpu.cfg",
}

// acceleratorMounts converts vendor device injection into PRoot bind mounts;
// udocker cannot enforce Docker device-cgroup arguments itself.
func acceleratorMounts(spec runtime.WorkloadSpec) []runtime.Mount {
	seen := map[string]bool{}
	for _, mount := range spec.Mounts {
		seen[filepath.Clean(mount.Source)] = true
		seen[filepath.Clean(mount.Target)] = true
	}
	var mounts []runtime.Mount
	add := func(path string) {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || seen[path] {
			return
		}
		seen[path] = true
		mounts = append(mounts, runtime.Mount{Source: path, Target: path})
	}
	if isAscendSpec(spec) {
		for _, raw := range strings.Split(spec.Env["ASCEND_VISIBLE_DEVICES"], ",") {
			if index := strings.TrimSpace(raw); index != "" {
				add("/dev/davinci" + index)
			}
		}
		for _, path := range ascendSupportPaths {
			if _, err := os.Stat(path); err == nil {
				add(path)
			}
		}
	}
	// Raw device arguments are emitted by the Ascend/PPU/Hygon probers. Parse
	// the Docker-compatible spellings uniformly so a new vendor does not need
	// another permission branch in the udocker driver.
	for i := 0; i < len(spec.DockerArgs); i++ {
		arg := spec.DockerArgs[i]
		if arg == "--device" && i+1 < len(spec.DockerArgs) {
			i++
			add(strings.SplitN(spec.DockerArgs[i], ":", 2)[0])
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--device="); ok {
			add(strings.SplitN(value, ":", 2)[0])
		}
	}
	if isNVIDIASpec(spec) {
		// The device node backing CUDA index N is named after the driver's
		// minor number, not after N - see nvidia_devices.go.
		nodes := nvidiaDeviceNodeMap()
		for _, raw := range strings.Split(spec.Env["CUDA_VISIBLE_DEVICES"], ",") {
			if index := strings.TrimSpace(raw); index != "" {
				addNVIDIADeviceNode(nodes, index, add)
			}
		}
		for _, path := range []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset"} {
			if _, err := os.Stat(path); err == nil {
				add(path)
			}
		}
	}
	return mounts
}

// workloadDeviceMounts includes both mounts supplied directly by a prober and
// mounts derived from Docker-style device arguments. requiredDeviceGroups
// filters regular files and directories, so this remains vendor-neutral.
func workloadDeviceMounts(spec runtime.WorkloadSpec) []runtime.Mount {
	mounts := append([]runtime.Mount(nil), spec.Mounts...)
	return append(mounts, acceleratorMounts(spec)...)
}

// rootfsFixups applies the one-time rootfs corrections: nvidia driver shim,
// /etc/hosts, host-path loopback symlink, /root perms.
func (d *Driver) rootfsFixups(ctx context.Context, name string) error {
	return d.rootfsFixupsWithNVIDIA(ctx, name, true)
}

// rootfsFixupsWithNVIDIA writes the GPU runtime shims into a prepared container
// ROOT.
//
// OWNERSHIP: this runs in the agent process, so every path it creates below is
// owned by the agent's uid - root on the validation node - while the unpack has
// already handed the rest of ROOT to the run user. `udocker rm` runs as that run
// user, and upstream FileUtil._removedir() chmods each file before unlinking it,
// so a root-owned entry makes the whole removal fail with "Error: deleting
// directory" and the rootfs then leaks. Whatever is written here has to end up
// owned by the run user (see Remove in udocker.go for the full mechanism).
// ROOT/etc/hosts below is written unconditionally, so this applies to every
// container, not just the ones that take the NVIDIA shim.
func (d *Driver) rootfsFixupsWithNVIDIA(ctx context.Context, name string, installNVIDIA bool) error {
	root, err := d.containerRoot(ctx, name)
	if err != nil || root == "" {
		return fmt.Errorf("resolve container ROOT: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve canonical container ROOT %s: %w", root, err)
	}
	if err := d.ensureHostRootLoopback(canonicalRoot); err != nil {
		return fmt.Errorf("prepare container ROOT loopback: %w", err)
	}

	// nvidia driver shim: vLLM's platform detection dlopens libcuda and friends;
	// copy the four host libraries into /opt/nvidia-shim inside the rootfs.
	// A library the host has but the copy could not write (a full disk) fails
	// the launch here: an engine started without libnvidia-ml exits at once
	// with an error that does not name the cause.
	if installNVIDIA {
		shim := filepath.Join(canonicalRoot, "opt", "nvidia-shim")
		if err := os.MkdirAll(shim, 0o755); err != nil {
			return fmt.Errorf("create the container's NVIDIA library directory: %w", err)
		}
		for _, lib := range shimDriverLibs {
			if err := copyDriverLib(lib, shim); err != nil {
				return fmt.Errorf("copy NVIDIA driver library %s into the container: %w", lib, err)
			}
		}
	}

	// /etc/hosts localhost (multi-GPU NCCL needs it; harmless single-GPU).
	if err := atomicWriteFile(filepath.Join(canonicalRoot, "etc", "hosts"),
		[]byte("127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n"), 0o644); err != nil {
		return fmt.Errorf("write container hosts: %w", err)
	}

	// /root must be enterable (udocker passwd points home at /root).
	_ = os.Chmod(filepath.Join(canonicalRoot, "root"), 0o755)
	return nil
}

// ensureHostRootLoopback makes host paths leaked by /proc/self/maps resolvable
// inside a proot guest. CUDA consumers such as FlashInfer, DeepEP, torchvision,
// vLLM and SGLang may discover a loaded library as:
//
//	<UDOCKER_DIR>/containers/<real-id>/ROOT/usr/local/.../libcudart.so
//
// proot then resolves that absolute host path inside the guest. Mirror the
// canonical (real-ID) ROOT at that same guest path. This is deliberately based
// on d.udockerDir and EvalSymlinks rather than udocker's named-container alias.
func (d *Driver) ensureHostRootLoopback(canonicalRoot string) error {
	resolvedRoot, err := filepath.EvalSymlinks(canonicalRoot)
	if err != nil {
		return fmt.Errorf("resolve canonical ROOT %s: %w", canonicalRoot, err)
	}
	canonicalRoot = filepath.Clean(resolvedRoot)
	udockerDir, err := filepath.EvalSymlinks(d.udockerDir)
	if err != nil {
		return fmt.Errorf("resolve canonical udocker directory %s: %w", d.udockerDir, err)
	}
	udockerDir = filepath.Clean(udockerDir)
	if !filepath.IsAbs(canonicalRoot) || !filepath.IsAbs(udockerDir) {
		return fmt.Errorf("udocker paths must be absolute: root=%q dir=%q", canonicalRoot, udockerDir)
	}
	containersDir := filepath.Join(udockerDir, "containers")
	rel, err := filepath.Rel(containersDir, canonicalRoot)
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[0] == "" || parts[0] == "." || parts[0] == ".." || parts[1] != "ROOT" {
		return fmt.Errorf("canonical ROOT %q is outside %q or has an unexpected layout", canonicalRoot, containersDir)
	}

	root, err := os.OpenRoot(canonicalRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	relativeLoopback := strings.TrimPrefix(canonicalRoot, string(filepath.Separator))
	guestLoopback := filepath.Join(canonicalRoot, relativeLoopback)
	if err := root.MkdirAll(filepath.Dir(relativeLoopback), 0o755); err != nil {
		return err
	}
	// MkdirAll honors umask, and a root agent may create these as 0700.
	// Repair even an existing correct loopback before returning. Anchor all
	// operations inside ROOT so image symlinks cannot reach host directories.
	for parent := filepath.Dir(relativeLoopback); parent != "."; parent = filepath.Dir(parent) {
		directory, err := root.OpenFile(parent, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		info, err := directory.Stat()
		if err == nil && info.Mode().Perm()&0o111 != 0o111 {
			// Add search only; preserve image read/write and special bits.
			err = directory.Chmod(info.Mode() | 0o111)
		}
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if info, err := root.Lstat(relativeLoopback); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink loopback path %s", guestLoopback)
		}
		target, readErr := root.Readlink(relativeLoopback)
		if readErr == nil && filepath.Clean(target) == canonicalRoot {
			return nil
		}
		if err := root.Remove(relativeLoopback); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return root.Symlink(canonicalRoot, relativeLoopback)
}

// containerRoot resolves the rootfs path for a named container.
func (d *Driver) containerRoot(ctx context.Context, name string) (string, error) {
	// Scan the containers dir for a container.json whose name matches.
	containersDir := filepath.Join(d.udockerDir, "containers")
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		return "", err
	}
	// GPUWorks passes the exact udocker container name (created with
	// --name=<name>), and udocker stores named containers in a directory with
	// that name. Prefer this deterministic lookup: container.json does not
	// contain the name, and falling back to the first directory would apply
	// NVIDIA shims, hosts, and symlinks to an unrelated stale container when
	// multiple deployments exist.
	directRoot := filepath.Join(containersDir, name, "ROOT")
	if st, err := os.Stat(directRoot); err == nil && st.IsDir() {
		return directRoot, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(containersDir, e.Name(), "ROOT")
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			// Prefer the named-link container; udocker also keeps a "names" file.
			data, _ := os.ReadFile(filepath.Join(containersDir, e.Name(), "container.json"))
			if strings.Contains(string(data), `"`+name+`"`) || len(entries) == 1 {
				return root, nil
			}
		}
	}
	// Fallback: single container dir.
	for _, e := range entries {
		root := filepath.Join(containersDir, e.Name(), "ROOT")
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			return root, nil
		}
	}
	return "", fmt.Errorf("no ROOT found for %s", name)
}

// shimDriverLibs are the host driver libraries CUDA consumers dlopen, copied
// into /opt/nvidia-shim inside the rootfs.
var shimDriverLibs = []string{"libcuda.so.1", "libnvidia-ml.so.1", "libnvidia-ptxjitcompiler.so.1", "libnvidia-nvvm.so.4"}

// driverLibDirs are where a host keeps its driver libraries; tests point it
// elsewhere.
var driverLibDirs = []string{"/usr/lib/x86_64-linux-gnu", "/usr/lib64", "/lib/x86_64-linux-gnu"}

// copyDriverLib copies a host driver library into the shim dir, dereferencing
// symlinks. A library the host does not have is skipped (a non-GPU host, or a
// driver without it); one it has but that cannot be read or written is an
// error.
func copyDriverLib(lib, destDir string) error {
	for _, base := range driverLibDirs {
		src := filepath.Join(base, lib)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return atomicWriteFile(filepath.Join(destDir, lib), data, 0o755)
	}
	return nil
}

// atomicWriteFile prevents a failed fixup from leaving a truncated runtime
// library or hosts file in a rootfs that may be retried by the reconciler.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gpuworks-atomic-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// udockerInvocation builds the foreground argv to run a udocker command as the
// run user (used by the detached Run).
func (d *Driver) udockerInvocation(args []string) []string {
	full := d.udockerArgs(args...)
	if d.runUser != "" {
		return []string{"runuser", "-u", d.runUser, "--", "bash", "-lc", d.shellInvocation(full)}
	}
	return full
}

func (d *Driver) udockerArgs(args ...string) []string {
	if d.udockerExe != "" {
		return append([]string{d.udockerExe, "--allow-root"}, args...)
	}
	if d.udockerScript != "" {
		return append([]string{d.pyBin, d.udockerScript, "--allow-root"}, args...)
	}
	return append([]string{d.pyBin, "-m", "udocker", "--allow-root"}, args...)
}

func (d *Driver) shellInvocation(full []string) string {
	env := []string{
		"UDOCKER_DIR=" + shellQuote(d.udockerDir),
		"UDOCKER_USE_PROOT_EXECUTABLE=" + shellQuote(d.prootPath),
	}
	if d.pyPath != "" {
		env = append(env, "PYTHONPATH="+shellQuote(d.pyPath))
	}
	command := strings.Join(append(env, quoteAll(full)...), " ")
	if d.runUserHome != "" {
		return "cd " + shellQuote(d.runUserHome) + " && HOME=" + shellQuote(d.runUserHome) + " USER=" + shellQuote(d.runUser) + " LOGNAME=" + shellQuote(d.runUser) + " " + command
	}
	return command
}

func (d *Driver) pidOf(name string) (int, bool) { return d.procs().PID(name) }

func processAlive(pid int) bool { return procgroup.Alive(pid) }

func normalizeImage(image string) string {
	image = strings.TrimSpace(image)
	image = strings.TrimPrefix(image, "https://")
	image = strings.TrimPrefix(image, "http://")
	if image == "" || strings.Contains(image, "@") {
		return image
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		image += ":latest"
	}
	return image
}

// udockerImageListed performs an exact repository:tag match. Suffix matching
// is unsafe for mirror pulls: an entry such as
// swr.example/gpustack/runner:tag previously made the canonical
// gpustack/runner:tag look present, so the engine skipped the canonical tag
// step and `udocker create` failed with "imagerepo is invalid".
func udockerImageListed(output, image string) bool {
	want := normalizeImage(image)
	if want == "" || strings.Contains(want, "@") {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == want {
			return true
		}
	}
	return false
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// copyTree copies src into dst. skip, when set, names relative paths to leave
// untouched in dst.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.RemoveAll(target)
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported offline bundle entry %s (%s)", path, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

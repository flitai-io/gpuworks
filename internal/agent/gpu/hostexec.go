package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// An agent running in a container (a Kubernetes DaemonSet) runs the vendor's
// CLIs in the host's mount namespace instead of its own: they are the
// host's programs, linked against the host's C library and driver
// libraries, which a container of another distribution cannot load (the PPU
// SDK on Ubuntu 24.04 needs glibc 2.38; the agent image has 2.36). The
// manifest grants no vendor mounts, so every vendor's CLIs and driver
// libraries are found by walking the host's own file system through
// hostRoot; RunCLIsOnHost switches every probe to do so, once at startup
// (cmd/gpuworks-agent sets it unconditionally for --driver=kube).
// nvidia-smi alone also has a container-local fallback: see
// nvidiaSMICommand.

var cliOnHost atomic.Bool

// hostRoot is the host's root as an agent sharing its PID namespace sees it.
var hostRoot = "/proc/1/root"

// hostCLIDirs are searched for a CLI on the host: the usual PATH plus the
// vendor directories the drivers install their tools in.
var hostCLIDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
	"/usr/local/Ascend/driver/tools", "/opt/hyhal/bin", "/opt/dtk/bin",
	// Kunlun's XRE SDK installs under /usr/local/xpu (its version.txt sits
	// there); xpu-smi is also commonly linked into /usr/bin.
	"/usr/local/xpu/bin"}

// RunCLIsOnHost makes every vendor CLI the probes run execute in the host's
// mount namespace (the agent must share the host's PID namespace).
func RunCLIsOnHost() { cliOnHost.Store(true) }

// CLIsOnHost reports whether RunCLIsOnHost was called: callers outside this
// package that also have to place a process the way a vendor CLI runs (the
// doctor's NVIDIA diagnostics, which run python3) ask this instead of
// duplicating the switch.
func CLIsOnHost() bool { return cliOnHost.Load() }

// HostCommand builds a command for name the same way cliCommand places a
// vendor CLI, for callers outside this package that run a program alongside
// one (the doctor's python3 diagnostics).
func HostCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return cliCommand(ctx, name, args...)
}

// lookCLI finds a CLI where it will run: in PATH, or on the host. nvidia-smi
// is special: see lookNVIDIASMI.
func lookCLI(name string) (string, error) {
	if !cliOnHost.Load() {
		return exec.LookPath(name)
	}
	if name == "nvidia-smi" {
		loc, err := locateNVIDIASMI()
		if err != nil {
			return "", err
		}
		return loc.path, nil
	}
	if filepath.IsAbs(name) {
		if fi, err := statOnHost(name); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return name, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	for _, dir := range hostCLIDirs {
		path := filepath.Join(dir, name)
		if fi, err := statOnHost(path); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// statTool stats a file a runtime check needs (a driver library, a toolkit),
// where the check will run: on the host when the CLIs do.
func statTool(path string) (os.FileInfo, error) {
	if cliOnHost.Load() {
		return statOnHost(path)
	}
	return os.Stat(path)
}

// lstatTool is statTool without following a final symbolic link: a check
// that must reject a device path that is itself a link (Kunlun's
// /dev/xdrdrv), or read a node's own mode and device number, uses it.
func lstatTool(path string) (os.FileInfo, error) {
	if cliOnHost.Load() {
		dir, err := resolveDirUnderRoot("", filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		return os.Lstat(filepath.Join(hostRoot, dir, filepath.Base(path)))
	}
	return os.Lstat(path)
}

// globTool is filepath.Glob where the check will run.
func globTool(pattern string) []string {
	if !cliOnHost.Load() {
		matches, _ := filepath.Glob(pattern)
		return matches
	}
	// The directories before the first wildcard may be links (resolved on
	// the host); the matches are reported under the pattern's own prefix.
	prefix, rest := globPrefix(pattern)
	resolved, err := resolveDirUnderRoot("", prefix)
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(hostRoot, resolved, rest))
	base := filepath.Join(strings.TrimSuffix(hostRoot, "/"), resolved)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Join(prefix, strings.TrimPrefix(m, base)))
	}
	return out
}

// globPrefix splits a pattern into the directory before its first wildcard
// and the rest.
func globPrefix(pattern string) (string, string) {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return filepath.Dir(pattern), filepath.Base(pattern)
	}
	dir := filepath.Dir(pattern[:i+1])
	rel, _ := filepath.Rel(dir, pattern)
	return dir, rel
}

// readTool reads a file a probe needs (a version file, an install marker),
// where the check will run: on the host when the CLIs do.
func readTool(path string) ([]byte, error) {
	if !cliOnHost.Load() {
		return os.ReadFile(path)
	}
	resolved, _, err := resolveUnderRoot("", path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(hostRoot, resolved))
}

// statOnHost stats a host path, following its symbolic links on the host:
// through /proc/1/root the kernel resolves an absolute link target against
// the agent's own root (Hygon's /usr/local/bin/hy-smi links to
// /usr/local/hyhal/bin/hy-smi, which the container does not have).
func statOnHost(path string) (os.FileInfo, error) {
	_, fi, err := resolveUnderRoot("", path)
	return fi, err
}

// resolveOnHost is statOnHost, also returning the fully-resolved host path
// (never one prefixed with hostRoot): a caller that turns the result into a
// mount source, or passes it to a command that itself runs on the host,
// needs the path as the host sees it, not as this process does.
func resolveOnHost(path string) (string, os.FileInfo, error) {
	return resolveUnderRoot("", path)
}

// resolveUnderRoot stats path as it would resolve under root on the host
// (root "" is the host's own root; a non-empty root is a chroot the host
// keeps elsewhere, such as the GPU Operator's driver container at
// nvidiaDriverContainerRoot): an absolute symlink target is resolved
// against root, exactly as a process chrooted (or nsentered and chrooted)
// there would see it, not against the host's real root.
func resolveUnderRoot(root, path string) (string, os.FileInfo, error) {
	dir, err := resolveDirUnderRoot(root, filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return path, nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	for i := 0; i < 16; i++ {
		fi, err := os.Lstat(filepath.Join(hostRoot, root, path))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path, fi, err
		}
		target, err := os.Readlink(filepath.Join(hostRoot, root, path))
		if err != nil {
			return path, nil, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if dir, err = resolveDirUnderRoot(root, filepath.Dir(filepath.Clean(target))); err != nil {
			return path, nil, err
		}
		path = filepath.Join(dir, filepath.Base(target))
	}
	return path, nil, &os.PathError{Op: "stat", Path: path, Err: errors.New("too many levels of symbolic links")}
}

// resolveDirUnderRoot resolves every link among a directory path's
// components under root, as resolveUnderRoot does for its last one:
// through /proc/1/root an absolute link in the middle of a path (Ubuntu's
// /var/run -> /run, /opt/rocm -> /etc/alternatives/rocm) would otherwise
// point into the agent's own file system.
func resolveDirUnderRoot(root, dir string) (string, error) {
	dir = filepath.Clean("/" + dir)
	out := "/"
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	for hops := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			out = filepath.Dir(out)
			continue
		}
		next := filepath.Join(out, part)
		fi, err := os.Lstat(filepath.Join(hostRoot, root, next))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			// A missing component is left for the caller's own stat to report.
			out = next
			continue
		}
		if hops++; hops > 40 {
			return dir, &os.PathError{Op: "stat", Path: dir, Err: errors.New("too many levels of symbolic links")}
		}
		target, err := os.Readlink(filepath.Join(hostRoot, root, next))
		if err != nil {
			return dir, err
		}
		if filepath.IsAbs(target) {
			out = "/"
		}
		parts = append(strings.Split(strings.Trim(target, "/"), "/"), parts...)
	}
	return out, nil
}

// cliCommand builds the command for a vendor CLI, on the host when CLIs run
// there. nvidia-smi is special: see nvidiaSMICommand.
func cliCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !cliOnHost.Load() {
		return exec.CommandContext(ctx, name, args...)
	}
	if name == "nvidia-smi" {
		return nvidiaSMICommand(ctx, args...)
	}
	path := name
	if found, err := lookCLI(name); err == nil {
		path = found
	}
	return exec.CommandContext(ctx, "nsenter", append([]string{"-t", "1", "-m", "--", path}, args...)...)
}

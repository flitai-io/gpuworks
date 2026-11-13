package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
)

// The agent pod shares the host's PID namespace, so PID 1's root is the
// host's file system and `nsenter -t 1 -m` runs a command in the host's mount
// namespace. Everything that talks to the node's containerd runs that way:
// the socket, the registry configuration (certs.d) and the FIFOs an exec
// hands the shim are then at the paths containerd itself sees, whatever the
// distribution (kubeadm, k3s, RKE2) put them at, and no host path has to be
// mounted into the agent for them.

// HostRoot is the host's root as the agent sees it.
const HostRoot = "/proc/1/root"

// containerdLayout is where one distribution keeps containerd's socket,
// configuration and per-registry hosts directory.
type containerdLayout struct {
	Name, Socket, Config, CertsDir string
}

// containerdLayouts are tried in order; the first whose socket exists wins,
// and among those sharing a socket (k3s and RKE2) the one whose config does.
var containerdLayouts = []containerdLayout{
	{"containerd", "/run/containerd/containerd.sock", "/etc/containerd/config.toml", "/etc/containerd/certs.d"},
	{"k3s", "/run/k3s/containerd/containerd.sock", "/var/lib/rancher/k3s/agent/etc/containerd/config.toml", "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"},
	{"rke2", "/run/k3s/containerd/containerd.sock", "/var/lib/rancher/rke2/agent/etc/containerd/config.toml", "/var/lib/rancher/rke2/agent/etc/containerd/certs.d"},
	{"microk8s", "/var/snap/microk8s/common/run/containerd.sock", "/var/snap/microk8s/current/args/containerd-template.toml", "/var/snap/microk8s/current/args/certs.d"},
}

func exists(root, path string) bool {
	_, err := os.Stat(filepath.Join(root, path))
	return err == nil
}

// findContainerd picks the node's containerd. socket, when set, is the
// cluster's override: the layout with that socket, or a bare one.
func findContainerd(root, socket string) (containerdLayout, error) {
	if socket != "" {
		if !exists(root, socket) {
			return containerdLayout{}, fmt.Errorf("containerd socket %s does not exist on the node", socket)
		}
		for _, l := range containerdLayouts {
			if l.Socket == socket && exists(root, l.Config) {
				return l, nil
			}
		}
		return containerdLayout{Name: "containerd", Socket: socket}, nil
	}
	var bySocket *containerdLayout
	for i, l := range containerdLayouts {
		if !exists(root, l.Socket) {
			continue
		}
		if exists(root, l.Config) {
			return l, nil
		}
		if bySocket == nil {
			bySocket = &containerdLayouts[i]
		}
	}
	if bySocket != nil {
		return *bySocket, nil
	}
	var tried []string
	for _, l := range containerdLayouts {
		tried = append(tried, l.Socket)
	}
	return containerdLayout{}, fmt.Errorf("no containerd socket on the node (tried %s); set the cluster's containerd socket", strings.Join(tried, ", "))
}

// errNotContainerd refuses a node whose kubelet runs another runtime.
var errNotContainerd = errors.New("the node's container runtime is not containerd")

// checkRuntime refuses a kubelet that does not run containerd
// ("containerd://2.2.7-k3s1").
func checkRuntime(runtimeVersion string) error {
	if name, _, _ := strings.Cut(runtimeVersion, "://"); name != "containerd" {
		return fmt.Errorf("%w (%s): GPUWorks prepares images and execs through containerd; CRI-O and Docker nodes are not supported", errNotContainerd, runtimeVersion)
	}
	return nil
}

// hostTools are the node-facing commands the kube runtime prepares: nerdctl
// run in the host's mount namespace, configured for the node's containerd.
type hostTools struct {
	Layout containerdLayout
	// Bin is the directory holding the nerdctl wrapper; it goes first in
	// PATH, so every nerdctl call the image store makes goes through it.
	Bin string
}

// prepareHost installs nerdctl for the node: the binary is copied into the
// state directory, which is the same path on the host, and a wrapper in
// binDir runs it in the host's mount namespace with a configuration naming
// the node's socket and registry hosts directory.
func prepareHost(root, stateDir, binDir, socket, nerdctlPath string) (hostTools, error) {
	layout, err := findContainerd(root, socket)
	if err != nil {
		return hostTools{}, err
	}
	toolDir := filepath.Join(stateDir, "kube-tools")
	if err := os.MkdirAll(filepath.Join(toolDir, "nerdctl-data"), 0o755); err != nil {
		return hostTools{}, err
	}
	nerdctlCopy := filepath.Join(toolDir, "nerdctl")
	if err := copyIfChanged(nerdctlPath, nerdctlCopy); err != nil {
		return hostTools{}, fmt.Errorf("install nerdctl for the node: %w", err)
	}
	var toml strings.Builder
	fmt.Fprintf(&toml, "address = %q\ndata_root = %q\n", layout.Socket, filepath.Join(toolDir, "nerdctl-data"))
	if layout.CertsDir != "" && exists(root, layout.CertsDir) {
		fmt.Fprintf(&toml, "hosts_dir = [%q]\n", layout.CertsDir)
	}
	tomlPath := filepath.Join(toolDir, "nerdctl.toml")
	if err := os.WriteFile(tomlPath, []byte(toml.String()), 0o644); err != nil {
		return hostTools{}, err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return hostTools{}, err
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nNERDCTL_TOML=%s exec nsenter -t 1 -m -- %s \"$@\"\n", shellQuote(tomlPath), shellQuote(nerdctlCopy))
	if err := os.WriteFile(filepath.Join(binDir, "nerdctl"), []byte(wrapper), 0o755); err != nil {
		return hostTools{}, err
	}
	return hostTools{Layout: layout, Bin: binDir}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// copyIfChanged copies src to dst unless dst already holds the same bytes
// (a busy binary cannot be rewritten in place, so a new one is renamed in).
func copyIfChanged(src, dst string) error {
	want, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, want) {
		return nil
	}
	tmp := dst + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(want)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// hostCommand runs a command in the host's mount namespace (an NFS mount the
// agent makes lands on the host, where instance pods see it).
func hostCommand(name string, args ...string) (string, []string) {
	return "nsenter", append([]string{"-t", "1", "-m", "--", name}, args...)
}

// groupID resolves a group name in the host's /etc/group.
func groupID(root, name string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(root, "/etc/group"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 3 && f[0] == name {
			return strconv.ParseInt(f[2], 10, 64)
		}
	}
	return 0, fmt.Errorf("group %q does not exist on the node", name)
}

// HostExec runs a command in the host's mount namespace and returns its
// combined output; the agent mounts NFS storages through it.
func HostExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	bin, full := hostCommand(name, args...)
	return exec.CommandContext(ctx, bin, full...).CombinedOutput()
}

// shimSocket finds the containerd that runs the agent's own container: the
// agent's ancestor is that container's shim, started with "-address
// <socket>". It settles which containerd the kubelet uses on a node that runs
// two (a k3s node where Docker keeps the system containerd); "" when no
// ancestor says.
func shimSocket(proc string, pid int) string {
	for i := 0; i < 8 && pid > 1; i++ {
		cmdline, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return ""
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if strings.Contains(filepath.Base(args[0]), "containerd-shim") {
			for j := 1; j+1 < len(args); j++ {
				if args[j] == "-address" || args[j] == "--address" {
					return args[j+1]
				}
			}
			return ""
		}
		stat, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
		if err != nil {
			return ""
		}
		// pid (comm) state ppid …; comm may hold spaces and parentheses.
		rest := string(stat)
		if k := strings.LastIndexByte(rest, ')'); k >= 0 {
			rest = rest[k+1:]
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			return ""
		}
		if pid, err = strconv.Atoi(f[1]); err != nil {
			return ""
		}
	}
	return ""
}

// authSubdir is where the image store writes a pull's private registry login,
// under the state directory: nerdctl runs in the node's mount namespace
// (host.go), where the agent container's /tmp does not exist but its state
// directory does, at the same path.
var authSubdir = filepath.Join("kube-tools", "pull-auth")

// newImageStore is the node's containerd in kubelet's namespace, pulling with
// logins staged under the state directory.
func newImageStore(stateDir string) ImageStore {
	return nerdctl.NewInNamespace("k8s.io").WithAuthDir(filepath.Join(stateDir, authSubdir))
}

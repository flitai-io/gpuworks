package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostTree builds a fake host root with the given files.
func hostTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(path, "/") {
			continue
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindContainerd(t *testing.T) {
	k3s := hostTree(t, map[string]string{
		"/run/k3s/containerd/containerd.sock":                       "",
		"/var/lib/rancher/k3s/agent/etc/containerd/config.toml":     "version = 3\n",
		"/var/lib/rancher/k3s/agent/etc/containerd/certs.d/docker/": "",
	})
	if l, err := findContainerd(k3s, ""); err != nil || l.Name != "k3s" {
		t.Fatalf("k3s = %+v, %v", l, err)
	}
	// RKE2 shares k3s's socket; its config tells them apart.
	rke2 := hostTree(t, map[string]string{
		"/run/k3s/containerd/containerd.sock":                    "",
		"/var/lib/rancher/rke2/agent/etc/containerd/config.toml": "",
	})
	if l, err := findContainerd(rke2, ""); err != nil || l.Name != "rke2" {
		t.Fatalf("rke2 = %+v, %v", l, err)
	}
	// A socket without any known config still names its layout.
	bare := hostTree(t, map[string]string{"/run/containerd/containerd.sock": ""})
	if l, err := findContainerd(bare, ""); err != nil || l.Name != "containerd" || l.Socket != "/run/containerd/containerd.sock" {
		t.Fatalf("bare = %+v, %v", l, err)
	}
	if _, err := findContainerd(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "set the cluster's containerd socket") {
		t.Fatalf("no containerd = %v", err)
	}
	custom := hostTree(t, map[string]string{"/data/containerd.sock": ""})
	if l, err := findContainerd(custom, "/data/containerd.sock"); err != nil || l.Socket != "/data/containerd.sock" || l.Config != "" {
		t.Fatalf("override = %+v, %v", l, err)
	}
	if _, err := findContainerd(custom, "/missing.sock"); err == nil {
		t.Fatal("a missing override socket was accepted")
	}
}

func TestCheckRuntime(t *testing.T) {
	for _, ok := range []string{"containerd://2.2.7-k3s1", "containerd://v1.7.22"} {
		if err := checkRuntime(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, other := range []string{"cri-o://1.30.4", "docker://27.1.1", ""} {
		if err := checkRuntime(other); !errors.Is(err, errNotContainerd) {
			t.Fatalf("%q = %v", other, err)
		}
	}
}

func TestPrepareHost(t *testing.T) {
	root := hostTree(t, map[string]string{
		"/run/k3s/containerd/containerd.sock":                       "",
		"/var/lib/rancher/k3s/agent/etc/containerd/config.toml":     "version = 3\n",
		"/var/lib/rancher/k3s/agent/etc/containerd/certs.d/docker/": "",
	})
	state, bin := t.TempDir(), t.TempDir()
	nerdctl := filepath.Join(t.TempDir(), "nerdctl")
	if err := os.WriteFile(nerdctl, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools, err := prepareHost(root, state, bin, "", nerdctl)
	if err != nil || tools.Layout.Name != "k3s" || tools.Bin != bin {
		t.Fatalf("prepareHost = %+v, %v", tools, err)
	}
	toml, _ := os.ReadFile(filepath.Join(state, "kube-tools", "nerdctl.toml"))
	want := `address = "/run/k3s/containerd/containerd.sock"` + "\n" +
		`data_root = "` + filepath.Join(state, "kube-tools", "nerdctl-data") + `"` + "\n" +
		`hosts_dir = ["/var/lib/rancher/k3s/agent/etc/containerd/certs.d"]` + "\n"
	if string(toml) != want {
		t.Fatalf("nerdctl.toml =\n%s\nwant\n%s", toml, want)
	}
	wrapper, _ := os.ReadFile(filepath.Join(bin, "nerdctl"))
	if !strings.Contains(string(wrapper), "exec nsenter -t 1 -m -- '"+filepath.Join(state, "kube-tools", "nerdctl")+"' \"$@\"") ||
		!strings.Contains(string(wrapper), "NERDCTL_TOML='"+filepath.Join(state, "kube-tools", "nerdctl.toml")+"'") {
		t.Fatalf("wrapper = %s", wrapper)
	}
	if got, _ := os.ReadFile(filepath.Join(state, "kube-tools", "nerdctl")); string(got) != "binary-v1" {
		t.Fatalf("nerdctl copy = %q", got)
	}
	// A new agent image brings a new nerdctl, which replaces the copy.
	if err := os.WriteFile(nerdctl, []byte("binary-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHost(root, state, bin, "", nerdctl); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(state, "kube-tools", "nerdctl")); string(got) != "binary-v2" {
		t.Fatalf("nerdctl copy after upgrade = %q", got)
	}
	// Without a certs.d, no hosts_dir is configured.
	plain := hostTree(t, map[string]string{"/run/containerd/containerd.sock": ""})
	if _, err := prepareHost(plain, state, bin, "", nerdctl); err != nil {
		t.Fatal(err)
	}
	if toml, _ := os.ReadFile(filepath.Join(state, "kube-tools", "nerdctl.toml")); strings.Contains(string(toml), "hosts_dir") {
		t.Fatalf("hosts_dir without a certs.d: %s", toml)
	}
	if _, err := prepareHost(t.TempDir(), state, bin, "", nerdctl); err == nil {
		t.Fatal("prepared a host without containerd")
	}
	if _, err := prepareHost(root, state, bin, "", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("prepared a host without a nerdctl binary")
	}
}

func TestGroupID(t *testing.T) {
	root := hostTree(t, map[string]string{"/etc/group": "root:x:0:\nvideo:x:39:udocker\nrender:x:109:\n"})
	if g, err := groupID(root, "video"); err != nil || g != 39 {
		t.Fatalf("video = %d, %v", g, err)
	}
	if _, err := groupID(root, "missing"); err == nil {
		t.Fatal("a missing group resolved")
	}
	if _, err := groupID(t.TempDir(), "video"); err == nil {
		t.Fatal("resolved without /etc/group")
	}
}

func TestHostCommand(t *testing.T) {
	bin, args := hostCommand("mount", "-t", "nfs", "srv:/x", "/opt/gpuworks/s")
	if bin != "nsenter" || strings.Join(args, " ") != "-t 1 -m -- mount -t nfs srv:/x /opt/gpuworks/s" {
		t.Fatalf("hostCommand = %s %v", bin, args)
	}
}

// TestShimSocket: the agent's parent chain leads to its container's shim,
// whose -address names the containerd the kubelet uses, even when the node
// runs another one (Docker's) at the default path.
func TestShimSocket(t *testing.T) {
	proc := t.TempDir()
	write := func(pid, ppid int, comm string, args ...string) {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 1 1 0", pid, comm, ppid)), 0o644)
	}
	write(900, 1, "containerd-shim", "/var/lib/rancher/k3s/data/current/bin/containerd-shim-runc-v2", "-namespace", "k8s.io", "-id", "abc", "-address", "/run/k3s/containerd/containerd.sock")
	write(950, 900, "sh (x)", "/bin/sh", "-c", "gpuworks-agent run")
	write(990, 950, "gpuworks-agent", "/usr/bin/gpuworks-agent", "run")
	if got := shimSocket(proc, 990); got != "/run/k3s/containerd/containerd.sock" {
		t.Fatalf("shimSocket = %q", got)
	}
	write(800, 1, "bash", "/bin/bash")
	if got := shimSocket(proc, 800); got != "" {
		t.Fatalf("no shim ancestor = %q", got)
	}
	if got := shimSocket(proc, 4242); got != "" {
		t.Fatalf("missing process = %q", got)
	}
	// The shim's socket wins over Docker's containerd at the default path.
	root := hostTree(t, map[string]string{
		"/run/containerd/containerd.sock":                       "",
		"/etc/containerd/config.toml":                           "",
		"/run/k3s/containerd/containerd.sock":                   "",
		"/var/lib/rancher/k3s/agent/etc/containerd/config.toml": "",
	})
	if l, err := findContainerd(root, shimSocket(proc, 990)); err != nil || l.Name != "k3s" {
		t.Fatalf("layout = %+v, %v", l, err)
	}
}

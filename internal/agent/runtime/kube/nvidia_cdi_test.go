package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The specs as a GPU Operator node (v26.7, default CDI mode) had them on
// 2026-11-13: the toolkit's management spec with only "all", the device
// plugin's spec with one device per card UUID.
const (
	cdiManagementSpec = `---
cdiVersion: 0.5.0
kind: management.nvidia.com/gpu
devices:
- name: all
  containerEdits:
    deviceNodes:
    - path: /dev/nvidia0
      hostPath: /run/nvidia/driver/dev/nvidia0
containerEdits:
  env:
  - NVIDIA_VISIBLE_DEVICES=void
`
	cdiDevicePluginSpec = `{"cdiVersion":"0.5.0","kind":"k8s.device-plugin.nvidia.com/gpu","devices":[` +
		`{"name":"GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c","containerEdits":{"deviceNodes":[{"path":"/dev/nvidia0","hostPath":"/run/nvidia/driver/dev/nvidia0"}]}},` +
		`{"name":"GPU-11111111-2222-3333-4444-555555555555","containerEdits":{"deviceNodes":[{"path":"/dev/nvidia1"}]}}],` +
		`"containerEdits":{"env":["NVIDIA_VISIBLE_DEVICES=void"]}}`
)

func writeHostFile(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

var operatorCards = map[int]string{0: "GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c", 1: "GPU-11111111-2222-3333-4444-555555555555"}

func TestTheGPUOperatorsCDIModeGetsFullyQualifiedCardNames(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "var/run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	writeHostFile(t, root, "var/run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	writeHostFile(t, root, "var/run/cdi/other-vendor.yaml", "kind: vendor.example/thing\ndevices: [{name: GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c}]\n")
	writeHostFile(t, root, "var/run/cdi/broken.json", "{not json")
	cdi := readNVIDIACDI(root)
	if !cdi.Toolkit || len(cdi.Names) != 2 {
		t.Fatalf("cdi = %+v", cdi)
	}
	for in, want := range map[string]string{
		"0":   "k8s.device-plugin.nvidia.com/gpu=GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c",
		"1,0": "k8s.device-plugin.nvidia.com/gpu=GPU-11111111-2222-3333-4444-555555555555,k8s.device-plugin.nvidia.com/gpu=GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c",
		"GPU-11111111-2222-3333-4444-555555555555": "k8s.device-plugin.nvidia.com/gpu=GPU-11111111-2222-3333-4444-555555555555",
		"all": "all", "void": "void",
	} {
		if got, err := cdi.qualify(in, operatorCards); err != nil || got != want {
			t.Fatalf("qualify(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A card no spec names: refused with the way out, not sent to fail later.
	_, err := cdi.qualify("2", map[int]string{2: "GPU-not-there"})
	if err == nil || !strings.Contains(err.Error(), "NVIDIA_CONTAINER_RUNTIME_MODE=legacy") || !strings.Contains(err.Error(), "GPU-not-there") {
		t.Fatalf("unnamed card: %v", err)
	}
}

func TestTheDevicePluginsNamesWinOverTheToolkitsPerDeviceSpec(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "etc/cdi/nvidia.yaml", "kind: nvidia.com/gpu\ndevices:\n- name: \"0\"\n- name: GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c\n- name: all\n")
	writeHostFile(t, root, "var/run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	if got := readNVIDIACDI(root).Names["GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c"]; got != "nvidia.com/gpu=GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c" {
		t.Fatalf("toolkit spec alone = %q", got)
	}
	writeHostFile(t, root, "var/run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	if got := readNVIDIACDI(root).Names["GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c"]; !strings.HasPrefix(got, "k8s.device-plugin.nvidia.com/gpu=") {
		t.Fatalf("with the device plugin's spec = %q", got)
	}
}

// Without the toolkit's CDI mode the runtime takes plain indexes: a host
// toolkit (no management spec), or the operator's toolkit set back to
// legacy, whose management spec stays behind.
func TestPlainIndexesStayWithoutTheToolkitsCDIMode(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "var/run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	d := &Driver{o: Options{HostRoot: root, Cards: func(context.Context) (map[int]string, error) { return operatorCards, nil }}}
	if d.nvidiaVisible(context.Background()) != nil {
		t.Fatal("no management spec: indexes must stay")
	}
	writeHostFile(t, root, "var/run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	writeHostFile(t, root, "usr/local/nvidia/toolkit/.config/nvidia-container-runtime/config.toml",
		"disable-require = false\n\n[nvidia-container-cli]\n  mode = \"other\"\n\n[nvidia-container-runtime]\n  log-level = \"info\"\n  mode = \"legacy\"\n")
	if d.nvidiaVisible(context.Background()) != nil {
		t.Fatal("toolkit in legacy mode: indexes must stay")
	}
	writeHostFile(t, root, "usr/local/nvidia/toolkit/.config/nvidia-container-runtime/config.toml", "[nvidia-container-runtime]\nmode = \"cdi\"\n")
	rewrite := d.nvidiaVisible(context.Background())
	if rewrite == nil {
		t.Fatal("toolkit in CDI mode: no rewrite")
	}
	if got, err := rewrite("1"); err != nil || got != "k8s.device-plugin.nvidia.com/gpu=GPU-11111111-2222-3333-4444-555555555555" {
		t.Fatalf("rewrite(1) = %q, %v", got, err)
	}
	if (&Driver{}).nvidiaVisible(context.Background()) != nil {
		t.Fatal("without the host's root nothing is read")
	}
}

func TestAnInstancePodNamesItsNVIDIACardTheWayTheNodeResolvesIt(t *testing.T) {
	spec := runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", `"device=1"`}}
	o := podOptions{Namespace: "ns", NodeName: "n", RuntimeClass: "nvidia",
		NVIDIAVisible: func(v string) (string, error) { return "k8s.device-plugin.nvidia.com/gpu=" + operatorCards[1], nil }}
	pod, err := instancePod(spec, o)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "NVIDIA_VISIBLE_DEVICES" {
			got = e.Value
		}
	}
	if got != "k8s.device-plugin.nvidia.com/gpu="+operatorCards[1] || pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("env %q, runtime class %v", got, pod.Spec.RuntimeClassName)
	}
	// A workload that sets the variable itself keeps its own value.
	spec.Env = map[string]string{"NVIDIA_VISIBLE_DEVICES": "void"}
	if pod, err = instancePod(spec, o); err != nil {
		t.Fatal(err)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "NVIDIA_VISIBLE_DEVICES" && e.Value != "void" {
			t.Fatalf("workload's own value replaced: %q", e.Value)
		}
	}
	// A card the node cannot name fails the pod before it is created.
	o.NVIDIAVisible = func(string) (string, error) { return "", os.ErrNotExist }
	spec.Env = nil
	if _, err := instancePod(spec, o); err == nil {
		t.Fatal("an unresolvable card made a pod")
	}
}

// On Ubuntu /var/run is an absolute symlink to /run. Read through the host's
// root it points into the agent's own file system, so the specs are found
// under /run/cdi.
func TestTheSpecsAreFoundWhereVarRunIsASymlinkToRun(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	writeHostFile(t, root, "run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	cdi := readNVIDIACDI(root)
	if !cdi.Toolkit || cdi.Names["GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c"] == "" {
		t.Fatalf("specs under /run/cdi behind a /var/run symlink = %+v", cdi)
	}
}

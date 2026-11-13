package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
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
		if got, err := cdi.qualify(in, operatorCards); err != nil || got != (nvidiaVisibility{Devices: want}) {
			t.Fatalf("qualify(%q) = %+v, %v; want %q", in, got, err, want)
		}
	}
}

// HAMi (or another device plugin) replaced NVIDIA's, so no spec names the
// cards: the pod gets every card through the management spec's "all" and
// CUDA_VISIBLE_DEVICES holds it to its own, by the host's index.
func TestAnUnnamedCardFallsBackToAllCardsLimitedByCUDA(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	cdi := readNVIDIACDI(root)
	if !cdi.Toolkit || len(cdi.Names) != 0 {
		t.Fatalf("cdi = %+v", cdi)
	}
	cards := map[int]string{0: "GPU-a", 1: "GPU-b", 2: "GPU-c"}
	all := nvidiaCDIManagementKind + "=all"
	for in, want := range map[string]nvidiaVisibility{
		"1":        {Devices: all, CUDAVisible: "1"},
		"2,0":      {Devices: all, CUDAVisible: "2,0"},
		"GPU-c":    {Devices: all, CUDAVisible: "2"},
		"GPU-b, 0": {Devices: all, CUDAVisible: "1,0"},
		"all":      {Devices: "all"},
		"void":     {Devices: "void"},
		"none":     {Devices: "none"},
		"":         {},
	} {
		if got, err := cdi.qualify(in, cards); err != nil || got != want {
			t.Fatalf("qualify(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	// When the probe cannot say, a card index is still the host's index.
	if got, err := cdi.qualify("1,3", nil); err != nil || got != (nvidiaVisibility{Devices: all, CUDAVisible: "1,3"}) {
		t.Fatalf("without the node's cards: %+v, %v", got, err)
	}
	// A UUID that is none of the node's cards has no index to confine CUDA to.
	if _, err := cdi.qualify("GPU-nope", cards); err == nil || !strings.Contains(err.Error(), "GPU-nope") {
		t.Fatalf("a card the node does not have: %v", err)
	}
	// One named card among unnamed ones is not enough: the pod falls back for all of them.
	partial := nvidiaCDI{Toolkit: true, Names: map[string]string{"GPU-a": "k8s.device-plugin.nvidia.com/gpu=GPU-a"}}
	if got, err := partial.qualify("0,1", cards); err != nil || got != (nvidiaVisibility{Devices: all, CUDAVisible: "0,1"}) {
		t.Fatalf("partly named: %+v, %v", got, err)
	}
}

// The node says so while its pods run on the fallback, and stops when a spec
// names every card or the toolkit goes back to legacy mode.
func TestTheNodeReportsSoftIsolationWhileNoSpecNamesItsCards(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	cards := map[int]string{0: "GPU-88e65fd5-74fb-5350-d3da-5a7bbdd4d19c", 1: "GPU-11111111-2222-3333-4444-555555555555", 2: "AMD-card"}
	d := &Driver{o: Options{HostRoot: root, Cards: func(context.Context) (map[int]string, error) { return cards, nil }}}
	if d.NVIDIAIsolation(ctx) != nil {
		t.Fatal("no toolkit spec: nothing to report")
	}
	const operatorConfig = "usr/local/nvidia/toolkit/.config/nvidia-container-runtime/config.toml"
	writeHostFile(t, root, "run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	writeHostFile(t, root, operatorConfig, "[nvidia-container-runtime]\nmode = \"cdi\"\n")
	got := d.NVIDIAIsolation(ctx)
	want := proto.NVIDIAIsolation{Level: "soft", Reason: "cdi_names_missing", Toolkit: "gpu_operator", Cards: 2}
	if got == nil || *got != want {
		t.Fatalf("isolation = %+v, want %+v", got, want)
	}
	// A host toolkit is told apart from the operator's.
	hostRoot := t.TempDir()
	writeHostFile(t, hostRoot, "run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	writeHostFile(t, hostRoot, "etc/nvidia-container-runtime/config.toml", "[nvidia-container-runtime]\nmode = \"auto\"\n")
	hostDriver := &Driver{o: Options{HostRoot: hostRoot, Cards: d.o.Cards}}
	if got := hostDriver.NVIDIAIsolation(ctx); got == nil || got.Toolkit != "host" {
		t.Fatalf("host toolkit: %+v", got)
	}
	// Once the device plugin names every NVIDIA card the isolation is strict.
	writeHostFile(t, root, "run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	if got := d.NVIDIAIsolation(ctx); got != nil {
		t.Fatalf("every card named: %+v", got)
	}
	// Legacy mode takes indexes: strict whatever the specs say.
	if err := os.Remove(filepath.Join(root, "run/cdi/k8s.device-plugin.nvidia.com-gpu.json")); err != nil {
		t.Fatal(err)
	}
	writeHostFile(t, root, operatorConfig, "[nvidia-container-runtime]\nmode = \"legacy\"\n")
	if got := d.NVIDIAIsolation(ctx); got != nil {
		t.Fatalf("legacy mode: %+v", got)
	}
	// Without the host's root nothing is read; with cards the probe cannot list, nothing is reported.
	if (&Driver{}).NVIDIAIsolation(ctx) != nil {
		t.Fatal("no host root")
	}
	writeHostFile(t, root, operatorConfig, "[nvidia-container-runtime]\nmode = \"cdi\"\n")
	d.o.Cards = func(context.Context) (map[int]string, error) { return nil, os.ErrNotExist }
	if got := d.NVIDIAIsolation(ctx); got != nil {
		t.Fatalf("unknown cards: %+v", got)
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
	if got, err := rewrite("1"); err != nil || got != (nvidiaVisibility{Devices: "k8s.device-plugin.nvidia.com/gpu=GPU-11111111-2222-3333-4444-555555555555"}) {
		t.Fatalf("rewrite(1) = %+v, %v", got, err)
	}
	if (&Driver{}).nvidiaVisible(context.Background()) != nil {
		t.Fatal("without the host's root nothing is read")
	}
}

func TestAnInstancePodNamesItsNVIDIACardTheWayTheNodeResolvesIt(t *testing.T) {
	spec := runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", `"device=1"`}}
	o := podOptions{Namespace: "ns", NodeName: "n", RuntimeClass: "nvidia",
		NVIDIAVisible: func(v string) (nvidiaVisibility, error) {
			return nvidiaVisibility{Devices: "k8s.device-plugin.nvidia.com/gpu=" + operatorCards[1]}, nil
		}}
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
	o.NVIDIAVisible = func(string) (nvidiaVisibility, error) { return nvidiaVisibility{}, os.ErrNotExist }
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

func podEnv(t *testing.T, spec runtime.WorkloadSpec, o podOptions) map[string]string {
	t.Helper()
	pod, err := instancePod(spec, o)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// A pod on the fallback sees every card and is held to its own by CUDA, by the
// host's index: the engine's own CUDA_VISIBLE_DEVICES counts from the pod's
// cards and would point at the wrong ones. Strict and legacy pods keep theirs.
func TestAPodOnTheSoftIsolationFallbackIsLimitedByCUDAToItsCards(t *testing.T) {
	root := t.TempDir()
	writeHostFile(t, root, "run/cdi/management.nvidia.com-gpu.yaml", cdiManagementSpec)
	d := &Driver{o: Options{HostRoot: root, Cards: func(context.Context) (map[int]string, error) { return operatorCards, nil }}}
	for _, tc := range []struct{ gpus, cuda string }{{`"device=1"`, "1"}, {`"device=1,0"`, "1,0"}} {
		// The engine's injection: --gpus with the host indexes, and CUDA_VISIBLE_DEVICES counted from the pod's cards.
		spec := runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", tc.gpus},
			Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0", "OTHER": "kept"}}
		env := podEnv(t, spec, podOptions{Namespace: "ns", NodeName: "n", RuntimeClass: "nvidia", NVIDIAVisible: d.nvidiaVisible(context.Background())})
		if env["NVIDIA_VISIBLE_DEVICES"] != "management.nvidia.com/gpu=all" || env["CUDA_VISIBLE_DEVICES"] != tc.cuda ||
			env["CUDA_DEVICE_ORDER"] != "PCI_BUS_ID" || env["OTHER"] != "kept" {
			t.Fatalf("%s: env = %v", tc.gpus, env)
		}
	}
	// A workload that picked its own device order and devices keeps them.
	spec := runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", `"device=1"`}, Env: map[string]string{"CUDA_DEVICE_ORDER": "FASTEST_FIRST"}}
	if env := podEnv(t, spec, podOptions{NVIDIAVisible: d.nvidiaVisible(context.Background())}); env["CUDA_DEVICE_ORDER"] != "FASTEST_FIRST" || env["CUDA_VISIBLE_DEVICES"] != "1" {
		t.Fatalf("own device order: %v", env)
	}
	spec = runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", `"device=1"`}, Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void", "CUDA_VISIBLE_DEVICES": "7"}}
	if env := podEnv(t, spec, podOptions{NVIDIAVisible: d.nvidiaVisible(context.Background())}); env["NVIDIA_VISIBLE_DEVICES"] != "void" || env["CUDA_VISIBLE_DEVICES"] != "7" {
		t.Fatalf("own devices: %v", env)
	}

	// Once the device plugin names the cards the pod is strict again: its card alone, nothing else set.
	writeHostFile(t, root, "run/cdi/k8s.device-plugin.nvidia.com-gpu.json", cdiDevicePluginSpec)
	spec = runtime.WorkloadSpec{Name: "gw-1", Image: "img", DockerArgs: []string{"--gpus", `"device=1"`}, Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"}}
	env := podEnv(t, spec, podOptions{NVIDIAVisible: d.nvidiaVisible(context.Background())})
	if env["NVIDIA_VISIBLE_DEVICES"] != "k8s.device-plugin.nvidia.com/gpu="+operatorCards[1] || env["CUDA_VISIBLE_DEVICES"] != "0" || env["CUDA_DEVICE_ORDER"] != "" {
		t.Fatalf("strict: env = %v", env)
	}
	// Legacy mode takes the plain index, as before.
	writeHostFile(t, root, "etc/nvidia-container-runtime/config.toml", "[nvidia-container-runtime]\nmode = \"legacy\"\n")
	if d.nvidiaVisible(context.Background()) != nil {
		t.Fatal("legacy mode: indexes must stay")
	}
	env = podEnv(t, spec, podOptions{NVIDIAVisible: d.nvidiaVisible(context.Background())})
	if env["NVIDIA_VISIBLE_DEVICES"] != "1" || env["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("legacy: env = %v", env)
	}
}

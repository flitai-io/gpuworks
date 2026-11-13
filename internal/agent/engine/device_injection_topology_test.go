package engine

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

type topologyInjectionProber struct {
	injectionProber
	indexes []int
	driver  string
	nodes   int
	env     map[string]string
	fail    bool
}

func (p *topologyInjectionProber) RuntimeInjectionForLaunch(_ context.Context, req gpu.InjectionRequest) gpu.RuntimeInjection {
	p.indexes, p.driver, p.nodes, p.env = append([]int(nil), req.Indexes...), req.DriverName, req.NNodes, req.Env
	if p.fail {
		return gpu.RuntimeInjection{UnsupportedReason: "transport unavailable"}
	}
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/example-transport"}}
}

func TestDeviceInjectionUsesTopologyExtensionWithoutVendorBranch(t *testing.T) {
	p := &topologyInjectionProber{}
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{
		DockerArgs: []string{"--ipc=host"},
		Env:        map[string]string{"BKCL_RDMA_NICS": "bond0,bond0", "GLOO_SOCKET_IFNAME": "bond0"},
	}
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{3, 7}, "docker", 2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.indexes, []int{3, 7}) || p.driver != "docker" || p.nodes != 2 || !reflect.DeepEqual(p.env, ws.Env) || !reflect.DeepEqual(ws.DockerArgs, []string{"--ipc=host", "--device", "/dev/example-transport"}) {
		t.Fatalf("p=%+v ws=%+v", p, ws)
	}
	p.fail = true
	before := append([]string(nil), ws.DockerArgs...)
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{3}, "docker", 2); err == nil || !reflect.DeepEqual(ws.DockerArgs, before) {
		t.Fatalf("failure changed workload: %+v, %v", ws, err)
	}
}

func TestDeviceInjectionAllowsEmptySelectionForBackendsThatSupportIt(t *testing.T) {
	// Preserve the existing engine-level no-injection contract, including an
	// optional topology extension that would reject or inject if called.
	p := &topologyInjectionProber{fail: true}
	withExtension := NewWithProber(&headlessTestDriver{}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{DockerArgs: []string{"--ipc=host"}}
	if err := withExtension.applyDeviceInjectionForLaunch(context.Background(), &ws, nil, "docker", 2); err != nil || len(ws.DockerArgs) != 1 || p.nodes != 0 {
		t.Fatalf("empty selection reached extension: %+v %+v %v", p, ws, err)
	}
	e := NewWithProber(&headlessTestDriver{}, nil, slog.Default(), t.TempDir(), gpu.NoAccelerator{})
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &runtime.WorkloadSpec{}, nil, "docker", 1); err != nil {
		t.Fatal(err)
	}
}

// An injector implementing all optional seams must receive the resolved args
// through the richest interface exactly once.
type allInjectionProber struct {
	topologyInjectionProber
	called string
	args   []string
}

func (p *allInjectionProber) RuntimeInjectionForDriver([]int, string) gpu.RuntimeInjection {
	p.called = "driver"
	return gpu.RuntimeInjection{UnsupportedReason: "wrong interface"}
}
func (p *allInjectionProber) RuntimeInjectionForLaunch(ctx context.Context, req gpu.InjectionRequest) gpu.RuntimeInjection {
	p.called = "resolved"
	p.args = append([]string(nil), req.DockerArgs...)
	p.env = req.Env
	if err := ctx.Err(); err != nil {
		return gpu.RuntimeInjection{UnsupportedReason: err.Error()}
	}
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/xdrdrv"}}
}
func TestResolvedInjectionPrecedesOtherOptionalInterfaces(t *testing.T) {
	p := &allInjectionProber{}
	e := NewWithProber(&headlessTestDriver{name: "docker"}, nil, slog.Default(), t.TempDir(), p)
	ws := runtime.WorkloadSpec{DockerArgs: []string{"--device", "/dev/infiniband"}, Env: map[string]string{"GLOO_SOCKET_IFNAME": "bond0"}}
	if err := e.applyDeviceInjectionForLaunch(context.Background(), &ws, []int{1}, "docker", 2); err != nil {
		t.Fatal(err)
	}
	if p.called != "resolved" || !reflect.DeepEqual(p.args, []string{"--device", "/dev/infiniband"}) || !reflect.DeepEqual(p.env, ws.Env) || p.nodes != 0 {
		t.Fatalf("dispatch: %+v", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := append([]string(nil), ws.DockerArgs...)
	if err := e.applyDeviceInjectionForLaunch(ctx, &ws, []int{1}, "docker", 2); err == nil || !reflect.DeepEqual(ws.DockerArgs, before) {
		t.Fatalf("cancel mutated workload: %v %+v", err, ws)
	}
}

func TestKunlunManagedSelectionUsesAtomicMergeAndPreservesResolvedRDMA(t *testing.T) {
	ws := runtime.WorkloadSpec{
		Env: map[string]string{
			"XPU_VISIBLE_DEVICES": "0",
			"KEEP":                "value",
			"BKCL_RDMA_NICS":      "bond0,bond0",
			"GLOO_SOCKET_IFNAME":  "bond0",
			"BKCL_ENABLE_XDR":     "stale",
		},
		DockerArgs: []string{
			"--device", "/dev/xpu0",
			"--device=/dev/xpu1:/dev/xpu1:rwm",
			"--device", "/dev/xpuctrl",
			"--device", "/dev/xdrdrv",
			"--device", "/dev/infiniband",
			"--ulimit", "memlock=-1",
			"--cap-add", "IPC_LOCK",
		},
	}
	inj := gpu.RuntimeInjection{
		ManagedSelection: &gpu.RuntimeSelectionCleanup{
			Env:         []string{"XPU_VISIBLE_DEVICES"},
			DevicePaths: []string{"/dev/xpu0", "/dev/xpu1", "/dev/xpuctrl", "/dev/xdrdrv"},
		},
		Env: map[string]string{
			"BKCL_ENABLE_XDR":    "1",
			"BKCL_RDMA_VERBS":    "1",
			"BKCL_ENABLE_IBGDA":  "0",
			"BKCL_SOCKET_IFNAME": "bond0",
		},
		DockerArgs: []string{
			"--runtime", "runc",
			"--device", "/dev/xpu1:/dev/xpu1:rwm",
			"--device", "/dev/xpuctrl:/dev/xpuctrl:rwm",
			"--device", "/dev/xdrdrv",
		},
	}
	applyRuntimeInjection(&ws, inj, []int{1}, "docker")
	want := []string{
		"--device", "/dev/infiniband",
		"--ulimit", "memlock=-1",
		"--cap-add", "IPC_LOCK",
		"--runtime", "runc",
		"--device", "/dev/xpu1:/dev/xpu1:rwm",
		"--device", "/dev/xpuctrl:/dev/xpuctrl:rwm",
		"--device", "/dev/xdrdrv",
	}
	if !reflect.DeepEqual(ws.DockerArgs, want) {
		t.Fatalf("merged args = %v, want %v", ws.DockerArgs, want)
	}
	wantEnv := map[string]string{
		"KEEP":               "value",
		"BKCL_RDMA_NICS":     "bond0,bond0",
		"GLOO_SOCKET_IFNAME": "bond0",
		"BKCL_ENABLE_XDR":    "1",
		"BKCL_RDMA_VERBS":    "1",
		"BKCL_ENABLE_IBGDA":  "0",
		"BKCL_SOCKET_IFNAME": "bond0",
	}
	if !reflect.DeepEqual(ws.Env, wantEnv) {
		t.Fatalf("merged environment = %v, want %v", ws.Env, wantEnv)
	}
}

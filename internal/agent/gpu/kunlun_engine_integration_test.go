package gpu_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// Stop at the runtime boundary, after the actual Engine has applied the real
// injection. Unimplemented driver operations panic rather than fake success.
type recordingLaunchDriver struct {
	runtime.Driver
	launch chan runtime.WorkloadSpec
}

func (*recordingLaunchDriver) Name() string { return "docker" }
func (*recordingLaunchDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{}, nil
}
func (*recordingLaunchDriver) ImageExists(context.Context, string) (bool, error) { return true, nil }
func (*recordingLaunchDriver) Stop(context.Context, string) error                { return nil }
func (*recordingLaunchDriver) Remove(context.Context, string) error              { return nil }
func (d *recordingLaunchDriver) Run(_ context.Context, w runtime.WorkloadSpec) (string, error) {
	d.launch <- w
	return "", errors.New("test stopped at runtime boundary")
}

func driveInjection(t *testing.T, ctx context.Context, p gpu.Prober, spec proto.InstanceDesiredState) (*runtime.WorkloadSpec, proto.InstanceStatus) {
	t.Helper()
	driver := &recordingLaunchDriver{launch: make(chan runtime.WorkloadSpec, 1)}
	terminal := make(chan proto.InstanceStatus, 8)
	e := engine.NewWithProber(driver, func(s proto.InstanceStatus) {
		if s.State == proto.InstFailed {
			terminal <- s
		}
	}, nil, t.TempDir(), p, engine.WithHostRuntimeFacts(func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }))
	e.SetDesiredFull(ctx, []proto.InstanceDesiredState{spec})
	var status proto.InstanceStatus
	select {
	case status = <-terminal:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not finish injection")
	}
	select {
	case w := <-driver.launch:
		return &w, status
	default:
		return nil, status
	}
}

func TestResolvedNetworkThroughEngineAndCapturedKunlun(t *testing.T) {
	for _, mode := range []string{"success", "duplicate-network-args", "finite-memlock", "missing-xdr", "cancel-xdr", "duplicate-selection"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var cancelAtXDR func()
			if mode == "cancel-xdr" {
				cancelAtXDR = cancel
			}
			p := gpu.CapturedKunlunForTest(t, mode == "missing-xdr", cancelAtXDR)
			eng, err := deploy.SelectEngine("vllm")
			if err != nil {
				t.Fatal(err)
			}
			input := deploy.ResolveLaunchInput{Engine: eng, Params: deploy.Params{Vendor: "kunlunxin", ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 2, NNodes: 2, MasterAddr: "192.0.2.1", MasterPort: 29500, ComputeNetwork: deploy.ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_1:1"}}}}
			if mode == "finite-memlock" {
				input.ContainerOverrides.DockerArgs = []string{"--ulimit", "memlock=4096"}
			}
			if mode == "duplicate-network-args" {
				input.ContainerOverrides.DockerArgs = []string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}
			}
			launch, err := deploy.ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			launch.Env["BKCL_RDMA_NICS"] = "bond0,bond0"
			indexes := []int{7}
			if mode == "duplicate-selection" {
				indexes = []int{7, 7}
			}
			before := append([]string(nil), launch.DockerArgs...)
			spec := proto.InstanceDesiredState{UUID: "kunlun-integration", DesiredState: "running", Image: "fixture:present", Engine: "vllm", GPUIndexes: indexes, NNodes: 2, HostNetwork: true, ResolvedLaunch: launch}
			got, status := driveInjection(t, ctx, p, spec)
			if !reflect.DeepEqual(before, launch.DockerArgs) {
				t.Fatal("borrowed launch args mutated")
			}
			if mode != "success" && mode != "duplicate-network-args" {
				if got != nil {
					t.Fatalf("rejected launch reached runtime: %+v", got)
				}
				cause := map[string]string{"finite-memlock": "finite", "missing-xdr": "XDR", "cancel-xdr": "canceled", "duplicate-selection": "duplicate"}[mode]
				if !strings.Contains(status.Message, cause) {
					t.Fatalf("wrong failure: %+v", status)
				}
				return
			}
			if got == nil {
				t.Fatalf("never reached runtime: %+v", status)
			}
			for _, arg := range []string{"/dev/infiniband", "memlock=-1", "IPC_LOCK", "/dev/xdrdrv", "/dev/xpu7:/dev/xpu7:rwm", "/dev/xpuctrl:/dev/xpuctrl:rwm"} {
				count := 0
				for _, value := range got.DockerArgs {
					if value == arg {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("%s appears %d times: %v", arg, count, got.DockerArgs)
				}
			}
			if got.Env["NCCL_IB_HCA"] != "mlx5_1:1" {
				t.Fatalf("network rail overwritten: %v", got.Env)
			}
			if got.Env["XPU_VISIBLE_DEVICES"] != "0" {
				t.Fatalf("container-logical XPU selection = %+v", got.Env)
			}
		})
	}
}

func TestSingleNodeKunlunDPThroughEngineAndCapturedKunlun(t *testing.T) {
	eng, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{Engine: eng, Params: deploy.Params{
		Vendor: "kunlunxin", ModelPath: "/model", AssignedGPUs: 2,
		TensorParallel: 1, PipelineParallel: 1, DataParallel: 2, NNodes: 1,
		Quick:          deploy.LaunchConfig{DP: 2},
		ComputeNetwork: deploy.ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "br_eno1_31"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	dpRendered := false
	for i, arg := range launch.Argv {
		if arg == "--data-parallel-size" && i+1 < len(launch.Argv) && launch.Argv[i+1] == "2" {
			dpRendered = true
		}
	}
	if !dpRendered || launch.Env["GLOO_SOCKET_IFNAME"] != "br_eno1_31" || launch.Env["NCCL_SOCKET_IFNAME"] != "br_eno1_31" {
		t.Fatalf("single-node DP launch = %+v", launch)
	}

	spec := proto.InstanceDesiredState{
		UUID: "kunlun-single-node-dp", DesiredState: "running", Image: "fixture:present", Engine: "vllm",
		GPUIndexes: []int{6, 7}, NNodes: 1, HostNetwork: true, ResolvedLaunch: launch,
	}
	got, status := driveInjection(t, t.Context(), gpu.CapturedKunlunForTest(t, false, nil), spec)
	if got == nil {
		t.Fatalf("single-node DP injection rejected: %+v", status)
	}
	if got.Env["BKCL_SOCKET_IFNAME"] != "br_eno1_31" {
		t.Fatalf("injected collective interface = %q", got.Env["BKCL_SOCKET_IFNAME"])
	}
	for _, device := range []string{"/dev/xpu6:/dev/xpu6:rwm", "/dev/xpu7:/dev/xpu7:rwm"} {
		found := false
		for _, arg := range got.DockerArgs {
			found = found || arg == device
		}
		if !found {
			t.Fatalf("selected device %s missing: %v", device, got.DockerArgs)
		}
	}
}

func TestExistingNativeBackendsThroughEngineTopologySeam(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       gpu.Prober
		indexes []int
		device  string
	}{
		{"hygon", gpu.CapturedHygonForTest(t), []int{1}, "/dev/dri/renderD129:/dev/dri/renderD129:rwm"},
		{"amd", gpu.NativeAMDForTest(), []int{7}, "/dev/dri/renderD135:/dev/dri/renderD135:rwm"},
		{"ppu", gpu.NativePPUForTest(), []int{8}, "/dev/alixpu_ppu9:/dev/alixpu_ppu9:rwm"},
	} {
		for _, nodes := range []int{1, 2} {
			t.Run(tc.name, func(t *testing.T) {
				spec := proto.InstanceDesiredState{UUID: "backend-contract", DesiredState: "running", Image: "fixture:present", Engine: "vllm", GPUIndexes: tc.indexes, NNodes: nodes, HostNetwork: true, ResolvedLaunch: proto.ResolvedLaunch{Env: map[string]string{"USER_SETTING": "keep", "GLOO_SOCKET_IFNAME": "bond0", "NCCL_SOCKET_IFNAME": "bond0"}, DockerArgs: []string{"--ulimit=memlock=65536:65536"}}}
				got, status := driveInjection(t, t.Context(), tc.p, spec)
				if got == nil {
					t.Fatalf("backend rejected: %+v", status)
				}
				if got.Env["USER_SETTING"] != "keep" || got.DockerArgs[0] != "--ulimit=memlock=65536:65536" {
					t.Fatalf("user settings changed: %+v", got)
				}
				found := false
				for _, arg := range got.DockerArgs {
					found = found || arg == tc.device
					if arg == "/dev/xdrdrv" {
						t.Fatal("Kunlun transport leaked into another vendor")
					}
				}
				if !found {
					t.Fatalf("selected device missing: %v", got.DockerArgs)
				}
			})
		}
	}
}

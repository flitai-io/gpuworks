package udocker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// A vendor whose images set CUDA_VISIBLE_DEVICES is, read from the variables
// alone, an NVIDIA one and gets the NVIDIA shim. Named by the injection it is
// not, and this driver has no branch for it: Kunlunxin stands for any vendor
// accelerator.NormalizeVendor knows and this driver does not.
func TestNamedVendorDecidesNVIDIASetup(t *testing.T) {
	cuda := map[string]string{"CUDA_VISIBLE_DEVICES": "0"}
	for _, tc := range []struct {
		name string
		spec runtime.WorkloadSpec
		want bool
	}{
		{"unnamed with CUDA devices is read as NVIDIA", runtime.WorkloadSpec{Env: cuda}, true},
		{"unnamed with nothing is prepared as NVIDIA", runtime.WorkloadSpec{}, true},
		{"unnamed with Hygon devices", runtime.WorkloadSpec{Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}}, false},
		{"named NVIDIA", runtime.WorkloadSpec{Env: cuda, Grant: runtime.DeviceGrant{Vendor: accelerator.VendorNVIDIA}}, true},
		{"named PPU, which also sets CUDA devices", runtime.WorkloadSpec{Env: cuda, Grant: runtime.DeviceGrant{Vendor: accelerator.VendorPPU}}, false},
		{"a vendor this driver has no branch for", runtime.WorkloadSpec{Env: cuda, Grant: runtime.DeviceGrant{Vendor: accelerator.VendorKunlunxin}}, false},
		{"the name wins over a stale variable", runtime.WorkloadSpec{Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}, Grant: runtime.DeviceGrant{Vendor: accelerator.VendorNVIDIA}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsNVIDIASetup(tc.spec); got != tc.want {
				t.Fatalf("needsNVIDIASetup = %v, want %v", got, tc.want)
			}
		})
	}
}

// The same decision as the container sees it: no NVIDIA loader path, NCCL
// settings or nvidia-smi for a named vendor that is not NVIDIA, while the
// selection the injection made is passed through untouched.
func TestNamedNonNVIDIAVendorGetsNoNVIDIAEnvironment(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	spec := runtime.WorkloadSpec{
		Name:       "bridge",
		GPUIndexes: []int{3},
		Env:        map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
		Grant:      runtime.DeviceGrant{Vendor: accelerator.VendorKunlunxin},
	}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, leaked := range []string{"nvidia-shim", "NCCL_IB_DISABLE", "nvidia-smi", "CUDA_VISIBLE_DEVICES=3"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("a non-NVIDIA workload got NVIDIA setup (%s): %s", leaked, joined)
		}
	}
	if !strings.Contains(joined, "CUDA_VISIBLE_DEVICES=0") {
		t.Fatalf("the injected selection was not passed through: %s", joined)
	}

	spec.Grant.Vendor = accelerator.VendorNVIDIA
	joined = strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"nvidia-shim", "NCCL_IB_DISABLE"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("an NVIDIA workload is missing %s: %s", want, joined)
		}
	}
}

// A granted device node is bound into the container as it is, without being
// written as a docker option first, and whichever vendor's it is. A device
// the workload's own options name is still bound too.
func TestGrantedDevicesBecomeUdockerVolumes(t *testing.T) {
	d := New("/tmp/base")
	spec := runtime.WorkloadSpec{
		Name:       "granted",
		DockerArgs: []string{"--device", "/dev/infiniband"},
		Grant:      runtime.DeviceGrant{Vendor: accelerator.VendorKunlunxin, Devices: []runtime.Device{{Path: "/dev/vendorctl", Access: "rwm"}, {Path: "/dev/dri/renderD128"}}},
	}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/vendorctl:/dev/vendorctl", "--volume=/dev/dri/renderD128:/dev/dri/renderD128", "--volume=/dev/infiniband:/dev/infiniband"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q: %s", want, joined)
		}
	}
	if strings.Count(joined, "--volume=/dev/vendorctl:/dev/vendorctl") != 1 {
		t.Fatalf("a granted device was bound more than once: %s", joined)
	}
}

// A workload recorded by an agent that wrote its grant as docker options has
// its devices only there and names no vendor. Its devices are bound and its
// vendor is inferred, so a terminal in a container that agent started works.
func TestWorkloadThatNamesNoVendorIsBoundFromItsOptions(t *testing.T) {
	d := New("/tmp/base")
	d.nvidiaSMIPath = "/host/bin/nvidia-smi"
	spec := runtime.WorkloadSpec{
		Name:       "recorded",
		DockerArgs: []string{"--device", "/dev/kfd:/dev/kfd:rwm", "--device=/dev/dri/renderD128:/dev/dri/renderD128:rwm"},
		Env:        map[string]string{"HYGON_VISIBLE_DEVICES": "1"},
	}
	joined := strings.Join(d.containerRunArgs(spec), " ")
	for _, want := range []string{"--volume=/dev/kfd:/dev/kfd", "--volume=/dev/dri/renderD128:/dev/dri/renderD128"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "NCCL_IB_DISABLE") || strings.Contains(joined, "nvidia-smi") {
		t.Fatalf("a recorded Hygon workload got NVIDIA setup: %s", joined)
	}
}

// A terminal opened after the agent restarted is built from the spec the
// driver recorded on disk. The granted devices and the vendor's name have to
// survive that round trip, or the shell would see none of the cards the
// workload has and would be prepared as an NVIDIA one.
func TestRecordedSpecKeepsGrantedDevicesAcrossRestart(t *testing.T) {
	t.Setenv(launcherEnv, "udocker") // pins the udocker CLI launcher
	base := t.TempDir()
	first := New(base)
	if err := os.MkdirAll(first.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	granted := runtime.WorkloadSpec{
		Env: map[string]string{"HIP_VISIBLE_DEVICES": "0"},
		Grant: runtime.DeviceGrant{Vendor: accelerator.VendorHygon,
			Devices: []runtime.Device{{Path: "/dev/kfd", Access: "rwm"}, {Path: "/dev/dri/renderD129", Access: "rwm"}},
			Groups:  []int{39}},
	}
	if err := first.rememberSpec("demo", granted); err != nil {
		t.Fatal(err)
	}
	// What was recorded is the driver's own copy, in memory as on disk: the
	// caller changing its spec afterwards changes neither.
	granted.Grant.Devices[0].Path = "/dev/changed-after-recording"
	granted.Grant.Groups[0] = 7
	held, ok, err := first.loadSpec("demo")
	if err != nil || !ok {
		t.Fatalf("loadSpec = %v, %v", ok, err)
	}
	if held.Grant.Devices[0].Path != "/dev/kfd" || held.Grant.Groups[0] != 39 {
		t.Fatalf("the driver's recorded spec follows the caller's: devices %+v groups %v", held.Grant.Devices, held.Grant.Groups)
	}

	restarted := New(base)
	restarted.provisioned = true
	restarted.nvidiaSMIPath = "/host/bin/nvidia-smi"
	restarted.prootPath = filepath.Join(base, "udocker", "bin", "proot-x86_64")
	restarted.udockerScript = filepath.Join(base, "offline", "udocker", "udocker")
	argv, err := restarted.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--volume=/dev/kfd:/dev/kfd", "--volume=/dev/dri/renderD129:/dev/dri/renderD129", "--env=HIP_VISIBLE_DEVICES=0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q after restart: %s", want, joined)
		}
	}
	for _, leaked := range []string{"changed-after-recording", "NCCL_IB_DISABLE", "nvidia-smi"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("unexpected %q after restart: %s", leaked, joined)
		}
	}
}

// What is bound for a named vendor is its granted devices and nothing
// inferred from another vendor's variables: PPU sets CUDA_VISIBLE_DEVICES
// and is not given the NVIDIA nodes, and a stray ASCEND_VISIBLE_DEVICES on a
// Hygon workload binds no davinci node.
func TestNamedVendorBindsNoOtherVendorsDevices(t *testing.T) {
	stubNVIDIADevices(t, map[int]string{0: "/dev/nvidia0"}, "/dev/nvidia0")
	for _, tc := range []struct {
		name string
		spec runtime.WorkloadSpec
	}{
		{"PPU", runtime.WorkloadSpec{
			Env:   map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
			Grant: runtime.DeviceGrant{Vendor: accelerator.VendorPPU, Devices: []runtime.Device{{Path: "/dev/alixpu_ppu0", Access: runtime.DeviceAccessAll}}},
		}},
		{"Hygon", runtime.WorkloadSpec{
			Env:   map[string]string{"HYGON_VISIBLE_DEVICES": "0", "ASCEND_VISIBLE_DEVICES": "0", "CUDA_VISIBLE_DEVICES": "0"},
			Grant: runtime.DeviceGrant{Vendor: accelerator.VendorHygon, Devices: []runtime.Device{{Path: "/dev/kfd", Access: runtime.DeviceAccessAll}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := mountSources(acceleratorMounts(tc.spec))
			if len(sources) != 1 || sources[0] != tc.spec.Grant.Devices[0].Path {
				t.Fatalf("bound %v, want only the granted device", sources)
			}
		})
	}
	// The same variables on a workload that names no vendor are inferred from.
	unnamed := mountSources(acceleratorMounts(runtime.WorkloadSpec{Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"}}))
	if !slices.Contains(unnamed, "/dev/nvidia0") {
		t.Fatalf("an unnamed CUDA workload bound %v, want the card's NVIDIA node", unnamed)
	}
}

// The workload's own devices are bound before the ones it was granted.
func TestOwnDevicesAreBoundBeforeGrantedOnes(t *testing.T) {
	spec := runtime.WorkloadSpec{
		DockerArgs: []string{"--device", "/dev/infiniband", "--device=/dev/own:/dev/own:rwm"},
		Grant: runtime.DeviceGrant{Vendor: accelerator.VendorHygon, Devices: []runtime.Device{
			{Path: "/dev/kfd", Access: runtime.DeviceAccessAll}, {Path: "/dev/dri/renderD128", Access: runtime.DeviceAccessAll},
		}},
	}
	got := mountSources(acceleratorMounts(spec))
	want := []string{"/dev/infiniband", "/dev/own", "/dev/kfd", "/dev/dri/renderD128"}
	if !slices.Equal(got, want) {
		t.Fatalf("bound in order %v, want %v", got, want)
	}
}

// A device among the workload's own options that its grant replaces is not
// bound: only the granted nodes of the backend's devices are.
func TestReplacedOwnDevicesAreNotBound(t *testing.T) {
	spec := runtime.WorkloadSpec{
		DockerArgs: []string{"--device", "/dev/dri/renderD128", "--device", "/dev/infiniband"},
		Grant: runtime.DeviceGrant{
			Vendor:   accelerator.VendorAMD,
			Devices:  []runtime.Device{{Path: "/dev/dri/renderD135", Access: runtime.DeviceAccessAll}},
			Replaces: runtime.Replaced{Devices: []string{"/dev/dri/renderD128", "/dev/dri/renderD135"}},
		},
	}
	got := mountSources(acceleratorMounts(spec))
	if want := []string{"/dev/infiniband", "/dev/dri/renderD135"}; !slices.Equal(got, want) {
		t.Fatalf("bound %v, want %v", got, want)
	}
}

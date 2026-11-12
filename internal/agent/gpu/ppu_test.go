package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// TestVerifyPPUDeviceNodeSeesTheLiveHostDev: a privileged container's own
// /dev is a copy made at container start, so a device node created
// afterward is missing there; verifyPPUDeviceNode must stat the host's
// live /dev through hostRoot when CLIs run there.
func TestVerifyPPUDeviceNodeSeesTheLiveHostDev(t *testing.T) {
	root := withHostRoot(t)
	const path = "/dev/alixpu0"
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPPUDeviceNode(0, path, 0); err != nil {
		t.Fatalf("verifyPPUDeviceNode(present only under hostRoot) = %v", err)
	}
	cliOnHost.Store(false)
	if err := verifyPPUDeviceNode(0, path, 0); err == nil {
		t.Fatal("verifyPPUDeviceNode off the host found a node that only exists under hostRoot")
	}
}

func TestParsePPUFrameworkVersion(t *testing.T) {
	if got := parsePPUFrameworkVersion("HGGC Runtime Version : v13.0.1\n"); got != "13.0.1" {
		t.Fatalf("runtime version = %q", got)
	}
	if got := parsePPUFrameworkVersion("HGGC Version : N/A\nHGGC Runtime Version : N/A\n"); got != "" {
		t.Fatalf("N/A runtime version = %q", got)
	}
}

func TestParseLdconfigLibraryPath(t *testing.T) {
	output := `123 libs found in cache /etc/ld.so.cache
	libuki.so (libc6,x86-64) => /usr/lib64/libuki.so
	libhgml.so (libc6,x86-64) => /usr/lib64/libhgml.so
`
	if got := parseLdconfigLibraryPath(output, "libhgml.so"); got != "/usr/lib64/libhgml.so" {
		t.Fatalf("libhgml path = %q", got)
	}
	if got := parseLdconfigLibraryPath(output, "missing.so"); got != "" {
		t.Fatalf("missing path = %q", got)
	}
}

const ppuInfoSample = `0, PPU-ZW810E, Beethoven, GPU-019e1215-4131-0310-0000-0000e090e071, 1, 00000001:C9:00.0, 98304, 3, 0, 31, 82.75, 1.6.1-f3016a, 0, 0, 0, No, 0, No, No, [N/A], [N/A], [N/A]
7, PPU-ZW810E, Beethoven, GPU-011ef111-c280-0918-0000-000020462378, 9, 00000001:A5:00.0, 98304, 3, 0, 33, 79.50, 1.6.1-f3016a, 0, 0, 0, No, 0, No, No, [N/A], [N/A], [N/A]
`

func TestParsePPUInfoRealV118Sample(t *testing.T) {
	gpus := parsePPUInfo(ppuInfoSample)
	if len(gpus) != 2 {
		t.Fatalf("GPUs = %d, want 2: %+v", len(gpus), gpus)
	}
	g := gpus[0]
	if g.Index != 0 || g.UUID != "GPU-019e1215-4131-0310-0000-0000e090e071" {
		t.Fatalf("identity = %d/%q", g.Index, g.UUID)
	}
	if g.Name != "PPU-ZW810E" || g.Vendor != "ppu" || g.ArchID != "ZW810E" {
		t.Fatalf("model fields = %+v", g)
	}
	if g.MemoryTotalBytes != 96*(1<<30) || g.PCIeBDF != "0001:c9:00.0" {
		t.Fatalf("memory/BDF = %d/%q", g.MemoryTotalBytes, g.PCIeBDF)
	}
	if !g.Healthy || g.HealthStatus != "OK" || g.DriverVersion != "1.6.1-f3016a" {
		t.Fatalf("health/driver = %+v", g)
	}
	// After a physical card is assigned to VFIO, PPU index 7 maps to minor 9.
	// Parsing must keep the CLI index and stable UUID independent.
	if gpus[1].Index != 7 || gpus[1].UUID != "GPU-011ef111-c280-0918-0000-000020462378" {
		t.Fatalf("renumbered card = %+v", gpus[1])
	}
}

func TestParsePPUHealthLevels(t *testing.T) {
	const prefix = "15, PPU-ZW810E, Beethoven, GPU-x, 8, 00000000:C6:00.0, 98304, 3, 0, 29, 57.57, 1.6.1-f3016a, "
	cases := []struct {
		name, health        string
		healthy             bool
		status, code, proof string
	}{
		{"clean", "0, 0, 0, No, 0, No, No, [N/A], [N/A], [N/A]", true, "OK", "", ""},
		// Only the fields that contributed are evidence; the rest of the
		// state is what the recheck command shows.
		{"pending remap stays schedulable", "0, 0, 0, No, 0, Yes, No, [N/A], [N/A], [N/A]", true, "Warning", "memory_repair_pending",
			"remapped_rows.pending=Yes"},
		{"pending page retirement stays schedulable", "0, 0, 0, Yes, 0, No, No, [N/A], [N/A], [N/A]", true, "Warning", "memory_repair_pending",
			"retired_pages.pending=Yes"},
		// Lifetime counters of handled errors are history, not a fault: a
		// reset never clears them, so treating them as a fault would retire
		// the card for good.
		{"handled history stays healthy", "0, 2, 1, No, 3, No, No, [N/A], [N/A], [N/A]", true, "OK", "memory_error_history",
			"ecc.errors.uncorrected.aggregate.dram=2\nretired_pages.double_bit.count=1\nremapped_rows.uncorrectable=3"},
		{"errors since reset stop scheduling", "4, 4, 0, No, 0, Yes, No, [N/A], [N/A], [N/A]", false, "Error", "memory_uncorrectable_errors",
			"ecc.errors.uncorrected.volatile.dram=4\necc.errors.uncorrected.aggregate.dram=4\nremapped_rows.pending=Yes"},
		{"failed replacement stops scheduling", "0, 0, 0, No, 0, No, Yes, [N/A], [N/A], [N/A]", false, "Error", "memory_remap_failed",
			"remapped_rows.failure=Yes"},
		{"reset event", "0, 0, 0, No, 0, No, No, 17, [N/A], [N/A]", false, "Error", "reset_required",
			"xid.correctable.ppu_reset=17"},
		{"most severe wins and every contributing field is kept", "2, 0, 1, Yes, 3, Yes, Yes, 17, [N/A], 79", false, "Error", "power_cycle_required",
			"ecc.errors.uncorrected.volatile.dram=2\nretired_pages.double_bit.count=1\nretired_pages.pending=Yes\nremapped_rows.uncorrectable=3\nremapped_rows.pending=Yes\nremapped_rows.failure=Yes\nxid.correctable.ppu_reset=17\nxid.correctable.cold_reboot=79"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpus := parsePPUInfo(prefix + tc.health)
			if len(gpus) != 1 {
				t.Fatalf("gpus = %+v", gpus)
			}
			g := gpus[0]
			if g.Healthy != tc.healthy || g.HealthStatus != tc.status || g.HealthCode != tc.code || g.HealthMessage != tc.proof {
				t.Fatalf("health = %v/%q/%q/%q, want %v/%q/%q/%q", g.Healthy, g.HealthStatus, g.HealthCode, g.HealthMessage,
					tc.healthy, tc.status, tc.code, tc.proof)
			}
		})
	}
}

func TestParsePPUMetrics(t *testing.T) {
	metrics := parsePPUMetrics("7, GPU-ppu-7, 3, 98304, 42, 33, 79.50\n")
	if len(metrics) != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
	m := metrics[0]
	if m.Index != 7 || m.UUID != "GPU-ppu-7" || m.Vendor != "ppu" {
		t.Fatalf("labels = %+v", m)
	}
	if m.Values["gpuworks_gpu_memory_total_bytes"] != 96*(1<<30) || m.Values["gpuworks_gpu_utilization"] != 42 || m.Values["gpuworks_gpu_power_watts"] != 79.5 {
		t.Fatalf("values = %+v", m.Values)
	}
}

func TestParsePPUProcInfo(t *testing.T) {
	fields := parsePPUProcInfo(`Model: PPU-ZW810E
PPU UUID: GPU-011ef111-c280-0918-0000-000020462378
Bus Location: 0001:a5:00.0
Device Minor: 9
`)
	if fields["uuid"] != "GPU-011ef111-c280-0918-0000-000020462378" || fields["minor"] != "9" {
		t.Fatalf("proc info = %#v", fields)
	}
	if got := ppuDevicePath(9); got != "/dev/alixpu_ppu8" {
		t.Fatalf("minor 9 device path = %q", got)
	}
}

func TestPPURuntimeInjectionFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	inj := (&PPUProber{}).RuntimeInjection([]int{7})
	if inj.UnsupportedReason == "" || len(inj.DockerArgs) != 0 || len(inj.Env) != 0 {
		t.Fatalf("injection = %+v", inj)
	}
}

func TestPPURuntimeInjectionSelectsDevicesInRequestedOrder(t *testing.T) {
	const first = "GPU-011ef111-c280-0918-0000-000020462378"
	const second = "GPU-019ec20c-0692-0214-0000-0000c0e5b553"
	p := &PPUProber{lastSeen: map[int]ppuIdentity{8: {uuid: first}, 9: {uuid: second}}}
	rows := "8, " + first + "\n9, " + second
	resolve := func(index int, id string) (string, error) {
		if (index == 8 && id == first) || (index == 9 && id == second) {
			return fmt.Sprintf("/dev/alixpu_ppu%d", index+1), nil
		}
		return "", fmt.Errorf("unknown identity")
	}
	for _, tc := range []struct {
		name                     string
		indexes                  []int
		inventory, want, failure string
	}{
		{"nonzero device", []int{8}, rows, "0", ""},
		{"reverse order", []int{9, 8}, rows, "1,0", ""},
		{"absent device", []int{0}, rows, "", "not present"},
		{"renumbered", []int{8}, "8, " + second, "", "changed"},
		{"invalid UUID", []int{8}, "8, GPU-bad", "", "invalid"},
		{"duplicate device", []int{8, 8}, rows, "", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := p.runtimeInjection(tc.indexes, tc.inventory, resolve, "docker")
			if tc.failure != "" {
				if !strings.Contains(inj.UnsupportedReason, tc.failure) || len(inj.Env) != 0 || len(inj.DockerArgs) != 0 {
					t.Fatalf("injection=%+v", inj)
				}
				return
			}
			if inj.UnsupportedReason != "" || inj.Env[accelerator.EnvCUDAVisibleDevices] != tc.want || inj.Env[accelerator.EnvNVIDIAVisibleDevices] != "void" {
				t.Fatalf("injection=%+v", inj)
			}
			identities := strings.Split(inj.Env[accelerator.PPUEnvDeviceUUIDs], ",")
			if len(identities) != len(tc.indexes) {
				t.Fatalf("missing verified identities: %+v", inj)
			}
			for i, index := range tc.indexes {
				if identities[i] != p.lastSeen[index].uuid {
					t.Fatalf("identity order differs from selected devices: %+v", inj)
				}
				path := fmt.Sprintf("/dev/alixpu_ppu%d", index+1)
				if !strings.Contains(strings.Join(inj.DockerArgs, " "), path+":"+path+":rwm") {
					t.Fatalf("wrong device mapping: %+v", inj)
				}
			}
		})
	}
	unknown := p.runtimeInjection([]int{8}, rows, resolve, "unknown")
	if unknown.UnsupportedReason == "" || len(unknown.Env) != 0 || len(unknown.DockerArgs) != 0 {
		t.Fatalf("unsupported runtime injection=%+v", unknown)
	}

	inj := p.runtimeInjection([]int{8}, rows, func(int, string) (string, error) { return "", fmt.Errorf("device UUID/minor mismatch") }, "docker")
	if !strings.Contains(inj.UnsupportedReason, "mismatch") || len(inj.Env) != 0 {
		t.Fatalf("injection=%+v", inj)
	}
}

func TestPPURuntimeInjectionUsesDriverInventory(t *testing.T) {
	const first = "GPU-00000000-0000-0000-0000-000000000008"
	const second = "GPU-00000000-0000-0000-0000-000000000012"
	p := &PPUProber{lastSeen: map[int]ppuIdentity{8: {uuid: first}, 12: {uuid: second}}}
	rows := "8, " + first + "\n12, " + second
	resolve := func(index int, _ string) (string, error) {
		return fmt.Sprintf("/dev/alixpu_ppu%d", index+1), nil
	}
	for _, tc := range []struct {
		driver  string
		indexes []int
		want    string
	}{
		{"", []int{12, 8}, second + "," + first},
		{"docker", []int{8}, "0"},
		{"docker", []int{8, 12}, "0,1"},
		{"docker", []int{12, 8}, "1,0"},
		{"nerdctl", []int{12, 8}, "1,0"},
		// The kube runtime's device plugin hands the pod the selected
		// devices only, renumbered like an OCI runtime's.
		{"kube", []int{8}, "0"},
		{"kube", []int{12, 8}, "1,0"},
		{"udocker", []int{8}, "8"},
		{"udocker", []int{12, 8}, "12,8"},
	} {
		t.Run(tc.driver+"/"+tc.want, func(t *testing.T) {
			inj := p.runtimeInjection(tc.indexes, rows, resolve, tc.driver)
			if inj.UnsupportedReason != "" || inj.Env[accelerator.EnvCUDAVisibleDevices] != tc.want {
				t.Fatalf("injection=%+v, want visible=%q", inj, tc.want)
			}
			identities := strings.Split(inj.Env[accelerator.PPUEnvDeviceUUIDs], ",")
			if len(identities) != len(tc.indexes) {
				t.Fatalf("missing verified identities: %+v", inj)
			}
			for i, index := range tc.indexes {
				if identities[i] != p.lastSeen[index].uuid {
					t.Fatalf("identity must be independent of the runtime view: %+v", inj)
				}
				path := fmt.Sprintf("/dev/alixpu_ppu%d", index+1)
				if !strings.Contains(strings.Join(inj.DockerArgs, " "), path+":"+path+":rwm") {
					t.Fatalf("wrong device mapping: %+v", inj)
				}
			}
		})
	}
	t.Setenv("PATH", t.TempDir())
	inj := p.RuntimeInjectionForDriver(nil, "docker")
	if inj.UnsupportedReason != "" || len(inj.Env) != 0 || len(inj.DockerArgs) != 0 {
		t.Fatalf("empty selection must not query or inject devices: %+v", inj)
	}
}

type availabilityProber struct {
	vendor    string
	available bool
}

func (p availabilityProber) Vendor() string                                 { return p.vendor }
func (p availabilityProber) Available() bool                                { return p.available }
func (p availabilityProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (p availabilityProber) DriverVersion(context.Context) string           { return "" }
func (p availabilityProber) Topology(context.Context) string                { return "" }
func (p availabilityProber) Metrics(context.Context) ([]Metric, error)      { return nil, nil }
func (p availabilityProber) RuntimeInjection([]int) RuntimeInjection        { return RuntimeInjection{} }

// The PPU SDK puts a CUDA-compat nvidia-smi (/usr/local/PPU_SDK/CUDA_SDK/bin)
// ahead of ppu-smi on PATH, and it answers the inventory query with the PPU
// cards; an ACS PPU pod (2026-10-15) was registered as an NVIDIA node through
// it. Both CLIs here answer the way that pod's did.
func TestNewPrefersPPUOverCompatNVIDIASMI(t *testing.T) {
	bin := t.TempDir()
	writeCLI := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Real compat output for unknown properties is ppu-smi's own error text.
	writeCLI("nvidia-smi", `case "$1" in
--query-gpu=index) echo 0 ;;
*) echo "Invalid combination of input arguments. Please run 'ppu-smi -h' for help."; exit 2 ;;
esac
`)
	writeCLI("ppu-smi", `case "$1" in
--query-ppu=index) echo 0 ;;
*) exit 2 ;;
esac
`)
	t.Setenv("PATH", bin)

	if !(&SMIProber{}).Available() {
		t.Fatal("fixture: the compat nvidia-smi must answer, as it does on the pod")
	}
	if got := New().Vendor(); got != "ppu" {
		t.Fatalf("New() vendor = %q, want ppu", got)
	}
}

func TestFirstAvailableSkipsStaleNVIDIA(t *testing.T) {
	p := firstAvailable([]Prober{
		availabilityProber{vendor: "nvidia", available: false},
		availabilityProber{vendor: "ppu", available: true},
	})
	if p == nil || p.Vendor() != "ppu" {
		t.Fatalf("selected = %#v", p)
	}
}

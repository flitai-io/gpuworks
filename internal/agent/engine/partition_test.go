package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestApplyPartitionRejectsUnknownBackend(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	ok, reason := e.applyPartition(context.Background(), &agentruntime.WorkloadSpec{Env: map[string]string{}}, proto.InstanceDesiredState{UUID: "unknown", VRAMLimitBytes: 1 << 30, PartitionBackend: "made-up"})
	if ok || !strings.Contains(reason, "unsupported GPU partition backend") {
		t.Fatalf("result = %v/%q", ok, reason)
	}
}

func TestAscendPartitionGeneratesPrivateConfig(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{stateDir: root}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{"LD_LIBRARY_PATH": "/image/cann/lib64:/image/hccl/lib64"}}
	ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: "ascend/instance", GPUIndexes: []int{4}, VRAMLimitBytes: 8 << 30, SMLimitPct: 25, PartitionBackend: accelerator.BackendAscendLogical})
	if !ok {
		t.Fatalf("apply failed: %s", reason)
	}
	if ws.Env["ENPU_DSMI_HOOK"] != "1" || mountAt(ws, accelerator.AscendConfigTarget) == nil ||
		mountAt(ws, accelerator.AscendPreloadTarget) == nil || mountAt(ws, accelerator.AscendLibraryTarget) == nil {
		t.Fatalf("env/mounts = %+v/%+v", ws.Env, ws.Mounts)
	}
	config, err := os.ReadFile(filepath.Join(root, "dgpu-runtime", "instances", "ascend_instance", "npu_info.config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"physical-npu-id=4", "aicore-quota=25", "memory-quota=8192", "scheduling-policy=2"} {
		if !strings.Contains(string(config), want) {
			t.Fatalf("config %q missing %q", config, want)
		}
	}
	if !strings.Contains(ws.Env["LD_LIBRARY_PATH"], "/usr/local/Ascend/driver/lib64/driver") {
		t.Fatalf("LD_LIBRARY_PATH = %q", ws.Env["LD_LIBRARY_PATH"])
	}
	if !strings.HasSuffix(ws.Env["LD_LIBRARY_PATH"], ":/image/cann/lib64:/image/hccl/lib64") {
		t.Fatalf("image LD_LIBRARY_PATH was not preserved: %q", ws.Env["LD_LIBRARY_PATH"])
	}
}

type ascendPartitionTestProber struct {
	*gpu.FixedProber
	shareCalls   []int
	shareErr     error
	shareChanged bool
	hardwareID   string
}

func (p *ascendPartitionTestProber) EnsureDeviceShare(_ context.Context, index int) (bool, error) {
	p.shareCalls = append(p.shareCalls, index)
	return p.shareChanged, p.shareErr
}

func (p *ascendPartitionTestProber) PartitionHardwareID(context.Context, int) string {
	return p.hardwareID
}

func TestAscendVirtualIDLowestFreeRestartAndCardSharedLedger(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	prober := &ascendPartitionTestProber{FixedProber: &gpu.FixedProber{VendorName: "ascend", FrameworkName: "cann", FrameworkVersion: "9.0.0"}, shareChanged: true, hardwareID: "AAAA BBBB CCCC"}
	e := &Engine{stateDir: root, prober: prober}
	apply := func(uuid string) string {
		t.Helper()
		ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
		ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: uuid, GPUIndexes: []int{4}, VRAMLimitBytes: 4 << 30, PartitionBackend: accelerator.BackendAscendLogical})
		if !ok {
			t.Fatalf("apply %s: %s", uuid, reason)
		}
		if len(ws.PreparationNotes) != 1 || !strings.Contains(ws.PreparationNotes[0], "device-share auto-enabled") {
			t.Fatalf("device-share preparation notes = %v", ws.PreparationNotes)
		}
		data, err := os.ReadFile(filepath.Join(root, "dgpu-runtime", "instances", uuid, "npu_info.config"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	first := apply("first")
	second := apply("second")
	if !strings.Contains(first, "virtual-npu-id=0") || !strings.Contains(second, "virtual-npu-id=1") {
		t.Fatalf("allocated configs:\n%s\n%s", first, second)
	}
	if !strings.Contains(first, "shm-id=AAAA-BBBB-CCCC") || !strings.Contains(second, "shm-id=AAAA-BBBB-CCCC") {
		t.Fatalf("same-card ledger IDs differ:\n%s\n%s", first, second)
	}
	if restarted := apply("second"); !strings.Contains(restarted, "virtual-npu-id=1") {
		t.Fatalf("restart did not preserve id: %s", restarted)
	}
	if err := os.RemoveAll(filepath.Join(root, "dgpu-runtime", "instances", "first")); err != nil {
		t.Fatal(err)
	}
	if third := apply("third"); !strings.Contains(third, "virtual-npu-id=0") {
		t.Fatalf("lowest free hole was not reused: %s", third)
	}
	if len(prober.shareCalls) != 4 {
		t.Fatalf("device-share calls = %v", prober.shareCalls)
	}
}

func TestAscendDeviceShareFailureIsActionable(t *testing.T) {
	prober := &ascendPartitionTestProber{FixedProber: &gpu.FixedProber{VendorName: "ascend"}, shareErr: errors.New("permission denied; run: npu-smi set -t device-share -i 7 -c 0 -d 1")}
	e := &Engine{stateDir: t.TempDir(), prober: prober}
	ok, reason := e.applyPartition(context.Background(), &agentruntime.WorkloadSpec{Env: map[string]string{}}, proto.InstanceDesiredState{UUID: "x", GPUIndexes: []int{7}, VRAMLimitBytes: 1 << 30, PartitionBackend: accelerator.BackendAscendLogical})
	if ok || !strings.Contains(reason, "npu-smi set -t device-share -i 7") {
		t.Fatalf("result = %v, %q", ok, reason)
	}
}

func TestPPUPartitionUsesSafePreloadOrderAndQuota(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ppu", "linux-"+runtime.GOARCH, "hggc-13.0")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hggc_quota.so", "hgml_dlsym_hook.so"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), fakeELF(runtime.GOARCH), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{stateDir: root}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: "ppu-1", GPUIndexes: []int{7, 9}, VRAMLimitBytes: 8 << 30, SMLimitPct: 50, PartitionBackend: accelerator.BackendPPULogical})
	if !ok {
		t.Fatalf("apply failed: %s", reason)
	}
	if ws.Env["LD_PRELOAD"] != "/opt/gpuworks/partition/hgml_dlsym_hook.so:/opt/gpuworks/partition/hggc_quota.so" {
		t.Fatalf("LD_PRELOAD = %q", ws.Env["LD_PRELOAD"])
	}
	if ws.Env["HGGC_DEVICE_MEMORY_LIMIT_0"] != "8192" || ws.Env["HGGC_DEVICE_MEMORY_LIMIT_1"] != "8192" || ws.Env["HGGC_DEVICE_SM_LIMIT_0"] != "50" {
		t.Fatalf("quota env = %+v", ws.Env)
	}

	defaultWS := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	ok, reason = e.applyPartition(context.Background(), defaultWS, proto.InstanceDesiredState{UUID: "ppu-default", GPUIndexes: []int{7}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendPPULogical})
	if !ok {
		t.Fatalf("apply default SM limit failed: %s", reason)
	}
	if got := defaultWS.Env["HGGC_DEVICE_SM_LIMIT_0"]; got != "100" {
		t.Fatalf("default HGGC_DEVICE_SM_LIMIT_0 = %q, want 100", got)
	}
}

func TestAMDPartitionMountsLibvrocmAndSetsPerCardQuota(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "amd", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvrocm.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{stateDir: root}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: "amd-1", GPUIndexes: []int{0, 1}, VRAMLimitBytes: 60000 << 20, PartitionBackend: accelerator.BackendAMDLogical})
	if !ok {
		t.Fatalf("apply failed: %s", reason)
	}
	if ws.Env["LD_PRELOAD"] != "/opt/gpuworks/partition/libvrocm.so" {
		t.Fatalf("LD_PRELOAD = %q", ws.Env["LD_PRELOAD"])
	}
	if ws.Env["VROCM_DEVICE_MEMORY_LIMIT_0"] != "60000" || ws.Env["VROCM_DEVICE_MEMORY_LIMIT_1"] != "60000" {
		t.Fatalf("quota env = %+v", ws.Env)
	}
	if ws.Env["VROCM_LEDGER_PATH"] != "/dev/shm/gpuworks-amd-amd-1" {
		t.Fatalf("ledger path = %q", ws.Env["VROCM_LEDGER_PATH"])
	}
	var mounted bool
	for _, mount := range ws.Mounts {
		if mount.Target == "/opt/gpuworks/partition/libvrocm.so" {
			mounted = true
			if !mount.ReadOnly || mount.Source != filepath.Join(assetDir, "libvrocm.so") {
				t.Fatalf("libvrocm mount = %+v", mount)
			}
		}
	}
	if !mounted {
		t.Fatalf("libvrocm.so was not mounted: %+v", ws.Mounts)
	}
	// AMD has no compute/SM env: this backend is memory-only.
	for key := range ws.Env {
		if strings.Contains(key, "SM_LIMIT") || strings.Contains(key, "CU_MASK") {
			t.Fatalf("unexpected compute-limit env %q", key)
		}
	}
}

func TestAMDPartitionRequiresSelectedDevice(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	ok, reason := e.applyPartition(context.Background(), &agentruntime.WorkloadSpec{Env: map[string]string{}}, proto.InstanceDesiredState{UUID: "amd-empty", VRAMLimitBytes: 1 << 30, PartitionBackend: accelerator.BackendAMDLogical})
	if ok || !strings.Contains(reason, "at least one selected device") {
		t.Fatalf("result = %v/%q", ok, reason)
	}
}

func TestAMDPartitionLiveLimitIsUnsupported(t *testing.T) {
	injector := amdPartitionInjector{}
	changed, err := injector.applyLimit(&Engine{}, proto.InstanceDesiredState{})
	if changed || !errors.Is(err, errLiveLimitUnsupported) {
		t.Fatalf("applyLimit = %v, %v", changed, err)
	}
}

func TestCleanupPartitionStateRemovesAMDLedger(t *testing.T) {
	root := t.TempDir()
	e := &Engine{stateDir: root}
	uuid := "amd-cleanup-" + strings.ReplaceAll(t.Name(), "/", "-")
	instanceDir := filepath.Join(root, "dgpu-runtime", "instances", sanitizeID(uuid))
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := amdLedgerPath(uuid)
	ledgerCreated := false
	if _, err := os.Stat("/dev/shm"); err == nil {
		if err := os.WriteFile(ledger, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		ledgerCreated = true
	}
	t.Cleanup(func() {
		if ledgerCreated {
			_ = os.Remove(ledger)
		}
	})

	e.cleanupPartitionState(uuid)
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("instance directory remains: %v", err)
	}
	if ledgerCreated {
		if _, err := os.Stat(ledger); !os.IsNotExist(err) {
			t.Fatalf("AMD ledger remains: %v", err)
		}
	}
}

type probeLibraryPPUProber struct{ *gpu.FixedProber }

func (p *probeLibraryPPUProber) RuntimeInjection(indexes []int) gpu.RuntimeInjection {
	if len(indexes) != 1 || indexes[0] != 7 {
		return gpu.RuntimeInjection{UnsupportedReason: "device absent"}
	}
	return gpu.RuntimeInjection{Env: map[string]string{accelerator.EnvCUDAVisibleDevices: "0", accelerator.PPUEnvDeviceUUIDs: "GPU-011ef111-c280-0918-0000-000020462378"}}
}

func (p *probeLibraryPPUProber) PartitionProbeLibraries(context.Context) ([]gpu.ProbeLibrary, error) {
	return []gpu.ProbeLibrary{{Source: "/host/libhgml.so", Name: "libhgml.so"}, {Source: "/host/libuki.so", Name: "libuki.so"}}, nil
}

type imageEnvDriver struct {
	agentruntime.Driver
	env map[string]string
}

func (d imageEnvDriver) InspectImage(context.Context, string) (agentruntime.ImageInspection, error) {
	return agentruntime.ImageInspection{Env: d.env}, nil
}

func TestSeedImageLibraryPathPreservesExplicitEnvAndHydratesImageDefault(t *testing.T) {
	driver := imageEnvDriver{env: map[string]string{"LD_LIBRARY_PATH": "/image/cann:/image/hccl"}}
	e := &Engine{driver: driver}
	workload := &agentruntime.WorkloadSpec{Image: "cann-image", Env: map[string]string{}}
	e.seedImageLibraryPath(context.Background(), workload)
	if got := workload.Env["LD_LIBRARY_PATH"]; got != "/image/cann:/image/hccl" {
		t.Fatalf("hydrated LD_LIBRARY_PATH = %q", got)
	}
	workload.Env["LD_LIBRARY_PATH"] = "/deployment/override"
	e.seedImageLibraryPath(context.Background(), workload)
	if got := workload.Env["LD_LIBRARY_PATH"]; got != "/deployment/override" {
		t.Fatalf("explicit LD_LIBRARY_PATH was replaced: %q", got)
	}
}

func TestPPUProbeWorkloadMountsDriverLibrariesWithoutHostHGGCVersion(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ppu", "linux-"+runtime.GOARCH, "hggc-13.0")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hggc_quota.so", "hgml_dlsym_hook.so"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), fakeELF(runtime.GOARCH), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prober := &probeLibraryPPUProber{FixedProber: &gpu.FixedProber{VendorName: "ppu", FrameworkName: "hggc", FrameworkVersion: ""}}
	e := &Engine{stateDir: root, prober: prober, driver: &headlessTestDriver{name: "docker"}}
	workload, err := e.BuildPartitionProbeWorkload(context.Background(), accelerator.BackendPPULogical, "gpuworks/partition-probe:v1", 7, 4<<30, "ppu-probe")
	if err != nil {
		t.Fatal(err)
	}
	if workload.Env["GPUWORKS_PARTITION_PROBE_UUID"] != "GPU-011ef111-c280-0918-0000-000020462378" {
		t.Fatalf("missing probe identity: %+v", workload.Env)
	}
	wantMounts := map[string]string{
		"/opt/gpuworks/probe-libs/libhgml.so": "/host/libhgml.so",
		"/opt/gpuworks/probe-libs/libuki.so":  "/host/libuki.so",
	}
	for _, mount := range workload.Mounts {
		if wantSource, ok := wantMounts[mount.Target]; ok {
			if mount.Source != wantSource || !mount.ReadOnly {
				t.Fatalf("probe library mount = %+v", mount)
			}
			delete(wantMounts, mount.Target)
		}
	}
	probeLibraryPath := workload.Env["LD_LIBRARY_PATH"]
	if len(wantMounts) != 0 || probeLibraryPath != "/opt/gpuworks/probe-libs" && !strings.HasPrefix(probeLibraryPath, "/opt/gpuworks/probe-libs:") {
		t.Fatalf("probe runtime mounts=%v env=%v", wantMounts, workload.Env)
	}
}

func TestCleanupPartitionStateRemovesInstanceDirectoryAndPPULedger(t *testing.T) {
	root := t.TempDir()
	e := &Engine{stateDir: root}
	uuid := "cleanup-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	instanceDir := filepath.Join(root, "dgpu-runtime", "instances", sanitizeID(uuid))
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join("/dev/shm", "gpuworks-ppu-"+sanitizeID(uuid))
	ledgerCreated := false
	if _, err := os.Stat("/dev/shm"); err == nil {
		if err := os.WriteFile(ledger, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		ledgerCreated = true
	}
	t.Cleanup(func() {
		if ledgerCreated {
			_ = os.Remove(ledger)
		}
	})

	e.cleanupPartitionState(uuid)
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("instance directory remains: %v", err)
	}
	if ledgerCreated {
		if _, err := os.Stat(ledger); !os.IsNotExist(err) {
			t.Fatalf("PPU ledger remains: %v", err)
		}
	}
}

func TestObservedPartitionAssetsDoesNotRequireContainerDriver(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	prober := &gpu.FixedProber{VendorName: "ascend", FrameworkName: "cann", FrameworkVersion: "9.0.0"}
	reports := ObservedPartitionAssets(context.Background(), root, prober)
	if len(reports) != 1 || reports[0].Backend != accelerator.BackendAscendLogical || len(reports[0].Files) != 1 || reports[0].Files[0] != "libvruntime.so" {
		t.Fatalf("reports = %+v", reports)
	}
}

func TestAssetSelectionIgnoresCANNVersion(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	prober := &gpu.FixedProber{VendorName: "ascend", FrameworkName: "cann", FrameworkVersion: "9.0.0"}
	e := &Engine{stateDir: root, prober: prober}
	backend, _ := accelerator.Lookup(accelerator.BackendAscendLogical)
	asset, err := e.scanPartitionAssets(backend, "ascend", exactAscendAssetDir)
	if err != nil || asset.Root != assetDir {
		t.Fatalf("asset = %+v, %v; want %s", asset, err, assetDir)
	}
}

func TestAssetSelectionRejectsLegacyVersionDirectory(t *testing.T) {
	root := t.TempDir()
	legacyDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH, "cann-9.0.0")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{stateDir: root, prober: &gpu.FixedProber{VendorName: "ascend", FrameworkName: "cann", FrameworkVersion: "9.0.0"}}
	backend, _ := accelerator.Lookup(accelerator.BackendAscendLogical)
	if asset, err := e.scanPartitionAssets(backend, "ascend", exactAscendAssetDir); err == nil {
		t.Fatalf("legacy version directory was selected: %+v", asset)
	}
}

// Every registry backend must carry an agent-side executor, and no executor
// may exist for a backend the registry does not know.
func TestPartitionInjectorsCoverRegistry(t *testing.T) {
	for _, backend := range accelerator.All() {
		if _, ok := partitionInjectors[backend.ID()]; !ok {
			t.Errorf("backend %q has no partition injector", backend.ID())
		}
	}
	for id := range partitionInjectors {
		if _, ok := accelerator.Lookup(id); !ok {
			t.Errorf("partition injector registered for unknown backend %q", id)
		}
	}
}

func fakeELF(goarch string) []byte {
	b := make([]byte, 64)
	b[0], b[1], b[2], b[3], b[4], b[5] = 0x7f, 'E', 'L', 'F', 2, 1
	if goarch == "arm64" {
		b[18], b[19] = 0xb7, 0
	} else {
		b[18], b[19] = 0x3e, 0
	}
	return b
}

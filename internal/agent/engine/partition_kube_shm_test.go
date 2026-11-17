package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// TestAMDPartitionOnKubeMountsHostShmLedger covers the kube-only path: the
// agent's own /dev/shm is its pod's private tmpfs, not the host's, so the
// ledger the docker/nerdctl/udocker/runveil paths reach through --ipc=host
// has to be pre-created on the host (through hostRoot, a stand-in for
// kube.HostRoot/"/proc/1/root") and explicitly mounted into the instance
// pod. The env var and the ledger's own path are unchanged from the
// non-kube case (amdLedgerPath): only how the path reaches the container
// differs.
func TestAMDPartitionOnKubeMountsHostShmLedger(t *testing.T) {
	root, hostRoot := t.TempDir(), t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "amd", "linux-"+runtime.GOARCH)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvrocm.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{stateDir: root, hostRoot: hostRoot}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: "amd-kube-1", GPUIndexes: []int{0}, VRAMLimitBytes: 60000 << 20, PartitionBackend: accelerator.BackendAMDLogical})
	if !ok {
		t.Fatalf("apply failed: %s", reason)
	}
	wantLedger := "/dev/shm/gpuworks-amd-amd-kube-1"
	if ws.Env["VROCM_LEDGER_PATH"] != wantLedger {
		t.Fatalf("ledger env = %q, want %q (unchanged from the non-kube path)", ws.Env["VROCM_LEDGER_PATH"], wantLedger)
	}
	var ledgerMount *agentruntime.Mount
	for i := range ws.Mounts {
		if ws.Mounts[i].Target == wantLedger {
			ledgerMount = &ws.Mounts[i]
		}
	}
	if ledgerMount == nil {
		t.Fatalf("ledger was not mounted into the pod: %+v", ws.Mounts)
	}
	if ledgerMount.Source != wantLedger || ledgerMount.ReadOnly {
		t.Fatalf("ledger mount = %+v, want a writable hostPath at the same path", ledgerMount)
	}
	hostFile := filepath.Join(hostRoot, wantLedger)
	info, err := os.Stat(hostFile)
	if err != nil {
		t.Fatalf("ledger placeholder was not created on the host: %v", err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("ledger file mode = %v, want 0666 (not 0777: no execute bit is needed)", info.Mode().Perm())
	}
}

// TestAMDPartitionOnKubeCleanupRemovesHostShmLedger mirrors
// TestCleanupPartitionStateRemovesAMDLedger for the kube path: the ledger
// lives under hostRoot, not the agent's own /dev/shm.
func TestAMDPartitionOnKubeCleanupRemovesHostShmLedger(t *testing.T) {
	root, hostRoot := t.TempDir(), t.TempDir()
	e := &Engine{stateDir: root, hostRoot: hostRoot}
	uuid := "amd-kube-cleanup"
	ledger := filepath.Join(hostRoot, amdLedgerPath(uuid))
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte("test"), 0o666); err != nil {
		t.Fatal(err)
	}
	e.cleanupPartitionState(uuid)
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Fatalf("host /dev/shm ledger remains: %v", err)
	}
}

// TestPPUPartitionOnKubeMountsHostShmLedger is
// TestAMDPartitionOnKubeMountsHostShmLedger for PPU.
func TestPPUPartitionOnKubeMountsHostShmLedger(t *testing.T) {
	root, hostRoot := t.TempDir(), t.TempDir()
	assetDir := filepath.Join(root, "dgpu-runtime", "ppu", "linux-"+runtime.GOARCH, "hggc-13.0")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hggc_quota.so", "hgml_dlsym_hook.so"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), fakeELF(runtime.GOARCH), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{stateDir: root, hostRoot: hostRoot}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), ws, proto.InstanceDesiredState{UUID: "ppu-kube-1", GPUIndexes: []int{7}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendPPULogical})
	if !ok {
		t.Fatalf("apply failed: %s", reason)
	}
	wantLedger := "/dev/shm/gpuworks-ppu-ppu-kube-1"
	if ws.Env["HGGC_LEDGER_PATH"] != wantLedger {
		t.Fatalf("ledger env = %q, want %q (unchanged from the non-kube path)", ws.Env["HGGC_LEDGER_PATH"], wantLedger)
	}
	var ledgerMount *agentruntime.Mount
	for i := range ws.Mounts {
		if ws.Mounts[i].Target == wantLedger {
			ledgerMount = &ws.Mounts[i]
		}
	}
	if ledgerMount == nil {
		t.Fatalf("ledger was not mounted into the pod: %+v", ws.Mounts)
	}
	if ledgerMount.Source != wantLedger || ledgerMount.ReadOnly {
		t.Fatalf("ledger mount = %+v, want a writable hostPath at the same path", ledgerMount)
	}
	info, err := os.Stat(filepath.Join(hostRoot, wantLedger))
	if err != nil {
		t.Fatalf("ledger placeholder was not created on the host: %v", err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("ledger file mode = %v, want 0666", info.Mode().Perm())
	}
}

// TestPPUPartitionOnKubeCleanupRemovesHostShmLedger mirrors
// TestAMDPartitionOnKubeCleanupRemovesHostShmLedger for PPU.
func TestPPUPartitionOnKubeCleanupRemovesHostShmLedger(t *testing.T) {
	root, hostRoot := t.TempDir(), t.TempDir()
	e := &Engine{stateDir: root, hostRoot: hostRoot}
	uuid := "ppu-kube-cleanup"
	ledger := filepath.Join(hostRoot, ppuLedgerPath(uuid))
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte("test"), 0o666); err != nil {
		t.Fatal(err)
	}
	e.cleanupPartitionState(uuid)
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Fatalf("host /dev/shm ledger remains: %v", err)
	}
}

// TestHostRootForOnlyKube asserts hostRootFor's sole trigger is the kube
// driver name, matching device_injection.go's existing convention of
// comparing driver.Name() against string literals rather than importing a
// concrete driver package.
func TestHostRootForOnlyKube(t *testing.T) {
	for _, tc := range []struct {
		driver agentruntime.Driver
		want   string
	}{
		{nil, ""},
		{fakeNamedDriver{name: "docker"}, ""},
		{fakeNamedDriver{name: "nerdctl"}, ""},
		{fakeNamedDriver{name: "udocker"}, ""},
		{fakeNamedDriver{name: "kube"}, "/proc/1/root"},
	} {
		if got := hostRootFor(tc.driver); got != tc.want {
			t.Errorf("hostRootFor(%v) = %q, want %q", tc.driver, got, tc.want)
		}
	}
}

// fakeNamedDriver is a minimal runtime.Driver stub naming itself only, for
// hostRootFor's driver.Name() switch.
type fakeNamedDriver struct {
	agentruntime.Driver
	name string
}

func (d fakeNamedDriver) Name() string { return d.name }

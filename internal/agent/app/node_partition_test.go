package app

import (
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestHygonReportsPartitionFactsWithoutRuntime(t *testing.T) {
	if !hasPartitionBackendForVendor("hygon") {
		t.Fatal("Hygon backend was not recognized by vendor")
	}
	n := newNodeRuntime(slog.Default(), nil, t.TempDir(), "none", &gpu.HygonProber{}, nil)
	if n.partitionProbe == nil {
		t.Fatal("missing runtime must still produce explicit Hygon probe results")
	}
	if reports := n.PartitionAssets(t.Context()); reports == nil {
		t.Fatal("Hygon asset observations must be an explicit empty list")
	}
}

func TestPartitionProbeRefreshesWhenObservedAssetsChange(t *testing.T) {
	n := &NodeRuntime{}
	initial := []proto.AssetReport{
		{Backend: "ppu-dgpu", Root: "/assets/ppu", Files: []string{"hgml_dlsym_hook.so", "hggc_quota.so"}},
		{Backend: "ascend-dgpu", Root: "/assets/ascend", Files: []string{"libvruntime.so"}},
	}
	if n.partitionProbeNeedsRefresh(initial, nil) {
		t.Fatal("first observed snapshot must use the runner's initial Results path")
	}
	reordered := []proto.AssetReport{
		{Backend: "ascend-dgpu", Root: "/assets/ascend", Files: []string{"libvruntime.so"}},
		{Backend: "ppu-dgpu", Root: "/assets/ppu", Files: []string{"hggc_quota.so", "hgml_dlsym_hook.so"}},
	}
	if n.partitionProbeNeedsRefresh(reordered, nil) {
		t.Fatal("report or file ordering must not look like an asset change")
	}
	changed := append([]proto.AssetReport(nil), reordered...)
	changed[0].Files = append(changed[0].Files, "new-runtime-sidecar.so")
	if !n.partitionProbeNeedsRefresh(changed, nil) {
		t.Fatal("adding an observed asset must refresh partition probes")
	}
	if n.partitionProbeNeedsRefresh(changed, nil) {
		t.Fatal("an unchanged post-refresh snapshot must remain cached")
	}
	if !n.partitionProbeNeedsRefresh(nil, nil) {
		t.Fatal("removing all observed assets must refresh partition probes")
	}
}

func TestPartitionProbeRefreshesWhenGPUChanges(t *testing.T) {
	n := &NodeRuntime{}
	assets := []proto.AssetReport{{Backend: "ascend-dgpu", Files: []string{"libvruntime.so"}}}
	gpus := []proto.GPUInfo{{Index: 0, Vendor: "ascend", DriverVersion: "1", Healthy: true}}
	for range 2 {
		if n.partitionProbeNeedsRefresh(assets, gpus) {
			t.Fatal("unchanged probe inputs should be cached")
		}
	}
	gpus[0].DriverVersion = "2"
	if !n.partitionProbeNeedsRefresh(assets, gpus) {
		t.Fatal("driver version change must invalidate the probe")
	}
	gpus = append(gpus, proto.GPUInfo{Index: 3, Vendor: "ascend", Healthy: true})
	if !n.partitionProbeNeedsRefresh(assets, gpus) {
		t.Fatal("new GPU must receive its own probe")
	}
	if partitionProbeFingerprint(assets, gpus, "docker") == partitionProbeFingerprint(assets, gpus, "nerdctl") {
		t.Fatal("runtime change must invalidate the probe")
	}
	// What changes from one minute's report to the next without changing
	// the cards does not rerun a probe container on every card.
	gpus[0].HealthCode, gpus[0].HealthMessage = "ecc_pending", "memory repair pending"
	gpus[0].ForeignProcesses = []proto.CardProcess{{PID: 42, Name: "python3"}}
	if n.partitionProbeNeedsRefresh(assets, gpus) {
		t.Fatal("a health finding or a foreign process must not invalidate the probe")
	}
}

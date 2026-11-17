package cloud

import (
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestSharedGPUStaysDisabledUntilLastConsumerStops(t *testing.T) {
	for _, protocol := range []string{"gpu-allocation", "pci", "gpu-pci-state"} {
		t.Run(protocol, func(t *testing.T) {
			testSharedGPUStaysDisabledUntilLastConsumerStops(t, protocol)
		})
	}
}

func testSharedGPUStaysDisabledUntilLastConsumerStops(t *testing.T, protocol string) {
	t.Helper()
	st, svc, fixture := newCloudRuntimeFixtureForGPUProtocol(t, protocol)
	fixture.description.Store("operator note")
	for _, instance := range []string{"first", "second"} {
		if err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: instance, GPUIndexes: []int{0}}); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.writes.Load() != 1 || !fixture.gpu.Load() {
		t.Fatal("sharing should reuse the existing physical reservation")
	}
	if got := fixture.description.Load().(string); got != "operator note\n被gpuworks-192.0.2.100使用中" {
		t.Fatalf("shared GPU description = %q", got)
	}
	stop := provider.InferenceNodeStopRequest{NodeUUID: "cloud-node", InstanceUUID: "first", State: "stopped"}
	if err := svc.AfterInstanceTerminal(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	leases, err := cloudstore.New(st.DB()).CloudResourceLeasesForInstance("second")
	if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved || fixture.writes.Load() != 1 || !fixture.gpu.Load() {
		t.Fatalf("first stop released another consumer's GPU: leases=%+v err=%v", leases, err)
	}
	if got := fixture.description.Load().(string); got != "operator note\n被gpuworks-192.0.2.100使用中" {
		t.Fatalf("first consumer removed shared ownership: %q", got)
	}
	fixture.description.Store(fixture.description.Load().(string) + "\nnew operator note")
	stop.InstanceUUID = "second"
	if err := svc.AfterInstanceTerminal(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	if got := fixture.description.Load().(string); got != "operator note\nnew operator note" {
		t.Fatalf("last consumer did not preserve operator descriptions: %q", got)
	}
	if fixture.writes.Load() != 2 || fixture.gpu.Load() {
		t.Fatal("last consumer did not release the physical GPU exactly once")
	}
}

func TestGPUAdmissionReadsCurrentCloudAllocation(t *testing.T) {
	for _, tc := range []struct {
		protocol  string
		allocated bool
	}{
		{"gpu-allocation", false}, {"gpu-allocation", true}, {"pci", false}, {"pci", true}, {"gpu-pci-state", false}, {"gpu-pci-state", true},
	} {
		t.Run(fmt.Sprintf("%s/allocated=%v", tc.protocol, tc.allocated), func(t *testing.T) {
			st, svc, fixture := newCloudRuntimeFixtureForGPUProtocol(t, tc.protocol)
			fixture.allocated.Store(tc.allocated)
			fixture.gpu.Store(!tc.allocated)
			if err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "rejected", GPUIndexes: []int{0}}); err == nil {
				t.Fatal("stale available snapshot admitted a currently occupied GPU")
			}
			leases, err := cloudstore.New(st.DB()).CloudResourceLeasesForInstance("rejected")
			if err != nil || len(leases) != 0 || fixture.writes.Load() != 0 {
				t.Fatalf("rejected admission changed resources: leases=%+v err=%v", leases, err)
			}
		})
	}
}

func TestGPUReleasedAfterInstallationCanBeReserved(t *testing.T) {
	st, svc, _ := newCloudRuntimeFixture(t)
	ledger := cloudstore.New(st.DB())
	host, err := ledger.CloudHostByNodeUUID("cloud-node")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot inventory.CloudResource
	if err := json.Unmarshal([]byte(host.Snapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.GPUs[0].Allocation = "Allocated"
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	host.Snapshot = string(raw)
	if err := ledger.SaveCloudHostSnapshot(*host); err != nil {
		t.Fatal(err)
	}
	if err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "now-free", GPUIndexes: []int{0}}); err != nil {
		t.Fatalf("installation baseline overrode live availability: %v", err)
	}
}

func TestUncertainGPUWriteBlocksNewReservation(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	ledger := cloudstore.New(st.DB())
	host, err := ledger.CloudHostByNodeUUID("cloud-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CreateCloudResourceLease(&cloudstore.CloudResourceLease{
		OperationID: "interrupted", ProviderID: host.ProviderID, CloudHostID: host.ID, NodeUUID: host.NodeUUID,
		InstanceUUID: "previous", ResourceType: "gpu", ResourceID: "gpu-1", Amount: 1, Unit: "device", State: cloudstore.CloudLeaseReservePending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "new", GPUIndexes: []int{0}}); err == nil || fixture.writes.Load() != 0 {
		t.Fatalf("new reservation bypassed an uncertain write: %v", err)
	}
}

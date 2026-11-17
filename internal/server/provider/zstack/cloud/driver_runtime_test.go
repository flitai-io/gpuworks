package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

type cloudRuntimeFixture struct {
	memory               atomic.Int64
	description          atomic.Value
	pendingDescription   atomic.Value
	gpu                  atomic.Bool // true means disabled
	allocated            atomic.Bool
	pending              atomic.Bool
	writes               atomic.Int64
	hostQueries          atomic.Int64
	gpuQueries           atomic.Int64
	releaseFailureStatus atomic.Int32
	reserveFailureStatus atomic.Int32
	reserveBeforeTimeout atomic.Bool

	allocationFromPCI atomic.Bool
}

func newCloudRuntimeFixture(t *testing.T) (*store.Store, *provider.Service, *cloudRuntimeFixture) {
	t.Helper()
	return newCloudRuntimeFixtureWithProtocol(t, false)
}

func newCloudRuntimeFixtureForGPUProtocol(t *testing.T, protocol string) (*store.Store, *provider.Service, *cloudRuntimeFixture) {
	t.Helper()
	st, svc, fixture := newCloudRuntimeFixtureWithProtocol(t, protocol == "pci")
	fixture.allocationFromPCI.Store(protocol == "gpu-pci-state")
	return st, svc, fixture
}

func newCloudRuntimeFixtureWithProtocol(t *testing.T, legacy bool) (*store.Store, *provider.Service, *cloudRuntimeFixture) {
	t.Helper()
	fixture := &cloudRuntimeFixture{}
	fixture.description.Store("")
	fixture.pendingDescription.Store("")
	fixture.memory.Store(8 << 30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts":
			if query := r.URL.Query(); len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=host-1" {
				t.Errorf("reservation must query only its host: %s", r.URL)
				http.Error(w, "unexpected query", http.StatusBadRequest)
				return
			}
			fixture.hostQueries.Add(1)
			fmt.Fprint(w, `{"inventories":[{"uuid":"host-1","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`)
		case r.Method == http.MethodGet && legacy && r.URL.Path == "/v1/gpu-device/gpu-devices":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "no api mapping to "+r.URL.Path)
		case r.Method == http.MethodGet && ((!legacy && r.URL.Path == "/v1/gpu-device/gpu-devices") || (legacy && r.URL.Path == "/v1/pci-device/pci-devices")):
			if query := r.URL.Query(); len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=gpu-1" {
				t.Errorf("reservation must query only its GPU: %s", r.URL)
				http.Error(w, "unexpected query", http.StatusBadRequest)
				return
			}
			fixture.gpuQueries.Add(1)
			state, allocation := "Enabled", "Unallocated"
			if fixture.gpu.Load() {
				state = "Disabled"
			}
			if fixture.allocated.Load() {
				allocation = "Allocated"
			}
			if legacy || fixture.allocationFromPCI.Load() {
				status := "Active"
				if fixture.allocated.Load() {
					status = "Reserved"
				}
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-1","hostUuid":"host-1","type":"GPU_3D_Controller","virtStatus":"UNVIRTUALIZABLE","pciDeviceAddress":"0000:17:00.0","state":%q,"status":%q,"description":%q}]}`, state, status, fixture.description.Load().(string))
			} else {
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-1","hostUuid":"host-1","pciDeviceAddress":"0000:17:00.0","state":%q,"allocateStatus":%q,"description":%q}]}`, state, allocation, fixture.description.Load().(string))
			}
		case r.Method == http.MethodPut && r.URL.Path == "/v1/pci-device/pci-devices/gpu-1/actions":
			var body struct {
				Update struct {
					State       string  `json:"state"`
					Description *string `json:"description"`
				} `json:"updatePciDevice"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Update.State != "Disabled" && body.Update.State != "Enabled") {
				http.Error(w, "invalid device state", http.StatusBadRequest)
				return
			}
			fixture.writes.Add(1)
			status := fixture.releaseFailureStatus.Load()
			if body.Update.State == "Disabled" {
				status = fixture.reserveFailureStatus.Load()
			}
			if status == http.StatusBadRequest {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"code":"SYS.1007","description":"Invalid argument"}}`)
				return
			}
			if status == http.StatusGatewayTimeout {
				if body.Update.State == "Disabled" && fixture.reserveBeforeTimeout.Load() {
					fixture.gpu.Store(true)
					if body.Update.Description != nil {
						fixture.description.Store(*body.Update.Description)
					}
				}
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			fixture.pending.Store(body.Update.State == "Disabled")
			if body.Update.Description != nil {
				fixture.pendingDescription.Store(*body.Update.Description)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"location":"/v1/api-jobs/update-device"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/api-jobs/update-device":
			status := fixture.releaseFailureStatus.Load()
			if fixture.pending.Load() {
				status = fixture.reserveFailureStatus.Load()
			}
			if status == http.StatusServiceUnavailable {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":{"code":"SYS.1006","description":"Operation failed"}}`)
				return
			}
			fixture.gpu.Store(fixture.pending.Load())
			fixture.description.Store(fixture.pendingDescription.Load().(string))
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/pci-device/pci-devices/gpu-1":
			state := "Enabled"
			if fixture.gpu.Load() {
				state = "Disabled"
			}
			fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-1","state":%q,"description":%q}]}`, state, fixture.description.Load().(string))
		default:
			t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("cloud-runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	svc.ConfigureDrivers(nil)
	creds, err := json.Marshal(provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Encrypt(creds)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProvider(&store.Provider{Name: "test", Kind: "zstack", CredsCipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("cloud-node", store.HashToken("node-token"), "host"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerURL("cloud-node", "http://192.0.2.100:8099"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("cloud-node", proto.NodeStatus{
		Sys: proto.SysInfo{MemTotalKB: 64 * 1024 * 1024},
		GPUs: []proto.GPUInfo{{Index: 0, UUID: "gpu", Name: "GPU", Vendor: "nvidia", MemoryTotalBytes: 24 << 30,
			PCIeBDF: "0000:17:00.0", Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	baseline, available := int64(8<<30), int64(32<<30)
	snapshot, err := json.Marshal(inventory.CloudResource{
		ExternalID: "host-1", Memory: inventory.ResourceCapacity{Available: &available, Reserved: &baseline},
		GPUs: []inventory.CloudGPU{{ExternalID: "gpu-1", PCIAddress: "0000:17:00.0", State: "Enabled", Allocation: "Unallocated", Mode: "PCI", Status: "Active"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.New(st.DB()).SaveCloudHostSnapshot(cloudstore.CloudHost{
		ProviderID: p.ID, ExternalID: "host-1", NodeUUID: "cloud-node", Snapshot: string(snapshot), InitialMemoryReserved: &baseline,
	}); err != nil {
		t.Fatal(err)
	}
	return st, svc, fixture
}

func TestCloudRuntimeReservesAndReleasesOnlyAssignedGPUs(t *testing.T) {
	for _, tc := range []struct {
		protocol     string
		createFailed bool
	}{
		{"gpu-allocation", false}, {"gpu-allocation", true}, {"pci", false}, {"pci", true}, {"gpu-pci-state", false}, {"gpu-pci-state", true},
	} {
		t.Run(fmt.Sprintf("%s/createFailed=%v", tc.protocol, tc.createFailed), func(t *testing.T) {
			st, svc, fixture := newCloudRuntimeFixtureForGPUProtocol(t, tc.protocol)
			req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
			ordinary := req
			ordinary.NodeUUID = "ordinary-node"
			if err := svc.BeforeCreate(t.Context(), ordinary); err != nil || fixture.writes.Load() != 0 {
				t.Fatalf("ordinary node changed Cloud resources: %v", err)
			}
			for range 2 { // Replaying the same operation must not reserve twice.
				if err := svc.BeforeCreate(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.memory.Load() != 8<<30 || !fixture.gpu.Load() || fixture.writes.Load() != 1 {
				t.Fatal("generic lifecycle must only disable the selected GPU exactly once")
			}
			leases, err := cloudstore.New(st.DB()).CloudResourceLeasesForInstance(req.InstanceUUID)
			if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved || leases[0].ResourceType != "gpu" || leases[0].ResourceID != "gpu-1" {
				t.Fatalf("reservation ledger = %+v, %v", leases, err)
			}
			if wait, err := svc.RequiresStopConfirmation(t.Context(), req.NodeUUID); err != nil || !wait {
				t.Fatalf("Cloud stop confirmation lost: %v, %v", wait, err)
			}
			for range 2 {
				if tc.createFailed {
					err = svc.OnCreateFailed(t.Context(), req)
				} else {
					err = svc.AfterInstanceTerminal(t.Context(), provider.InferenceNodeStopRequest{
						NodeUUID: req.NodeUUID, InstanceUUID: req.InstanceUUID, State: "stopped",
					})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			leases, err = cloudstore.New(st.DB()).CloudResourceLeasesForInstance(req.InstanceUUID)
			if err != nil || len(leases) != 0 || fixture.memory.Load() != 8<<30 || fixture.gpu.Load() || fixture.writes.Load() != 2 {
				t.Fatalf("release must restore baseline exactly once: leases=%+v err=%v", leases, err)
			}
		})
	}
}

func TestCloudRuntimeRetriesGPUReleaseAfterCloudFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		kind   apiclient.ErrorKind
	}{
		{name: "request rejected", status: http.StatusBadRequest, kind: apiclient.ErrorRequest},
		{name: "async operation failed", status: http.StatusServiceUnavailable, kind: apiclient.ErrorOperation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, svc, fixture := newCloudRuntimeFixture(t)
			req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
			if err := svc.BeforeCreate(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			ledger := cloudstore.New(st.DB())
			leases, err := ledger.CloudResourceLeasesForInstance(req.InstanceUUID)
			if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved {
				t.Fatalf("reservation ledger = %+v, err=%v", leases, err)
			}
			leaseID := leases[0].ID
			stop := provider.InferenceNodeStopRequest{NodeUUID: req.NodeUUID, InstanceUUID: req.InstanceUUID, State: proto.InstStopped}
			fixture.releaseFailureStatus.Store(int32(tc.status))
			for attempt := int64(1); attempt <= 2; attempt++ {
				err := svc.AfterInstanceTerminal(t.Context(), stop)
				var apiErr *apiclient.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Kind != tc.kind {
					t.Fatalf("release attempt %d must return the Cloud failure: %v", attempt, err)
				}
				lease, err := ledger.CloudResourceLeaseByID(leaseID)
				if err != nil || lease.State != cloudstore.CloudLeaseReleasePending {
					t.Fatalf("failed release must remain pending: lease=%+v err=%v", lease, err)
				}
				if !fixture.gpu.Load() || fixture.writes.Load() != 1+attempt {
					t.Fatalf("each attempt must reach Cloud and keep the GPU disabled on failure: disabled=%v writes=%d", fixture.gpu.Load(), fixture.writes.Load())
				}
			}

			fixture.releaseFailureStatus.Store(0)
			if err := svc.AfterInstanceTerminal(t.Context(), stop); err != nil {
				t.Fatalf("pending release must succeed after Cloud recovers: %v", err)
			}
			lease, err := ledger.CloudResourceLeaseByID(leaseID)
			if err != nil || lease.State != cloudstore.CloudLeaseReleased || lease.ErrorCode != "" {
				t.Fatalf("successful release must be persisted: lease=%+v err=%v", lease, err)
			}
			if fixture.gpu.Load() || fixture.writes.Load() != 4 {
				t.Fatalf("successful retry must enable the GPU: disabled=%v writes=%d", fixture.gpu.Load(), fixture.writes.Load())
			}
			if err := svc.AfterInstanceTerminal(t.Context(), stop); err != nil {
				t.Fatal(err)
			}
			if fixture.writes.Load() != 4 {
				t.Fatal("completed release submitted another Cloud write")
			}
		})
	}
}

func TestCloudRuntimeOwnershipLookupFailsClosed(t *testing.T) {
	if handler, err := New().InferenceNodeLifecycleFor("cloud-node"); err == nil || handler != nil {
		t.Fatal("an unconfigured prototype exposed a lifecycle handler")
	}
	st, svc, fixture := newCloudRuntimeFixture(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference"}); err == nil {
		t.Fatal("failed ownership lookup was mistaken for an unmanaged node")
	}
	if fixture.writes.Load() != 0 {
		t.Fatal("failed ownership lookup changed Cloud resources")
	}
}

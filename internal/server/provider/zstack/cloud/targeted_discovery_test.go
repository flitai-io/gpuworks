package cloud

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestQueryHostResourcesReadsOnlySelectedResources(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("invalid targeted request: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		query := r.URL.Query()
		if len(query) != 1 || len(query["q"]) != 1 {
			t.Errorf("query must contain exactly one UUID condition: %s", r.URL)
			http.Error(w, "invalid query", http.StatusBadRequest)
			return
		}
		key := r.URL.Path + "?" + query.Get("q")
		mu.Lock()
		requests = append(requests, key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "/v1/hosts?uuid=host-1":
			fmt.Fprint(w, `{"inventories":[{"uuid":"host-1","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`)
		case "/v1/gpu-device/gpu-devices?uuid=gpu-1":
			fmt.Fprint(w, `{"inventories":[{"uuid":"gpu-1","hostUuid":"host-1","state":"Disabled","allocateStatus":"Unallocated","pciDeviceAddress":"0000:17:00.0","status":"Active","mode":"PCI"}]}`)
		case "/v1/gpu-device/gpu-devices?uuid=gpu-2":
			fmt.Fprint(w, `{"inventories":[{"uuid":"gpu-2","hostUuid":"host-1","state":"Enabled","allocateStatus":"Allocated","vmInstanceUuid":"vm-1"}]}`)
		default:
			t.Errorf("queried unrelated resources: %s", r.URL)
			http.Error(w, "unrelated resources unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	resource, err := New().QueryHostResources(t.Context(), provider.Credentials{
		Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
	}, "host-1", []string{"gpu-1", "gpu-2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/hosts?uuid=host-1", "/v1/gpu-device/gpu-devices?uuid=gpu-1", "/v1/gpu-device/gpu-devices?uuid=gpu-2"}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
	if resource.ExternalID != "host-1" || resource.State != "Enabled" || resource.Status != "Connected" || len(resource.GPUs) != 2 {
		t.Fatalf("targeted host = %+v", resource)
	}
	if resource.GPUs[0].ExternalID != "gpu-1" || resource.GPUs[0].State != "Disabled" || resource.GPUs[0].Allocation != "Unallocated" || resource.GPUs[0].PCIAddress != "0000:17:00.0" {
		t.Fatalf("lost selected GPU identity or live state: %+v", resource.GPUs[0])
	}
	if resource.GPUs[1].ExternalID != "gpu-2" || resource.GPUs[1].Allocation != "Allocated" || resource.GPUs[1].VMUUID != "vm-1" {
		t.Fatalf("lost Cloud VM ownership: %+v", resource.GPUs[1])
	}
}

func TestQueryHostResourcesRejectsInvalidOrIncompleteInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		code int
	}{
		{name: "host missing", path: "/v1/hosts", body: `{"inventories":[]}`},
		{name: "host identity mismatch", path: "/v1/hosts", body: `{"inventories":[{"uuid":"another-host"}]}`},
		{name: "host query failed", path: "/v1/hosts", body: `{"error":{"code":"SYS.1006"}}`, code: http.StatusServiceUnavailable},
		{name: "GPU missing", path: "/v1/gpu-device/gpu-devices", body: `{"inventories":[]}`},
		{name: "GPU envelope missing", path: "/v1/gpu-device/gpu-devices", body: `{}`},
		{name: "multiple GPUs returned", path: "/v1/gpu-device/gpu-devices", body: `{"inventories":[{"uuid":"gpu-1","hostUuid":"host-1"},{"uuid":"gpu-2","hostUuid":"host-1"}]}`},
		{name: "legacy GPU identity mismatch", path: "/v1/pci-device/pci-devices", body: `{"inventories":[{"uuid":"gpu-2","hostUuid":"host-1","type":"GPU_3D_Controller"}]}`},
		{name: "legacy GPU moved to another host", path: "/v1/pci-device/pci-devices", body: `{"inventories":[{"uuid":"gpu-1","hostUuid":"another-host","type":"GPU_3D_Controller"}]}`},
		{name: "GPU identity mismatch", path: "/v1/gpu-device/gpu-devices", body: `{"inventories":[{"uuid":"gpu-2","hostUuid":"host-1"}]}`},
		{name: "GPU moved to another host", path: "/v1/gpu-device/gpu-devices", body: `{"inventories":[{"uuid":"gpu-1","hostUuid":"another-host"}]}`},
		{name: "GPU host missing", path: "/v1/gpu-device/gpu-devices", body: `{"inventories":[{"uuid":"gpu-1"}]}`},
		{name: "GPU query failed", path: "/v1/gpu-device/gpu-devices", body: `{"error":{"code":"SYS.1006"}}`, code: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.path == "/v1/pci-device/pci-devices" && r.URL.Path == "/v1/gpu-device/gpu-devices" {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, "no api mapping to "+r.URL.Path)
					return
				}
				if r.URL.Path == tc.path {
					if tc.code != 0 {
						w.WriteHeader(tc.code)
					}
					fmt.Fprint(w, tc.body)
					return
				}
				if r.URL.Path == "/v1/hosts" {
					fmt.Fprint(w, `{"inventories":[{"uuid":"host-1"}]}`)
					return
				}
				t.Errorf("unexpected request: %s", r.URL)
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			resource, err := New().QueryHostResources(t.Context(), provider.Credentials{
				Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
			}, "host-1", []string{"gpu-1"})
			if err == nil || !reflect.DeepEqual(resource, inventory.CloudResource{}) {
				t.Fatalf("invalid response must fail without partial inventory: resource=%+v err=%v", resource, err)
			}
		})
	}
}

func TestQueryHostResourcesRequiresExplicitUniqueTargets(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected query", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name string
		host string
		gpus []string
	}{
		{name: "no host", gpus: []string{"gpu-1"}},
		{name: "no GPUs", host: "host-1"},
		{name: "empty GPU", host: "host-1", gpus: []string{" "}},
		{name: "duplicate GPU", host: "host-1", gpus: []string{"gpu-1", "gpu-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().QueryHostResources(t.Context(), provider.Credentials{
				Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
			}, tc.host, tc.gpus)
			if err == nil || requests.Load() != 0 {
				t.Fatalf("invalid targets must not trigger Cloud queries: err=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestCloudReservationQueriesOnlySelectedResources(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
	if err := svc.BeforeCreate(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if fixture.hostQueries.Load() != 1 || fixture.gpuQueries.Load() != 1 || fixture.writes.Load() != 1 || !fixture.gpu.Load() {
		t.Fatalf("reservation must query its host and GPU once, then disable that GPU: host=%d gpu=%d writes=%d disabled=%v",
			fixture.hostQueries.Load(), fixture.gpuQueries.Load(), fixture.writes.Load(), fixture.gpu.Load())
	}
	leases, err := cloudstore.New(st.DB()).CloudResourceLeasesForInstance(req.InstanceUUID)
	if err != nil || len(leases) != 1 || leases[0].ResourceID != "gpu-1" || leases[0].State != cloudstore.CloudLeaseReserved {
		t.Fatalf("targeted reservation ledger = %+v, err=%v", leases, err)
	}
}

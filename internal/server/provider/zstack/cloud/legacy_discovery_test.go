package cloud

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
)

// Wire fixtures follow Cloud 4.8.0's PciDeviceInventory and ResourceConfig errors.
func TestLegacyCloudDiscoveryAndCPUReservation(t *testing.T) {
	testCloudDiscoveryWithPCIOwnership(t, false)
}

func TestIntermediateCloudDiscoveryAndCPUReservation(t *testing.T) {
	testCloudDiscoveryWithPCIOwnership(t, true)
}

func testCloudDiscoveryWithPCIOwnership(t *testing.T, gpuEndpoint bool) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected write or invalid AccessKey: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/hosts":
			fmt.Fprint(w, `{"inventories":[{"uuid":"host","name":"GPU host","managementIp":"192.0.2.10","hypervisorType":"KVM","state":"Enabled","status":"Connected","totalCpuCapacity":32,"availableCpuCapacity":24,"totalMemoryCapacity":68719476736,"availableMemoryCapacity":34359738368}]}`)
		case "/v1/gpu-device/gpu-devices":
			if gpuEndpoint {
				fmt.Fprint(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","name":"NVIDIA Test","type":"GPU_3D_Controller","pciDeviceAddress":"0000:17:00.0","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"}]}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "no api mapping to "+r.URL.Path)
		case "/v1/pci-device/pci-devices":
			if gpuEndpoint {
				t.Error("intermediate GPU response already carries PCI ownership")
			}
			fmt.Fprint(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","name":"NVIDIA Test","type":"GPU_3D_Controller","pciDeviceAddress":"0000:17:00.0","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"},{"uuid":"audio","hostUuid":"host","type":"GPU_Audio_Controller","state":"Enabled","status":"Active"}]}`)
		case "/v1/resource-configurations/host/kvm/reservedMemory":
			fmt.Fprint(w, `{"value":"1G"}`)
		case "/v1/resource-configurations/host/kvm/reservedCpu":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"SYS.1007","description":"Invalid argument","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`)
		case "/v1/global-configurations":
			fmt.Fprint(w, `{"inventories":[{"category":"kvm","name":"reservedCpu","value":"3"}]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	creds := provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
	driver := New()
	if err := driver.ValidateCredentials(t.Context(), creds); err != nil {
		t.Fatal(err)
	}
	resources, err := driver.DiscoverResources(t.Context(), creds)
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	host := resources[0]
	if host.ExternalID != "host" || host.CPU.Reserved == nil || *host.CPU.Reserved != 3 || host.Memory.Reserved == nil || *host.Memory.Reserved != 1<<30 || len(host.GPUs) != 1 {
		t.Fatalf("incorrect host snapshot: %+v", host)
	}
	if gpu := host.GPUs[0]; gpu.ExternalID != "gpu" || gpu.Model != "NVIDIA Test" || gpu.Allocation != "Unallocated" || gpu.PCIAddress != "0000:17:00.0" {
		t.Fatalf("incorrect GPU: %+v", gpu)
	}
	// An unbound CPU setting is read globally; a host mutation must not alter it.
	if err := driver.ApplyResourceMutation(t.Context(), creds, resourceclaim.ResourceMutation{ResourceType: "cpu", ResourceID: "host", Delta: 2}); err != nil {
		t.Fatalf("unbound CPU reservation should retain existing no-op behavior: %v", err)
	}
}

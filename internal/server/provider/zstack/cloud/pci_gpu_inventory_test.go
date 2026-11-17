package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPCIInventoryFindsGPUsWithoutPagingThroughOtherDevices(t *testing.T) {
	const nonGPUs = 100000
	const gpuCount = 101
	wantTypes := []string{"GPU_Video_Controller", "GPU_3D_Controller", "GPU_Processing_Accelerators", "GPU_Co_Processor"}
	var calls atomic.Int32
	creds := gpuInventoryCloud(t, "/v1/pci-device/pci-devices", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		query := r.URL.Query()
		start, err := strconv.Atoi(query.Get("start"))
		if err != nil || start < 0 || query.Get("limit") != "100" || query.Get("sort") != "uuid" {
			t.Errorf("invalid PCI pagination: %s", r.URL)
			http.Error(w, "invalid query", http.StatusBadRequest)
			return
		}
		filtered := len(query["q"]) != 0
		if filtered {
			if len(query["q"]) != 1 || !strings.HasPrefix(query.Get("q"), "type?=") {
				t.Errorf("unexpected PCI filter: %s", r.URL)
				http.Error(w, "invalid filter", http.StatusBadRequest)
				return
			}
			types := strings.Split(strings.TrimPrefix(query.Get("q"), "type?="), ",")
			slices.Sort(types)
			want := slices.Clone(wantTypes)
			slices.Sort(want)
			if !slices.Equal(types, want) {
				t.Errorf("PCI type filter = %v, want %v", types, want)
				http.Error(w, "incorrect types", http.StatusBadRequest)
				return
			}
		}
		total := nonGPUs + gpuCount
		if filtered {
			total = gpuCount
		}
		rows := []map[string]string{}
		for i := start; i < min(start+100, total); i++ {
			index := i - nonGPUs
			if filtered {
				index = i
			}
			row := map[string]string{"uuid": fmt.Sprintf("pci-%06d", i), "type": "Ethernet_Controller"}
			if index >= 0 {
				row = map[string]string{
					"uuid": fmt.Sprintf("gpu-%03d", index), "hostUuid": "host", "type": wantTypes[index%len(wantTypes)],
					"state": "Enabled", "status": "Active", "virtStatus": "UNVIRTUALIZABLE",
				}
			}
			rows = append(rows, row)
		}
		json.NewEncoder(w).Encode(map[string]any{"inventories": rows})
	})
	instances, err := New().ListInstances(t.Context(), creds)
	if err != nil || len(instances) != 1 || instances[0].GPUCount != gpuCount {
		t.Fatalf("GPU host discovery scanned unrelated PCI devices: instances=%+v err=%v", instances, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("PCI requests=%d, want only the two GPU pages", calls.Load())
	}
}

func TestPCIInventoryCountsPhysicalGPUWithoutItsVirtualFunctions(t *testing.T) {
	creds := gpuInventoryCloud(t, "/v1/pci-device/pci-devices", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"inventories":[
			{"uuid":"physical","hostUuid":"host","name":"Physical GPU","type":"GPU_3D_Controller","state":"Enabled","status":"Active","virtStatus":"SRIOV_VIRTUALIZED"},
			{"uuid":"vf-1","hostUuid":"host","name":"Virtual GPU","type":"GPU_3D_Controller","parentUuid":"physical","state":"Enabled","status":"Active","virtStatus":"SRIOV_VIRTUAL"},
			{"uuid":"vf-2","hostUuid":"host","name":"Virtual GPU","type":"GPU_3D_Controller","parentUuid":"physical","state":"Enabled","status":"Active","virtStatus":"SRIOV_VIRTUAL"},
			{"uuid":"vf-3","hostUuid":"host","name":"Virtual GPU","type":"GPU_3D_Controller","parentUuid":"physical","state":"Enabled","status":"Attached","vmInstanceUuid":"vm","virtStatus":"SRIOV_VIRTUAL"},
			{"uuid":"vf-4","hostUuid":"host","name":"Virtual GPU","type":"GPU_3D_Controller","parentUuid":"physical","state":"Disabled","status":"System","virtStatus":"SRIOV_VIRTUAL"}
		]}`)
	})
	instances, err := New().ListInstances(t.Context(), creds)
	if err != nil || len(instances) != 1 || instances[0].GPUCount != 1 || instances[0].GPUModel != "Physical GPU" {
		t.Fatalf("host inventory must count the physical GPU, not its VFs: instances=%+v err=%v", instances, err)
	}
}

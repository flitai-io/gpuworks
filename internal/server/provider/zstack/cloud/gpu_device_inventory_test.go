package cloud

import (
	"fmt"
	"net/http"
	"testing"
)

func TestGPUDeviceInventoryCountsPhysicalGPUWithoutItsVirtualFunctions(t *testing.T) {
	creds := gpuInventoryCloud(t, "/v1/gpu-device/gpu-devices", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"inventories":[
			{"uuid":"physical","hostUuid":"host","gpuType":"Physical GPU","state":"Enabled","status":"Active","allocateStatus":"Unallocated"},
			{"uuid":"vf-1","hostUuid":"host","gpuType":"Virtual GPU","parentUuid":"physical","state":"Enabled","status":"Active","allocateStatus":"Unallocated"},
			{"uuid":"vf-2","hostUuid":"host","gpuType":"Virtual GPU","parentUuid":"physical","state":"Enabled","status":"Active","allocateStatus":null},
			{"uuid":"vf-3","hostUuid":"host","gpuType":"Virtual GPU","parentUuid":"physical","state":"Enabled","status":"Active","type":"GPU_3D_Controller","virtStatus":"SRIOV_VIRTUAL"},
			{"uuid":"vf-4","hostUuid":"host","gpuType":"Virtual GPU","parentUuid":"physical","state":"Enabled","status":"Attached","vmInstanceUuid":"vm","allocateStatus":"Allocated"}
		]}`)
	})
	instances, err := New().ListInstances(t.Context(), creds)
	if err != nil || len(instances) != 1 || instances[0].GPUCount != 1 || instances[0].GPUModel != "Physical GPU" {
		t.Fatalf("host inventory must count the physical GPU, not its VFs: instances=%+v err=%v", instances, err)
	}
}

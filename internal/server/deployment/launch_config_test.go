package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func launchConfigNode(t *testing.T, st *store.Store, uuid string) {
	t.Helper()
	if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
}

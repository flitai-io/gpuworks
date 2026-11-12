package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReportedGPUFactsPreservesReportedArch(t *testing.T) {
	facts := reportedGPUFacts(&store.Node{GPUs: []store.GPU{{Vendor: "amd", Name: "MI300X", ArchID: "gfx942"}, {Vendor: "ascend", Name: "310P"}}})
	if facts[0]["arch_id"] != "gfx942" {
		t.Fatalf("lost reported arch: %v", facts)
	}
	if _, present := facts[1]["arch_id"]; present {
		t.Fatalf("invented unknown arch: %v", facts)
	}
}

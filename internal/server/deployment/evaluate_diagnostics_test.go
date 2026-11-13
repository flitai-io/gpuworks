package deployment

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReportedGPUFactsKeepMixedAndUnknownCapabilities(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{
		{Name: "RTX 2080 Ti", Vendor: "nvidia", ComputeCapability: "7.5"},
		{Name: "H100", Vendor: "nvidia", ComputeCapability: ""},
		{Name: "310P", Vendor: "ascend"},
		{Name: "not owned", Ownership: "foreign", ComputeCapability: "9.0"},
	}}
	want := []map[string]string{
		{"name": "RTX 2080 Ti", "vendor": "nvidia", "cc": "7.5"},
		{"name": "H100", "vendor": "nvidia", "cc": ""},
		{"name": "310P", "vendor": "ascend", "cc": ""},
	}
	if got := reportedGPUFacts(node); !reflect.DeepEqual(got, want) {
		t.Fatalf("reported facts=%v", got)
	}
	if got := reportedGPUFacts(&store.Node{}); len(got) != 0 {
		t.Fatalf("invented GPU facts=%v", got)
	}
}

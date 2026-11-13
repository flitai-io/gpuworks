package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAgentVersionGateOnlyRefusesAProvablyOlderAgent(t *testing.T) {
	for _, tc := range []struct {
		version string
		refused bool
	}{
		{version: "26.10.9", refused: true},
		{version: "v26.10.9", refused: true},
		{version: "0.0.1", refused: true},
		{version: "26.11.0"},
		{version: "v26.11.0"},
		{version: "26.11.1"},
		{version: "27.1.0"},
		// git describe after a tag describes a build past that tag.
		{version: "26.11.0-14-gabc1234"},
		{version: "26.10.0-14-gabc1234", refused: true},
		// A development build cannot be ordered against a release, and the
		// build of this very tree is one of them.
		{version: "dev"},
		{version: ""},
		{version: "ac2f04a4-dirty"},
		{version: "not.a.version"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := agentPredatesURLModels(tc.version); got != tc.refused {
				t.Fatalf("agentPredatesURLModels(%q) = %v, want %v", tc.version, got, tc.refused)
			}
		})
	}
}

func TestModelSourceEndpointAndHostOnlyDescribeDirectoryModels(t *testing.T) {
	directory := &store.Model{Source: "URL", SourceURL: " https://mirror.example.com:8443/Qwen/Qwen3-0.6B "}
	if got := modelSourceEndpoint(directory); got != "https://mirror.example.com:8443/Qwen/Qwen3-0.6B" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := modelSourceHost(directory); got != "mirror.example.com:8443" {
		t.Fatalf("host = %q", got)
	}
	for _, model := range []*store.Model{nil, {Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B"}, {Source: "local"}} {
		if got := modelSourceEndpoint(model); got != "" {
			t.Fatalf("endpoint for %+v = %q", model, got)
		}
		if got := modelSourceHost(model); got != "" {
			t.Fatalf("host for %+v = %q", model, got)
		}
	}
}

func urlModelStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

package deployment

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Deployment admission reaches the model's own source to confirm it can still
// be downloaded; for a directory that means the directory, not a repository
// host it was never served from.
func TestDeploymentAdmissionReachesTheDirectoryItself(t *testing.T) {
	st := urlModelStore(t, "api_admit_url_model")
	endpoints := []string{}
	h := New(Deps{Store: st, FetchModelMeta: func(_ context.Context, source, _, _, _ string, candidates []string) (*modelmeta.Meta, error) {
		endpoints = append(endpoints, source+"|"+strings.Join(candidates, ","))
		return &modelmeta.Meta{}, nil
	}})
	directory, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("admit-node", store.HashToken("token"), "admit-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.admitDeploymentModel(context.Background(), directory, store.SourceRules{}, []*store.Node{node}); err != nil {
		t.Fatalf("admission failed: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0] != "url|https://hf-mirror.example.com/Qwen/Qwen3-0.6B" {
		t.Fatalf("admission fetches = %v", endpoints)
	}
}

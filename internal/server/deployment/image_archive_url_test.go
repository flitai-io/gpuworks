package deployment

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// imageWithSources stores one catalog image carrying the given registry_sources.
func imageWithSources(t *testing.T, dsn string, sources []string) (*store.Store, *store.Image) {
	t.Helper()
	st, err := store.Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "registry.example/team/runner:v1", Engine: "vllm", Enabled: true, RegistrySources: string(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	return st, image
}

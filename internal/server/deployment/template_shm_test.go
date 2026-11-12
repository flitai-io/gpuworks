package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template spec's shm_bytes is the /dev/shm its engine needs. The preview
// shows what the launch will get: the larger of that figure and the derived
// size, unless the draft states its own size or shares the host's /dev/shm.
func TestTemplateSnapshotKeepsTheSpecShmBytes(t *testing.T) {
	selection := templateSelection{
		Model: &store.InferenceModel{ModelID: "acme/shm"},
		Spec:  &store.InferenceSpecRow{Name: "large-shm", Source: "upstream", ShmBytes: 400 << 30},
	}
	if got := snapshotTemplateSelection(selection, "img:1").ShmBytes; got != 400<<30 {
		t.Fatalf("snapshot shm_bytes = %d", got)
	}
	if got := selection.shmBytes(); got != 400<<30 {
		t.Fatalf("selection shm = %d", got)
	}
	if got := (templateSelection{}).shmBytes(); got != 0 {
		t.Fatalf("a deployment without a template states shm %d", got)
	}
}

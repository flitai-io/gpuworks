package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A spec's memlock_unlimited travels in the deployment's snapshot, and a
// deployment without a template states none.
func TestTemplateSnapshotKeepsTheSpecMemlockUnlimited(t *testing.T) {
	selection := templateSelection{
		Model: &store.InferenceModel{ModelID: "acme/memlock"},
		Spec:  &store.InferenceSpecRow{Name: "pinned", Source: "upstream", MemlockUnlimited: true},
	}
	if !snapshotTemplateSelection(selection, "img:1").MemlockUnlimited || !selection.memlockUnlimited() {
		t.Fatal("snapshot lost memlock_unlimited")
	}
	if (templateSelection{}).memlockUnlimited() {
		t.Fatal("a deployment without a template asks for unlimited locked memory")
	}
	selection.Spec.MemlockUnlimited = false
	if snapshotTemplateSelection(selection, "img:1").MemlockUnlimited || selection.memlockUnlimited() {
		t.Fatal("a spec stating none reads as unlimited")
	}
}

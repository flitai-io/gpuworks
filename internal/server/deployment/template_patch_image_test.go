package deployment

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A spec that patches its own runtime carries a pre-start script or assets
// written for the engine sources of one image. Running it on another image
// may be refused by the patch inside the container, so the selection warns
// and names both images; it does not refuse the choice.
func TestAPatchedSpecWarnsWhenAnotherImageIsChosen(t *testing.T) {
	fixture := deploymentFlatFixture()
	fixture.Specs[0].PreStartScript = "python3 /opt/gpuworks/patch.py"
	st, err := store.Open(filepath.Join(t.TempDir(), "patched-image.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	service := New(Deps{Store: st})
	hardware := templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}}

	const other = "gpuworks/gpuworks-runner:cuda13.0-vllm0.34.0-r1"
	req := dto.DeployRequest{TemplateID: model.ID, TemplateSpecName: "fp8-balanced", Image: other}
	selection, err := service.applyTemplateDefaults(&req, hardware)
	if err != nil {
		t.Fatalf("another image was refused: %v", err)
	}
	if req.Image != other {
		t.Fatalf("image = %q, want the choice kept", req.Image)
	}
	if len(selection.Warnings) != 1 || selection.Warnings[0].Code != messages.WarningTemplateImagePinnedByRuntimePatch ||
		selection.Warnings[0].Params["image"] != fixture.Specs[0].Image || selection.Warnings[0].Params["requested"] != other {
		t.Fatalf("warnings = %+v", selection.Warnings)
	}

	// The spec's own image, named explicitly, is the template's choice: no
	// warning.
	own := dto.DeployRequest{TemplateID: model.ID, TemplateSpecName: "fp8-balanced", Image: fixture.Specs[0].Image}
	selection, err = service.applyTemplateDefaults(&own, hardware)
	if err != nil {
		t.Fatalf("the spec's own image was refused: %v", err)
	}
	if len(selection.Warnings) != 0 {
		t.Fatalf("the spec's own image warned: %+v", selection.Warnings)
	}
}

// Without a patched runtime the image may be overridden with no warning; this
// rule does not narrow that beyond the patched specs.
func TestAnUnpatchedSpecWarnsNothingForAnotherImage(t *testing.T) {
	fixture := deploymentFlatFixture()
	st, err := store.Open(filepath.Join(t.TempDir(), "unpatched-image.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	req := dto.DeployRequest{TemplateID: model.ID, TemplateSpecName: "fp8-balanced", Image: "gpuworks/gpuworks-runner:cuda13.0-vllm0.34.0-r1"}
	selection, err := New(Deps{Store: st}).applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatalf("an unpatched spec refused another image: %v", err)
	}
	if len(selection.Warnings) != 0 {
		t.Fatalf("an unpatched spec warned: %+v", selection.Warnings)
	}
}

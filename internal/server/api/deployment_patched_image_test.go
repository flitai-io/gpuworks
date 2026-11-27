package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func patchedSpecFixture(t *testing.T, name string) (*store.Store, *apiHandlers, store.InferenceModel) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/patched"
	fixture.Specs[0].Name = "patched"
	fixture.Specs[0].PreStartScript = "python3 /opt/gpuworks/patch.py"
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, name+"-node")
	return st, &apiHandlers{d: Deps{Store: st}}, fixture
}

// A spec that patches its own runtime still deploys on a chosen image: the
// evaluate answer warns instead of refusing, naming the patch's image and the
// chosen one.
func TestEvaluateWarnsAnotherImageForAPatchedSpec(t *testing.T) {
	st, h, fixture := patchedSpecFixture(t, "evaluate_patched_spec")
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	const other = "gpuworks/gpuworks-runner:cuda13.0-vllm0.34.0-r1"
	evaluate := func(image string) string {
		t.Helper()
		body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"patched","node_uuid":"evaluate_patched_spec-node","image":%q,"quick_params":{},"placement":{}}`, template.ID, image)
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate on %s = %d %s", image, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if got := evaluate(other); !strings.Contains(got, `{"code":"TEMPLATE_IMAGE_PINNED_BY_RUNTIME_PATCH","params":{"image":"`+fixture.Specs[0].Image+`","requested":"`+other+`"}}`) {
		t.Fatalf("evaluate on another image = %s", got)
	}
	if got := evaluate(fixture.Specs[0].Image); strings.Contains(got, "TEMPLATE_IMAGE_PINNED_BY_RUNTIME_PATCH") {
		t.Fatalf("evaluate on the spec's own image warned: %s", got)
	}
}

// The edit succeeds on another image too: an edit cannot be stricter than the
// create the same request makes, and the warning travels with the evaluation
// the edit form runs.
func TestEditAcceptsAnotherImageForAPatchedSpec(t *testing.T) {
	st, h, fixture := patchedSpecFixture(t, "edit_patched_spec")
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	dep := createTemplateDeployment(t, h, map[string]any{
		"template_id": template.ID, "template_spec_name": "patched", "node_uuid": "edit_patched_spec-node",
	})
	draft, _ := getDraft(t, h, dep.ID)
	const other = "gpuworks/gpuworks-runner:cuda13.0-vllm0.34.0-r1"
	edit := draft.Deployment
	edit.Image = other
	status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision, Apply: "none"}, false)
	if status != http.StatusOK {
		t.Fatalf("edit to another image = %d %s", status, body)
	}
	if after, _ := st.GetDeployment(dep.ID); after.Image != other {
		t.Fatalf("image = %q, want the chosen one", after.Image)
	}
}

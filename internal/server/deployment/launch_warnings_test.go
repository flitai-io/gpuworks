package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// compatTemplateRig is a template whose own image is the compatibility image,
// on a udocker node. Requests built from it never state an image.
type compatTemplateRig struct {
	h      *Service
	st     *store.Store
	nodeID string
	body   func(extra map[string]any) string
}

func newCompatTemplateRig(t *testing.T, runtime string, mutate func(*store.InferenceSpecRow)) compatTemplateRig {
	t.Helper()
	st, err := store.Open("file:"+strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].Image = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	fixture.Specs[0].PreStartArgv = ""
	fixture.Specs[0].Env = `{}`
	mutate(&fixture.Specs[0])
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "tpl-node")
	status := nodeStatusTwoGPUs("tpl-node", 0)
	status.GPUs = append(status.GPUs, status.GPUs...)
	for index := range status.GPUs {
		status.GPUs[index].Index = index
	}
	status.Sys.RuntimeDriver = runtime
	if err := st.UpdateNodeStatus("tpl-node", status); err != nil {
		t.Fatal(err)
	}
	return compatTemplateRig{
		h: New(Deps{Store: st}), st: st, nodeID: "tpl-node",
		body: func(extra map[string]any) string {
			body := map[string]any{"template_id": template.ID, "template_spec_name": fixture.Specs[0].Name}
			for key, value := range extra {
				body[key] = value
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		},
	}
}

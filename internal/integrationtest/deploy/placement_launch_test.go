package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestPlacementPreviewAndPersistedLaunchAreIdentical(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("placement-launch-node",
		testenv.WithGPUs(4, testenv.GPUSpec{Model: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", VRAMMB: 32768}),
		testenv.WithAccelerator("ascend", "cann", "9.0.0"),
		testenv.WithDriverName("docker"),
	)
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "57559a6720ddaa2eaee52b09ec2ef011127997b2", ArtifactSubfolder: "FL2VA", Name: "MiniMax H3",
		Arch: "diffusion", WeightBytes: 144051182625,
		Meta: `{"model_type":"diffusion","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "minimax-h3")}); err != nil {
		t.Fatal(err)
	}
	embedded, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var h3 inferencecatalog.InferenceModel
	for _, candidate := range embedded.Models {
		if candidate.ModelID == "minimaxai/minimax-h3" {
			h3 = candidate
			break
		}
	}
	models, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{h3}})
	if err != nil || len(models) != 1 {
		t.Fatalf("convert embedded H3 model=%+v err=%v", h3, err)
	}
	if err := env.Store().ReconcileInferenceCatalog(models); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(h3.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	selected := []int{0, 1, 2, 3}
	body, _ := json.Marshal(map[string]any{
		"template_id": template.ID, "template_spec_name": "bf16-sglang-ascend-4x32g", "node_uuid": nodes[0].UUID,
		"served_name": "h3-placement", "placement": map[string]any{"gpus": selected},
	})
	api := env.API(t)
	previewResponse, err := api.PostJSON("/api/v1/evaluate", body)
	if err != nil {
		t.Fatal(err)
	}
	if previewResponse.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(previewResponse.Body)
		previewResponse.Body.Close()
		t.Fatalf("evaluate status/body=%d %s", previewResponse.StatusCode, raw)
	}
	var preview dto.EvaluateResponse
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		previewResponse.Body.Close()
		t.Fatal(err)
	}
	previewResponse.Body.Close()

	createdResponse, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if createdResponse.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(createdResponse.Body)
		createdResponse.Body.Close()
		t.Fatalf("create status/body=%d %s", createdResponse.StatusCode, raw)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		createdResponse.Body.Close()
		t.Fatal(err)
	}
	createdResponse.Body.Close()

	var persisted store.Instance
	env.Eventually(t, 30*time.Second, func() bool {
		instances, _ := env.Store().InstancesForDeployment(created.ID)
		if len(instances) != 1 {
			return false
		}
		persisted = instances[0]
		return persisted.DesiredStateJSON != ""
	})
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(persisted.DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	// The preview shows a placeholder port; the launch gets one allocated from
	// the harness's range. Pinning that range to the placeholder (30000) made
	// this test collide with every other harness claiming ports there.
	// The allocated port may itself be 30000: the harness's port grid starts
	// there. Only require that the launch carries a real port.
	argv, port := withPreviewPort(desired.Argv)
	if n, err := strconv.Atoi(port); err != nil || n <= 0 {
		t.Fatalf("persisted launch has no allocated --port: %v", desired.Argv)
	}
	if !reflect.DeepEqual(preview.Entrypoint, desired.Entrypoint) || !reflect.DeepEqual(preview.Argv, argv) || !reflect.DeepEqual(preview.Env, desired.Env) {
		t.Fatalf("preview and persisted launch diverged:\npreview entry=%v argv=%v env=%v\ndesired entry=%v argv=%v env=%v", preview.Entrypoint, preview.Argv, preview.Env, desired.Entrypoint, desired.Argv, desired.Env)
	}
	if len(desired.PreStartArgv) != 0 || desired.PlacementVariables["GPU_COUNT"] != float64(4) && desired.PlacementVariables["GPU_COUNT"] != int64(4) {
		t.Fatalf("resolved placement state=%+v pre_start=%v", desired.PlacementVariables, desired.PreStartArgv)
	}
	if !strings.Contains(string(desired.APIContract), `"endpoint":"/v1/videos"`) {
		t.Fatalf("resolved API contract=%s", desired.APIContract)
	}
}

// withPreviewPort returns argv with its --port value replaced by the preview
// placeholder, and the value it replaced.
func withPreviewPort(argv []string) ([]string, string) {
	out := append([]string(nil), argv...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--port" {
			port := out[i+1]
			out[i+1] = strconv.Itoa(planning.PreviewAPIPort)
			return out, port
		}
	}
	return out, ""
}

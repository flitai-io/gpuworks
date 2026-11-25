package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// acquisitionPreview is the wire shape the deploy form consumes. Decoding it
// by literal field names is the contract test: a rename on the server would
// silently empty the form's acquisition row otherwise.
type acquisitionPreview struct {
	Acquisition *struct {
		Providers []struct {
			Key         string `json:"key"`
			Label       string `json:"label"`
			Reason      string `json:"reason"`
			Recommended bool   `json:"recommended"`
		} `json:"providers"`
		Image struct {
			Image  string `json:"image"`
			Cached *bool  `json:"cached"`
		} `json:"image"`
		Nodes []struct {
			NodeUUID        string   `json:"node_uuid"`
			NodeName        string   `json:"node_name"`
			ImagePlan       string   `json:"image_plan"`
			ImageCopyFrom   string   `json:"image_copy_from"`
			ImagePullRefs   []string `json:"image_pull_refs"`
			ImagePullRanked bool     `json:"image_pull_ranked"`
			WeightsPresent  bool     `json:"weights_present"`
			WeightsPlan     string   `json:"weights_plan"`
			WeightsCopyFrom string   `json:"weights_copy_from"`
			WeightsProvider string   `json:"weights_provider"`
		} `json:"nodes"`
	} `json:"acquisition"`
}

func evaluateAcquisition(t *testing.T, env *testenv.Env, body map[string]any) acquisitionPreview {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := env.API(t).PostJSON("/api/v1/evaluate", encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("evaluate status = %d body=%s", response.StatusCode, raw)
	}
	var preview acquisitionPreview
	if err := json.NewDecoder(response.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if preview.Acquisition == nil {
		t.Fatal("evaluate returned no acquisition block")
	}
	return preview
}

// A node named by the request that already holds the weights is forecast to
// use them; nothing is fetched.
func TestEvaluateForecastsALocalCopy(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("acq-local", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "acq-local")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/acq-local", Revision: "main", Name: "acq-local",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The copy has to exist on disk. The agent reconciles every registered path
	// and reports one it cannot find as absent, which the server accepts for a
	// registered row; a real directory keeps the premise true.
	modelDir := filepath.Join(t.TempDir(), "acq-local")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: nodes[0].ID}, modelDir, 1<<28); err != nil {
		t.Fatal(err)
	}
	preview := evaluateAcquisition(t, env, map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if len(preview.Acquisition.Nodes) != 1 {
		t.Fatalf("forecast nodes = %+v, want the named node", preview.Acquisition.Nodes)
	}
	forecast := preview.Acquisition.Nodes[0]
	if forecast.NodeUUID != nodes[0].UUID || !forecast.WeightsPresent || forecast.WeightsPlan != "present" {
		t.Fatalf("forecast = %+v, want the local copy used", forecast)
	}
	// The provider says which weights run; it is offered either way.
	if len(preview.Acquisition.Providers) == 0 || !preview.Acquisition.Providers[0].Recommended {
		t.Fatalf("providers = %+v", preview.Acquisition.Providers)
	}
}

// With a copy on another online node of the same sync group, the named node
// is forecast to copy it from there rather than download it.
func TestEvaluateForecastsACopyFromASyncGroupPeer(t *testing.T) {
	env := testenv.New(t)
	for _, name := range []string{"acq-target", "acq-holder"} {
		env.AddNode(name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
		env.WaitNodeOnline(t, name)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/acq-peer", Revision: "main", Name: "acq-peer",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, holder := nodes[0], nodes[1]
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: holder.ID}, testenv.ModelDir(t, "acq-peer"), 1<<28); err != nil {
		t.Fatal(err)
	}
	preview := evaluateAcquisition(t, env, map[string]any{
		"model_id": model.ID, "node_uuid": target.UUID, "engine": "vllm",
	})
	if len(preview.Acquisition.Nodes) != 1 {
		t.Fatalf("forecast nodes = %+v", preview.Acquisition.Nodes)
	}
	forecast := preview.Acquisition.Nodes[0]
	if forecast.WeightsPresent || forecast.WeightsPlan != "copy" || forecast.WeightsCopyFrom != holder.DisplayName() {
		t.Fatalf("forecast = %+v, want a copy from %s", forecast, holder.DisplayName())
	}

	// Once the two nodes share no sync group, the node downloads instead.
	groups, err := env.Store().SyncGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %+v err=%v", groups, err)
	}
	if err := env.Store().DeleteSyncGroup(groups[0].ID); err != nil {
		t.Fatal(err)
	}
	preview = evaluateAcquisition(t, env, map[string]any{
		"model_id": model.ID, "node_uuid": target.UUID, "engine": "vllm",
	})
	if forecast := preview.Acquisition.Nodes[0]; forecast.WeightsPlan != "download" || forecast.WeightsProvider != "huggingface" {
		t.Fatalf("forecast without a shared group = %+v, want a download", forecast)
	}
}

// Without any copy, the named node is forecast to pull the image from its
// candidates and download the weights; an automatic placement gets no
// forecast, because where it lands is decided at launch.
func TestEvaluateForecastsAPullAndOnlyForNamedNodes(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("acq-public", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "acq-public")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/acq-public", Revision: "main", Name: "acq-public",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := evaluateAcquisition(t, env, map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if len(preview.Acquisition.Providers) == 0 || !preview.Acquisition.Providers[0].Recommended || preview.Acquisition.Providers[0].Label != "huggingface" {
		t.Fatalf("providers = %+v", preview.Acquisition.Providers)
	}
	if preview.Acquisition.Image.Image == "" || len(preview.Acquisition.Nodes) != 1 {
		t.Fatalf("acquisition = %+v", preview.Acquisition)
	}
	forecast := preview.Acquisition.Nodes[0]
	if forecast.ImagePlan != "pull" || len(forecast.ImagePullRefs) == 0 || forecast.ImagePullRefs[len(forecast.ImagePullRefs)-1] != preview.Acquisition.Image.Image {
		t.Fatalf("image forecast = %+v, want a pull ending at %s", forecast, preview.Acquisition.Image.Image)
	}
	if forecast.WeightsPlan != "download" || forecast.WeightsProvider != "huggingface" {
		t.Fatalf("weights forecast = %+v", forecast)
	}

	automatic := evaluateAcquisition(t, env, map[string]any{"model_id": model.ID, "engine": "vllm"})
	if len(automatic.Acquisition.Nodes) != 0 {
		t.Fatalf("an automatic placement got a forecast: %+v", automatic.Acquisition.Nodes)
	}
}

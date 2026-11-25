package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestRankScopedTemplateMountsEachRanksOwnSubfolder creates a two-node
// deployment of a spec whose weight subfolder is rank${NODE_RANK} (ADR 0076)
// through the REST API, the controller and two fake agents: each rank mounts
// its own artifact, and neither node is given the other rank's to download.
func TestRankScopedTemplateMountsEachRanksOwnSubfolder(t *testing.T) {
	env := testenv.New(t)
	leader := env.AddNode("rank-weights-0",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.43.0.10"))
	follower := env.AddNode("rank-weights-1",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress("10.43.0.11"))
	runs := []chan runtime.WorkloadSpec{make(chan runtime.WorkloadSpec, 1), make(chan runtime.WorkloadSpec, 1)}
	leader.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(runs[0], spec) })
	follower.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(runs[1], spec) })
	env.WaitNodeOnline(t, leader.Name)
	env.WaitNodeOnline(t, follower.Name)

	const repo = "gpuworks/ranked-fake"
	var models []*store.Model
	var dirs []string
	var rows []*store.Node
	for rank, node := range []*testenv.Node{leader, follower} {
		model, err := env.Store().UpsertModel(&store.Model{
			Source: "modelscope", SourceID: repo, Revision: "main", ArtifactSubfolder: fmt.Sprintf("rank%d", rank),
			Name: repo, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
			Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		dir := testenv.ModelDir(t, fmt.Sprintf("ranked-fake-rank%d", rank))
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: dir}); err != nil {
			t.Fatal(err)
		}
		models, dirs, rows = append(models, model), append(dirs, dir), append(rows, row)
	}
	catalog := store.InferenceModel{ModelID: repo, Source: "upstream", Architecture: "Qwen3ForCausalLM", ContextLength: 4096, Specs: []store.InferenceSpecRow{{
		Name: "tp2-ranked", Engine: "vllm", Image: "gpuworks/fake-engine:test", Quantization: "bf16", WeightBytes: 1 << 20,
		WeightSources: `[{"provider":"modelscope","repo_id":"` + repo + `","revision":"main","subfolder":"rank${NODE_RANK}"}]`,
		GPUs:          `{"vendor":"nvidia"}`, Args: `["--max-model-len","512"]`,
	}}}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}

	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"template_id": template.ID, "template_spec_name": "tp2-ranked", "node_uuid": leader.UUID,
		"node_uuids": []string{leader.UUID, follower.UUID}, "served_name": "ranked-fake",
		"nnodes": 2, "master_addr": "10.43.0.10", "master_port": 29501,
		"placement": map[string]any{"tp": 1, "pp": 2}, "quick_params": map[string]any{},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	dep, err := env.Store().GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.ModelID != models[0].ID {
		t.Fatalf("deployment model = %d, want rank 0's artifact %d", dep.ModelID, models[0].ID)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	if selection.WeightSubfolder != "rank${NODE_RANK}" || len(selection.RankWeights) != 2 || selection.RankWeights[1].StoreModelID != models[1].ID {
		t.Fatalf("snapshot rank weights = %q %+v", selection.WeightSubfolder, selection.RankWeights)
	}

	for rank := range 2 {
		spec := waitCapturedWorkload(t, runs[rank], fmt.Sprintf("rank %d", rank))
		// Its own weights, and the group's peers file beside them.
		if len(spec.Mounts) != 2 || spec.Mounts[0].Source != dirs[rank] || spec.Mounts[0].Target != "/model" || spec.Mounts[1].Target != proto.PeersFileMountPath {
			t.Fatalf("rank %d mounts %+v, want its own %s and the peers file", rank, spec.Mounts, dirs[rank])
		}
	}
	for rank, row := range rows {
		other := models[1-rank]
		if _, err := env.Store().ModelFileAt(store.FileRef{ModelID: other.ID, NodeID: row.ID}); err == nil {
			t.Fatalf("rank %d's node was given rank %d's artifact", rank, 1-rank)
		}
	}
}
